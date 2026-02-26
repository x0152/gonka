package public

import (
	"bytes"
	"decentralized-api/utils"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"decentralized-api/internal/teecrypto"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

type e2eEncryptedResponse struct {
	SessionID  string `json:"session_id"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
	AAD        string `json:"aad"`
}

func TestTEEFlow_EncryptedRelayThroughDapi(t *testing.T) {
	nodePriv, err := teecrypto.GenerateX25519PrivateKey()
	require.NoError(t, err)
	nodePubB64 := teecrypto.PublicKeyBase64(nodePriv.PublicKey())

	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/tee/chat/completions", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)

		var req teeEncryptedInferenceRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))

		peerPub, err := teecrypto.ParseX25519PublicKeyBase64(req.EphemeralPublicKey)
		require.NoError(t, err)

		requestKey, responseKey, err := teecrypto.DeriveRequestAndResponseKeys(nodePriv, peerPub, req.SessionID)
		require.NoError(t, err)

		plainReq, err := teecrypto.DecryptAESGCMBase64(requestKey, req.Nonce, req.Ciphertext, req.AAD)
		require.NoError(t, err)
		require.Contains(t, string(plainReq), "hello tee")

		plainResp := []byte(`{"id":"resp-1","choices":[{"message":{"role":"assistant","content":"secure answer"}}],"usage":{"total_tokens":16}}`)
		nonceB64, ciphertextB64, err := teecrypto.EncryptAESGCMBase64(responseKey, plainResp, req.AAD)
		require.NoError(t, err)

		_ = json.NewEncoder(w).Encode(e2eEncryptedResponse{
			SessionID:  req.SessionID,
			Nonce:      nonceB64,
			Ciphertext: ciphertextB64,
			AAD:        req.AAD,
		})
	}))
	defer node.Close()

	executor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/tee/executor/chat/completions", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "sess-1-inference", r.Header.Get(utils.XInferenceIdHeader))
		require.Equal(t, "prompt-hash", r.Header.Get(utils.XPromptHashHeader))

		nodeURL := r.Header.Get(teeHeaderNodeURL)
		require.NotEmpty(t, nodeURL)
		relayURL, err := url.JoinPath(nodeURL, teeNodeInferPath)
		require.NoError(t, err)

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		nodeReq, err := http.NewRequest(http.MethodPost, relayURL, bytes.NewReader(body))
		require.NoError(t, err)
		nodeReq.Header.Set("Content-Type", "application/json")
		nodeResp, err := http.DefaultClient.Do(nodeReq)
		require.NoError(t, err)
		defer nodeResp.Body.Close()

		rawResp, err := io.ReadAll(nodeResp.Body)
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(nodeResp.StatusCode)
		_, _ = w.Write(rawResp)
	}))
	defer executor.Close()

	sessions := newTEESessionStore()
	sessions.put(teeSession{
		SessionID:          "sess-1",
		NodeURL:            node.URL,
		NodePublicKey:      nodePubB64,
		ExecutorURL:        executor.URL,
		InferenceID:        "sess-1-inference",
		RequestTimestamp:   12345,
		RequesterAddress:   "requester-1",
		TransferAddress:    "transfer-1",
		TransferSignature:  "ta-signature",
		PromptHash:         "prompt-hash",
		OriginalPromptHash: "prompt-hash",
		Started:            true,
		ExpiresAt:          time.Now().Add(10 * time.Minute).Unix(),
	})

	srv := &Server{
		httpClient:  &http.Client{},
		teeSessions: sessions,
	}

	e := echo.New()
	e.POST("/v1/tee/chat/completions", srv.postTeeChatCompletions)
	dapi := httptest.NewServer(e)
	defer dapi.Close()

	ephemeralPriv, err := teecrypto.GenerateX25519PrivateKey()
	require.NoError(t, err)
	nodePub, err := teecrypto.ParseX25519PublicKeyBase64(nodePubB64)
	require.NoError(t, err)

	requestKey, responseKey, err := teecrypto.DeriveRequestAndResponseKeys(ephemeralPriv, nodePub, "sess-1")
	require.NoError(t, err)

	plainOpenAIReq := []byte(`{"model":"Qwen/Qwen2.5-7B-Instruct","messages":[{"role":"user","content":"hello tee"}]}`)
	aad := "gonka-tee:sess-1"
	nonceB64, ciphertextB64, err := teecrypto.EncryptAESGCMBase64(requestKey, plainOpenAIReq, aad)
	require.NoError(t, err)

	relayReq := map[string]interface{}{
		"session_id":           "sess-1",
		"ephemeral_public_key": teecrypto.PublicKeyBase64(ephemeralPriv.PublicKey()),
		"nonce":                nonceB64,
		"ciphertext":           ciphertextB64,
		"aad":                  aad,
	}
	rawRelayReq, err := json.Marshal(relayReq)
	require.NoError(t, err)

	relayResp := doJSONPostRequest(t, dapi.URL+"/v1/tee/chat/completions", rawRelayReq)
	var encResp e2eEncryptedResponse
	require.NoError(t, json.Unmarshal(relayResp, &encResp))

	plainResp, err := teecrypto.DecryptAESGCMBase64(responseKey, encResp.Nonce, encResp.Ciphertext, encResp.AAD)
	require.NoError(t, err)
	require.Contains(t, string(plainResp), "secure answer")
}

func doJSONPostRequest(t *testing.T, endpoint string, body []byte) []byte {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Less(t, resp.StatusCode, 400)

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return raw
}
