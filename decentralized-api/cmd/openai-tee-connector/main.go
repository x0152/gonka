package main

import (
	"bytes"
	"decentralized-api/utils"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"decentralized-api/internal/teecrypto"
)

type reserveRequest struct {
	UserID           string `json:"user_id,omitempty"`
	RequesterAddress string `json:"requester_address,omitempty"`
	Model            string `json:"model"`
	MaxTokens        int    `json:"max_tokens"`
	ExecutorID       string `json:"executor_id,omitempty"`
}

type reserveResponse struct {
	Ok      bool `json:"ok"`
	Session struct {
		SessionID string `json:"session_id"`
		NodeID    string `json:"node_id"`
		NodeURL   string `json:"node_url"`
	} `json:"session"`
	Assignment struct {
		ExecutorID    string `json:"executor_id"`
		ExecutorURL   string `json:"executor_url"`
		NodeID        string `json:"node_id"`
		NodeURL       string `json:"node_url"`
		NodePublicKey string `json:"node_public_key"`
		Attestation   string `json:"attestation"`
	} `json:"assignment"`
	Error string `json:"error"`
}

type encryptedInferenceRequest struct {
	SessionID          string `json:"session_id"`
	EphemeralPublicKey string `json:"ephemeral_public_key"`
	Nonce              string `json:"nonce"`
	Ciphertext         string `json:"ciphertext"`
	AAD                string `json:"aad,omitempty"`
	RequesterAddress   string `json:"requester_address,omitempty"`
}

type encryptedInferenceResponse struct {
	SessionID  string                 `json:"session_id"`
	Nonce      string                 `json:"nonce"`
	Ciphertext string                 `json:"ciphertext"`
	AAD        string                 `json:"aad,omitempty"`
	Metadata   map[string]interface{} `json:"metadata,omitempty"`
}

type closeSessionRequest struct {
	SessionID string `json:"session_id"`
}

