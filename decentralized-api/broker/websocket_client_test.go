package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"decentralized-api/cosmosclient"
	"decentralized-api/mlnodeclient"

	"github.com/gorilla/websocket"
	"github.com/productscience/inference/api/inference/inference"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func TestConvertHTTPToWSURL(t *testing.T) {
	tests := []struct {
		name     string
		httpURL  string
		expected string
	}{
		{
			name:     "http to ws",
			httpURL:  "http://localhost:8080",
			expected: "ws://localhost:8080",
		},
		{
			name:     "https to wss",
			httpURL:  "https://localhost:8080",
			expected: "wss://localhost:8080",
		},
		{
			name:     "no protocol",
			httpURL:  "localhost:8080",
			expected: "ws://localhost:8080",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertHTTPToWSURL(tt.httpURL)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestReconnectInterval(t *testing.T) {
	for i := 0; i < 10; i++ {
		interval := reconnectInterval()
		assert.GreaterOrEqual(t, interval, 3*time.Second)
		assert.LessOrEqual(t, interval, 5*time.Second)
	}
}

func TestWebSocketClient_URLConstruction(t *testing.T) {
	client := NewWebSocketClient("test-node", "http://localhost:5000/api/v1", &cosmosclient.MockCosmosMessageClient{})
	assert.Equal(t, "ws://localhost:5000/api/v1/pow/ws", client.wsURL)
}

func TestWebSocketClient_PingLoop(t *testing.T) {
	prevWriteWait, prevPongWait, prevPingPeriod := writeWait, pongWait, pingPeriod
	writeWait = 50 * time.Millisecond
	pongWait = 100 * time.Millisecond
	pingPeriod = 20 * time.Millisecond
	defer func() {
		writeWait = prevWriteWait
		pongWait = prevPongWait
		pingPeriod = prevPingPeriod
	}()

	pingReceived := make(chan struct{}, 1)
	errChan := make(chan error, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			errChan <- err
			return
		}
		defer conn.Close()

		conn.SetPingHandler(func(string) error {
			select {
			case pingReceived <- struct{}{}:
			default:
			}
			return conn.WriteControl(websocket.PongMessage, nil, time.Now().Add(writeWait))
		})

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if !assert.NoError(t, err) {
		return
	}
	defer conn.Close()

	client := &WebSocketClient{
		stopChan: make(chan struct{}),
		ctx:      context.Background(),
	}
	done := make(chan struct{})
	go client.pingLoop(conn, done)

	select {
	case <-pingReceived:
	case err := <-errChan:
		t.Fatalf("server error: %v", err)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected ping not received")
	}

	close(done)
}

func TestWebSocketClient_PongHandlerKeepsAlive(t *testing.T) {
	prevWriteWait, prevPongWait, prevPingPeriod := writeWait, pongWait, pingPeriod
	writeWait = 50 * time.Millisecond
	pongWait = 200 * time.Millisecond
	pingPeriod = 50 * time.Millisecond
	defer func() {
		writeWait = prevWriteWait
		pongWait = prevPongWait
		pingPeriod = prevPingPeriod
	}()

	errChan := make(chan error, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			errChan <- err
			return
		}
		defer conn.Close()

		conn.SetPingHandler(func(appData string) error {
			return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(writeWait))
		})

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if !assert.NoError(t, err) {
		return
	}
	defer conn.Close()

	client := &WebSocketClient{
		conn:     conn,
		handler:  NewBatchHandler(&cosmosclient.MockCosmosMessageClient{}),
		stopChan: make(chan struct{}),
		ctx:      context.Background(),
	}

	done := make(chan struct{})
	go func() {
		client.handleMessages()
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("connection closed before pong wait elapsed")
	case err := <-errChan:
		t.Fatalf("server error: %v", err)
	case <-time.After(250 * time.Millisecond):
	}

	close(client.stopChan)

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected handleMessages to stop")
	}
}

func TestWebSocketClient_MessageProcessing(t *testing.T) {
	mockRecorder := &cosmosclient.MockCosmosMessageClient{}
	mockRecorder.On("SubmitPocBatch", mock.AnythingOfType("*inference.MsgSubmitPocBatch")).Return(nil)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("Failed to upgrade: %v", err)
		}
		defer conn.Close()

		generatedBatch := mlnodeclient.ProofBatch{
			BlockHeight: 100,
			NodeNum:     1,
			Nonces:      []int64{1, 2, 3},
			Dist:        []float64{0.1, 0.2, 0.3},
		}
		batchJSON, _ := json.Marshal(generatedBatch)

		message := WebSocketMessage{
			Type:  "generated",
			Batch: batchJSON,
			ID:    "test-message-123",
		}

		if err := conn.WriteJSON(message); err != nil {
			t.Logf("Failed to write message: %v", err)
			return
		}

		var ack AckMessage
		if err := conn.ReadJSON(&ack); err != nil {
			t.Logf("Failed to read ack: %v", err)
			return
		}

		assert.Equal(t, "ack", ack.Type)
		assert.Equal(t, "test-message-123", ack.ID)

		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	client := &WebSocketClient{
		nodeID:      "test-node",
		wsURL:       wsURL,
		handler:     NewBatchHandler(mockRecorder),
		stopChan:    make(chan struct{}),
		stoppedChan: make(chan struct{}),
	}
	client.ctx, client.cancel = context.WithCancel(context.Background())

	go client.run()

	time.Sleep(500 * time.Millisecond)

	client.Stop()

	mockRecorder.AssertExpectations(t)
}

