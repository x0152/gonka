package broker

import (
	"context"
	"decentralized-api/cosmosclient"
	"decentralized-api/logging"
	"decentralized-api/mlnodeclient"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/productscience/inference/x/inference/types"
)

type WebSocketMessage struct {
	Type  string          `json:"type"`
	Batch json.RawMessage `json:"batch"`
	ID    string          `json:"id"`
}

type AckMessage struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type WebSocketClient struct {
	nodeID      string
	wsURL       string
	conn        *websocket.Conn
	mu          sync.RWMutex
	handler     *BatchHandler
	stopChan    chan struct{}
	stoppedChan chan struct{}
	ctx         context.Context
	cancel      context.CancelFunc
	stopOnce    sync.Once
}

var (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10
)

func NewWebSocketClient(nodeID string, pocURL string, recorder cosmosclient.CosmosMessageClient) *WebSocketClient {
	ctx, cancel := context.WithCancel(context.Background())
	wsURL, _ := url.JoinPath(convertHTTPToWSURL(pocURL), "pow/ws")

	return &WebSocketClient{
		nodeID:      nodeID,
		wsURL:       wsURL,
		handler:     NewBatchHandler(recorder),
		stopChan:    make(chan struct{}),
		stoppedChan: make(chan struct{}),
		ctx:         ctx,
		cancel:      cancel,
	}
}

func (c *WebSocketClient) Start() {
	go c.run()
}

func (c *WebSocketClient) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopChan)
		c.cancel()
	})
	<-c.stoppedChan
}

func (c *WebSocketClient) run() {
	defer close(c.stoppedChan)

	for {
		select {
		case <-c.stopChan:
			c.closeConnection()
			return
		case <-c.ctx.Done():
			c.closeConnection()
			return
		default:
			c.connectAndHandle()

			select {
			case <-c.stopChan:
				return
			case <-c.ctx.Done():
				return
			case <-time.After(reconnectInterval()):
			}
		}
	}
}

func (c *WebSocketClient) connectAndHandle() {
	if err := c.connect(); err != nil {
		logging.Debug("WebSocket. Failed to connect", types.PoC,
			"nodeId", c.nodeID, "error", err)
		return
	}

	logging.Info("WebSocket. Connected to node", types.PoC,
		"nodeId", c.nodeID, "wsURL", c.wsURL)

	c.handleMessages()
}

func (c *WebSocketClient) connect() error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.Dial(c.wsURL, nil)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	return nil
}

func (c *WebSocketClient) closeConnection() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
		logging.Debug("WebSocket. Closed connection", types.PoC, "nodeId", c.nodeID)
	}
}

func (c *WebSocketClient) handleMessages() {
	defer c.closeConnection()

	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()

	if conn == nil {
		return
	}

	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	done := make(chan struct{})
	defer close(done)
	go c.pingLoop(conn, done)

	for {
		select {
		case <-c.stopChan:
			return
		case <-c.ctx.Done():
			return
		default:
			_, rawMessage, err := conn.ReadMessage()
			if err != nil {
				logging.Debug("WebSocket. Read error, will reconnect", types.PoC,
					"nodeId", c.nodeID, "error", err)
				return
			}

			messageID, err := c.processMessage(rawMessage)
			if err != nil {
				logging.Error("WebSocket. Failed to process message", types.PoC,
					"nodeId", c.nodeID, "error", err)
				continue
			}

			if messageID != "" {
				if err := c.sendAck(messageID); err != nil {
					logging.Error("WebSocket. Failed to send acknowledgment", types.PoC,
						"nodeId", c.nodeID, "messageId", messageID, "error", err)
					return
				}

				logging.Debug("WebSocket. Sent acknowledgment", types.PoC,
					"nodeId", c.nodeID, "messageId", messageID)
			}
		}
	}
}

func (c *WebSocketClient) pingLoop(conn *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-c.stopChan:
			return
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *WebSocketClient) processMessage(rawMessage []byte) (string, error) {
	var msg WebSocketMessage
	if err := json.Unmarshal(rawMessage, &msg); err != nil {
		return "", fmt.Errorf("failed to unmarshal message: %w", err)
	}

	switch msg.Type {
	case "generated":
		var batch mlnodeclient.ProofBatch
		if err := json.Unmarshal(msg.Batch, &batch); err != nil {
			return "", fmt.Errorf("failed to unmarshal generated batch: %w", err)
		}
		if err := c.handler.HandleGeneratedBatch(c.nodeID, batch); err != nil {
			return "", fmt.Errorf("failed to handle generated batch: %w", err)
		}
		return msg.ID, nil

	case "validated":
		var batch mlnodeclient.ValidatedBatch
		if err := json.Unmarshal(msg.Batch, &batch); err != nil {
			return "", fmt.Errorf("failed to unmarshal validated batch: %w", err)
		}
		if err := c.handler.HandleValidatedBatch(batch); err != nil {
			return "", fmt.Errorf("failed to handle validated batch: %w", err)
		}
		return msg.ID, nil

	default:
		return "", fmt.Errorf("unknown message type: %s", msg.Type)
	}
}

func (c *WebSocketClient) sendAck(messageID string) error {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("connection is nil")
	}

	ack := AckMessage{
		Type: "ack",
		ID:   messageID,
	}

	ackData, err := json.Marshal(ack)
	if err != nil {
		return fmt.Errorf("failed to marshal ack: %w", err)
	}

	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, ackData); err != nil {
		return fmt.Errorf("failed to write ack: %w", err)
	}

	return nil
}

func convertHTTPToWSURL(httpURL string) string {
	if len(httpURL) >= 7 && httpURL[:7] == "http://" {
		return "ws://" + httpURL[7:]
	}
	if len(httpURL) >= 8 && httpURL[:8] == "https://" {
		return "wss://" + httpURL[8:]
	}
	return fmt.Sprintf("ws://%s", httpURL)
}

func reconnectInterval() time.Duration {
	base := 3 * time.Second
	jitter := time.Duration(rand.Intn(2000)) * time.Millisecond
	return base + jitter
}