type openAIRequest struct {
	Model     string          `json:"model"`
	Messages  []openAIMessage `json:"messages"`
	MaxTokens int             `json:"max_tokens,omitempty"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func main() {
	dapiURL := flag.String("dapi-url", "http://127.0.0.1:8080", "dAPI public URL")
	userID := flag.String("user-id", "", "optional legacy user id; do not set for chain-mode")
	requesterAddress := flag.String("requester-address", "", "optional chain requester address; defaults to transfer dAPI address")
	executorID := flag.String("executor-id", "", "optional pinned executor id: <participant_address> or <participant_address>/<local_node_id>")
	model := flag.String("model", "Qwen/Qwen2.5-7B-Instruct", "model")
	maxTokens := flag.Int("max-tokens", 256, "max_tokens reservation and request")
	message := flag.String("message", "Hello from confidential connector", "user message")
	closeSession := flag.Bool("close-session", true, "close session after response")
	flag.Parse()

	client := &http.Client{Timeout: 60 * time.Second}
	baseURL := strings.TrimRight(*dapiURL, "/")

	reserveResp, err := reserve(client, baseURL, reserveRequest{
		UserID:           *userID,
		RequesterAddress: *requesterAddress,
		Model:            *model,
		MaxTokens:        *maxTokens,
		ExecutorID:       *executorID,
	})
	if err != nil {
		log.Fatalf("reserve failed: %v", err)
	}
	log.Printf(
		"reserved session=%s executor=%s node=%s",
		reserveResp.Session.SessionID,
		firstNonEmpty(reserveResp.Assignment.ExecutorID, reserveResp.Assignment.ExecutorURL),
		reserveResp.Assignment.NodeID,
	)

	if reserveResp.Assignment.NodePublicKey == "" {
		log.Fatalf("reserve response missing node_public_key")
	}

	ephemeralPriv, err := teecrypto.GenerateX25519PrivateKey()
	if err != nil {
		log.Fatalf("failed to generate ephemeral key: %v", err)
	}
	nodePub, err := teecrypto.ParseX25519PublicKeyBase64(reserveResp.Assignment.NodePublicKey)
	if err != nil {
		log.Fatalf("invalid node_public_key: %v", err)
	}
	requestKey, responseKey, err := teecrypto.DeriveRequestAndResponseKeys(ephemeralPriv, nodePub, reserveResp.Session.SessionID)
	if err != nil {
		log.Fatalf("failed to derive shared keys: %v", err)
	}

	openAIReqBody, err := json.Marshal(openAIRequest{
		Model: *model,
		Messages: []openAIMessage{
			{Role: "user", Content: *message},
		},
		MaxTokens: *maxTokens,
	})
	if err != nil {
		log.Fatalf("failed to marshal openai request: %v", err)
	}

	aad := "gonka-tee:" + reserveResp.Session.SessionID
	nonceB64, ciphertextB64, err := teecrypto.EncryptAESGCMBase64(requestKey, openAIReqBody, aad)
	if err != nil {
		log.Fatalf("failed to encrypt request: %v", err)
	}

	encResp, inferenceID, err := infer(client, baseURL, encryptedInferenceRequest{
		SessionID:          reserveResp.Session.SessionID,
		EphemeralPublicKey: teecrypto.PublicKeyBase64(ephemeralPriv.PublicKey()),
		Nonce:              nonceB64,
		Ciphertext:         ciphertextB64,
		AAD:                aad,
		RequesterAddress:   strings.TrimSpace(*requesterAddress),
	})
	if err != nil {
		log.Fatalf("encrypted inference failed: %v", err)
	}
	if strings.TrimSpace(inferenceID) != "" {
		log.Printf("inference_id=%s", inferenceID)
	}

	plainResp, err := teecrypto.DecryptAESGCMBase64(responseKey, encResp.Nonce, encResp.Ciphertext, encResp.AAD)
	if err != nil {
		log.Fatalf("failed to decrypt response: %v", err)
	}

	var pretty bytes.Buffer
	if err := json.Indent(&pretty, plainResp, "", "  "); err == nil {
		fmt.Println(pretty.String())
	} else {
		fmt.Println(string(plainResp))
	}

	if *closeSession {
		if err := close(client, baseURL, reserveResp.Session.SessionID); err != nil {
			log.Fatalf("close session failed: %v", err)
		}
		log.Printf("session closed: %s", reserveResp.Session.SessionID)
	}
}

func reserve(client *http.Client, baseURL string, req reserveRequest) (*reserveResponse, error) {
	endpoint, err := url.JoinPath(baseURL, "/v1/tee/sessions/reserve")
	if err != nil {
		return nil, err
	}
	rawReq, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(rawReq))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}
	if httpResp.StatusCode >= 400 {
		return nil, fmt.Errorf("status=%d body=%s", httpResp.StatusCode, string(body))
	}

	var parsed reserveResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	if !parsed.Ok {
		return nil, fmt.Errorf("reserve error: %s", parsed.Error)
	}
	return &parsed, nil
}

func infer(client *http.Client, baseURL string, req encryptedInferenceRequest) (*encryptedInferenceResponse, string, error) {
	endpoint, err := url.JoinPath(baseURL, "/v1/tee/chat/completions")
	if err != nil {
		return nil, "", err
	}
	rawReq, err := json.Marshal(req)
	if err != nil {
		return nil, "", err
	}

	httpReq, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(rawReq))
	if err != nil {
		return nil, "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := client.Do(httpReq)
	if err != nil {
		return nil, "", err
	}
	defer httpResp.Body.Close()
	inferenceID := strings.TrimSpace(httpResp.Header.Get(utils.XInferenceIdHeader))

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, inferenceID, err
	}
	if httpResp.StatusCode >= 400 {
		return nil, inferenceID, fmt.Errorf("status=%d body=%s", httpResp.StatusCode, string(body))
	}

	var parsed encryptedInferenceResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, inferenceID, err
	}
	return &parsed, inferenceID, nil
}

func close(client *http.Client, baseURL, sessionID string) error {
	endpoint, err := url.JoinPath(baseURL, "/v1/tee/sessions/close")
	if err != nil {
		return err
	}
	rawReq, err := json.Marshal(closeSessionRequest{SessionID: sessionID})
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(rawReq))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return err
	}
	if httpResp.StatusCode >= 400 {
		return fmt.Errorf("status=%d body=%s", httpResp.StatusCode, string(body))
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