func TestWebSocketClient_StartStop(t *testing.T) {
	mockRecorder := &cosmosclient.MockCosmosMessageClient{}

	client := NewWebSocketClient("test-node", "http://invalid-url:9999", mockRecorder)

	client.Start()

	time.Sleep(100 * time.Millisecond)

	client.Stop()

	select {
	case <-client.stoppedChan:
	case <-time.After(2 * time.Second):
		t.Fatal("Client did not stop within timeout")
	}
}

func TestWebSocketClient_AckMessage(t *testing.T) {
	ack := AckMessage{
		Type: "ack",
		ID:   "test-123",
	}

	data, err := json.Marshal(ack)
	assert.NoError(t, err)

	var decoded AckMessage
	err = json.Unmarshal(data, &decoded)
	assert.NoError(t, err)
	assert.Equal(t, "ack", decoded.Type)
	assert.Equal(t, "test-123", decoded.ID)
}

func TestWebSocketClient_ProcessMessage_Generated(t *testing.T) {
	mockRecorder := &cosmosclient.MockCosmosMessageClient{}
	mockRecorder.On("SubmitPocBatch", mock.MatchedBy(func(msg *inference.MsgSubmitPocBatch) bool {
		return msg.PocStageStartBlockHeight == 100 && msg.NodeId == "test-node"
	})).Return(nil)

	handler := NewBatchHandler(mockRecorder)
	client := &WebSocketClient{
		nodeID:  "test-node",
		handler: handler,
	}

	batch := mlnodeclient.ProofBatch{
		BlockHeight: 100,
		NodeNum:     1,
		Nonces:      []int64{1, 2, 3},
		Dist:        []float64{0.1, 0.2, 0.3},
	}
	batchJSON, _ := json.Marshal(batch)

	message := WebSocketMessage{
		Type:  "generated",
		Batch: batchJSON,
		ID:    "test-msg-123",
	}
	messageJSON, _ := json.Marshal(message)

	messageID, err := client.processMessage(messageJSON)
	assert.NoError(t, err)
	assert.Equal(t, "test-msg-123", messageID)

	mockRecorder.AssertExpectations(t)
}

func TestWebSocketClient_ProcessMessage_Validated(t *testing.T) {
	validPubKey := "02a1633cafcc01ebfb6d78e39f687a1f0995c62fc95f51ead10a02ee0be551b5dc"

	mockRecorder := &cosmosclient.MockCosmosMessageClient{}
	mockRecorder.On("SubmitPoCValidation", mock.AnythingOfType("*inference.MsgSubmitPocValidation")).Return(nil)

	handler := NewBatchHandler(mockRecorder)
	client := &WebSocketClient{
		nodeID:  "test-node",
		handler: handler,
	}

	batch := mlnodeclient.ValidatedBatch{
		ProofBatch: mlnodeclient.ProofBatch{
			BlockHeight: 100,
			PublicKey:   validPubKey,
			Nonces:      []int64{1, 2, 3},
			Dist:        []float64{0.1, 0.2, 0.3},
		},
		ReceivedDist:      []float64{0.1, 0.2, 0.3},
		RTarget:           0.5,
		FraudThreshold:    0.1,
		NInvalid:          0,
		ProbabilityHonest: 1.0,
		FraudDetected:     false,
	}
	batchJSON, _ := json.Marshal(batch)

	message := WebSocketMessage{
		Type:  "validated",
		Batch: batchJSON,
		ID:    "test-msg-456",
	}
	messageJSON, _ := json.Marshal(message)

	messageID, err := client.processMessage(messageJSON)
	assert.NoError(t, err)
	assert.Equal(t, "test-msg-456", messageID)

	mockRecorder.AssertExpectations(t)
}

func TestWebSocketClient_ProcessMessage_UnknownType(t *testing.T) {
	mockRecorder := &cosmosclient.MockCosmosMessageClient{}
	handler := NewBatchHandler(mockRecorder)
	client := &WebSocketClient{
		nodeID:  "test-node",
		handler: handler,
	}

	message := WebSocketMessage{
		Type:  "unknown",
		Batch: json.RawMessage(`{}`),
		ID:    "test-msg-789",
	}
	messageJSON, _ := json.Marshal(message)

	messageID, err := client.processMessage(messageJSON)
	assert.Error(t, err)
	assert.Equal(t, "", messageID)
	assert.Contains(t, err.Error(), "unknown message type")
}
