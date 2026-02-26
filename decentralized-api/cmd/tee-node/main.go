package main

import (
	"bytes"
	"crypto/ecdh"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"decentralized-api/internal/teecrypto"
)

type encryptedInferenceRequest struct {
	SessionID          string `json:"session_id"`
	EphemeralPublicKey string `json:"ephemeral_public_key"`
	Nonce              string `json:"nonce"`
	Ciphertext         string `json:"ciphertext"`
	AAD                string `json:"aad,omitempty"`
}

type encryptedInferenceResponse struct {
	SessionID  string                 `json:"session_id"`
	Nonce      string                 `json:"nonce"`
	Ciphertext string                 `json:"ciphertext"`
	AAD        string                 `json:"aad,omitempty"`
	Metadata   map[string]interface{} `json:"metadata,omitempty"`
}

type nodeRuntime struct {
	nodeID      string
	model       string
	upstreamURL string
	dryRun      bool
	priv        *ecdh.PrivateKey
	pubB64      string
	client      *http.Client
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIRequest struct {
	Model     string          `json:"model"`
	Messages  []openAIMessage `json:"messages"`
	MaxTokens int             `json:"max_tokens,omitempty"`
}

func main() {
	listenAddr := flag.String("listen", ":18180", "listen address")
	nodeID := flag.String("node-id", "mlnode-tee-1", "logical node id")
	model := flag.String("model", "Qwen/Qwen2.5-7B-Instruct", "model name")
	upstreamURL := flag.String("upstream-url", "http://127.0.0.1:8000/v1/chat/completions", "vLLM/OpenAI-compatible endpoint")
	dryRun := flag.Bool("dry-run", false, "if true, returns synthetic response and does not call upstream")
	privateKeyB64 := flag.String("private-key-b64", "", "optional base64-encoded x25519 private key")
	flag.Parse()

	var privKey *ecdh.PrivateKey
	var err error
	if strings.TrimSpace(*privateKeyB64) != "" {
		privKey, err = teecrypto.ParseX25519PrivateKeyBase64(*privateKeyB64)
		if err != nil {
			log.Fatalf("failed to parse private key: %v", err)
		}
	} else {
		privKey, err = teecrypto.GenerateX25519PrivateKey()
		if err != nil {
			log.Fatalf("failed to generate private key: %v", err)
		}
	}

	runtime := &nodeRuntime{
		nodeID:      *nodeID,
		model:       *model,
		upstreamURL: *upstreamURL,
		dryRun:      *dryRun,
		priv:        privKey,
		pubB64:      teecrypto.PublicKeyBase64(privKey.PublicKey()),
		client: &http.Client{
			Timeout: 2 * time.Minute,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", runtime.health)
	mux.HandleFunc("/v1/tee/info", runtime.info)
	mux.HandleFunc("/v1/tee/chat/completions", runtime.infer)

	log.Printf("tee-node started: listen=%s node_id=%s model=%s dry_run=%v", *listenAddr, runtime.nodeID, runtime.model, runtime.dryRun)
	log.Printf("node_public_key=%s", runtime.pubB64)
	log.Printf("register hint: submit MsgSubmitHardwareDiff on chain with hardware.type=TEE, local_id=%q, model=%q, host/port of this adapter, and participant worker_key=%q", runtime.nodeID, runtime.model, runtime.pubB64)

	if err := http.ListenAndServe(*listenAddr, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func (n *nodeRuntime) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"node_id": n.nodeID,
		"model":   n.model,
	})
}

func (n *nodeRuntime) info(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":              true,
		"node_id":         n.nodeID,
		"model":           n.model,
		"node_public_key": n.pubB64,
		"attestation":     "stub-attestation",
	})
}

func (n *nodeRuntime) infer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"ok": false, "error": "method not allowed"})
		return
	}

	var req encryptedInferenceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "invalid json body"})
		return
	}
	if req.SessionID == "" || req.EphemeralPublicKey == "" || req.Nonce == "" || req.Ciphertext == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "session_id, ephemeral_public_key, nonce and ciphertext are required"})
		return
	}

	peerPub, err := teecrypto.ParseX25519PublicKeyBase64(req.EphemeralPublicKey)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "invalid ephemeral public key"})
		return
	}

	requestKey, responseKey, err := teecrypto.DeriveRequestAndResponseKeys(n.priv, peerPub, req.SessionID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "failed to derive session keys"})
		return
	}

	plainReq, err := teecrypto.DecryptAESGCMBase64(requestKey, req.Nonce, req.Ciphertext, req.AAD)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "failed to decrypt request"})
		return
	}

	plainResp, usedTokens, err := n.executeInference(plainReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}

	nonceB64, ciphertextB64, err := teecrypto.EncryptAESGCMBase64(responseKey, plainResp, req.AAD)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"ok": false, "error": "failed to encrypt response"})
		return
	}

	writeJSON(w, http.StatusOK, encryptedInferenceResponse{
		SessionID:  req.SessionID,
		Nonce:      nonceB64,
		Ciphertext: ciphertextB64,
		AAD:        req.AAD,
		Metadata: map[string]interface{}{
			"used_tokens": usedTokens,
		},
	})
}

func (n *nodeRuntime) executeInference(plainReq []byte) ([]byte, int, error) {
	if n.dryRun {
		var req openAIRequest
		_ = json.Unmarshal(plainReq, &req)

		content := "TEE dry-run response"
		if len(req.Messages) > 0 && req.Messages[len(req.Messages)-1].Content != "" {
			content = "TEE echo: " + req.Messages[len(req.Messages)-1].Content
		}

		resp := map[string]interface{}{
			"id":      fmt.Sprintf("tee-%d", time.Now().UnixNano()),
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   req.Model,
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": content,
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]interface{}{
				"prompt_tokens":     8,
				"completion_tokens": 8,
				"total_tokens":      16,
			},
		}
		body, err := json.Marshal(resp)
		return body, 16, err
	}

	req, err := http.NewRequest(http.MethodPost, n.upstreamURL, bytes.NewReader(plainReq))
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create upstream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to read upstream response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, 0, fmt.Errorf("upstream returned status %d: %s", resp.StatusCode, string(body))
	}

	usedTokens := extractTotalTokens(body)
	return body, usedTokens, nil
}

func extractTotalTokens(raw []byte) int {
	var parsed struct {
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return 0
	}
	return parsed.Usage.TotalTokens
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	body, err := json.Marshal(payload)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false,"error":"failed to marshal response"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
