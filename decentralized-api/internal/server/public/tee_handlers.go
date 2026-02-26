package public

import (
	"bytes"
	"context"
	"decentralized-api/utils"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/productscience/inference/api/inference/inference"
	"github.com/productscience/inference/x/inference/calculations"
	chainTypes "github.com/productscience/inference/x/inference/types"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

const (
	teeNodeInferPath              = "/v1/tee/chat/completions"
	teeExecutorInferPath          = "/v1/tee/executor/chat/completions"
	teeSessionTTL                 = 30 * time.Minute
	teeInferenceNodeVersionPrefix = "tee:"
	teeConfidentialFinishMarker   = "tee-confidential-v1"
	teeHeaderModel                = "X-Tee-Model"
	teeHeaderMaxTokens            = "X-Tee-Max-Tokens"
	teeHeaderNodeURL              = "X-Tee-Node-Url"
	teeHeaderOriginalPromptHash   = "X-Tee-Original-Prompt-Hash"
	teeSyntheticPromptTokenCount  = uint64(1)
	teeDefaultCompletionTokenCost = uint64(0)
)

type teeReserveSessionRequest struct {
	UserID           string `json:"user_id,omitempty"`
	RequesterAddress string `json:"requester_address,omitempty"`
	Model            string `json:"model"`
	MaxTokens        int    `json:"max_tokens"`
	ExecutorID       string `json:"executor_id,omitempty"`
}

type teeCloseSessionRequest struct {
	SessionID string `json:"session_id"`
}

type teeEncryptedInferenceRequest struct {
	SessionID          string `json:"session_id"`
	EphemeralPublicKey string `json:"ephemeral_public_key"`
	Nonce              string `json:"nonce"`
	Ciphertext         string `json:"ciphertext"`
	AAD                string `json:"aad,omitempty"`
	RequestTimestamp   int64  `json:"request_timestamp,omitempty"`
	DeveloperSignature string `json:"developer_signature,omitempty"`
	PromptHash         string `json:"prompt_hash,omitempty"`
	OriginalPromptHash string `json:"original_prompt_hash,omitempty"`
	RequesterAddress   string `json:"requester_address,omitempty"`
}

type teeSession struct {
	SessionID          string `json:"session_id"`
	Model              string `json:"model,omitempty"`
	MaxTokens          int    `json:"max_tokens,omitempty"`
	NodeID             string `json:"node_id,omitempty"`
	NodeURL            string `json:"node_url"`
	NodePublicKey      string `json:"node_public_key,omitempty"`
	ExecutorAddress    string `json:"executor_address,omitempty"`
	ExecutorURL        string `json:"executor_url,omitempty"`
	RequesterAddress   string `json:"requester_address,omitempty"`
	TransferAddress    string `json:"transfer_address,omitempty"`
	InferenceID        string `json:"inference_id,omitempty"`
	RequestTimestamp   int64  `json:"request_timestamp,omitempty"`
	PromptHash         string `json:"prompt_hash,omitempty"`
	OriginalPromptHash string `json:"original_prompt_hash,omitempty"`
	TransferSignature  string `json:"transfer_signature,omitempty"`
	Started            bool   `json:"started,omitempty"`
	Completed          bool   `json:"completed,omitempty"`
	CreatedAt          int64  `json:"created_at,omitempty"`
	ExpiresAt          int64  `json:"expires_at,omitempty"`
}

type teeGetSessionResponse struct {
	Ok      bool       `json:"ok"`
	Session teeSession `json:"session"`
	Error   string     `json:"error"`
}

type teeProviderCandidate struct {
	participantAddress string
	inferenceURL       string
	nodeLocalID        string
	nodeURL            string
	nodePublicKey      string
}

type teeEncryptedInferenceResponse struct {
	SessionID  string `json:"session_id"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
	AAD        string `json:"aad,omitempty"`
	Metadata   struct {
		UsedTokens int `json:"used_tokens,omitempty"`
	} `json:"metadata,omitempty"`
}

func (s *Server) postTeeReserveSession(ctx echo.Context) error {
	var req teeReserveSessionRequest
	if _, err := readJSONBody(ctx, &req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid JSON body")
	}
	if req.Model == "" || req.MaxTokens <= 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "model and max_tokens are required")
	}
	if s.recorder == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "chain recorder is not initialized")
	}

	requesterAddress := strings.TrimSpace(req.RequesterAddress)
	if requesterAddress == "" {
		requesterAddress = strings.TrimSpace(req.UserID)
	}
	if requesterAddress == "" {
		requesterAddress = s.recorder.GetAccountAddress()
	}

	queryClient := s.recorder.NewInferenceQueryClient()
	hardwareResp, err := queryClient.HardwareNodesAll(ctx.Request().Context(), &chainTypes.QueryHardwareNodesAllRequest{})
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, fmt.Sprintf("failed to load hardware nodes from chain: %v", err))
	}

	selected, err := s.selectTEEProvider(ctx.Request().Context(), queryClient, hardwareResp.GetNodes(), req.Model, req.ExecutorID)
	if err != nil {
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}

	now := time.Now().UTC()
	session := teeSession{
		SessionID:        uuid.NewString(),
		Model:            req.Model,
		MaxTokens:        req.MaxTokens,
		NodeID:           selected.participantAddress + "/" + selected.nodeLocalID,
		NodeURL:          selected.nodeURL,
		NodePublicKey:    selected.nodePublicKey,
		ExecutorAddress:  selected.participantAddress,
		ExecutorURL:      strings.TrimRight(selected.inferenceURL, "/"),
		RequesterAddress: requesterAddress,
		TransferAddress:  s.recorder.GetAccountAddress(),
		CreatedAt:        now.Unix(),
		ExpiresAt:        now.Add(teeSessionTTL).Unix(),
	}
	s.teeSessions.put(session)

	return ctx.JSON(http.StatusOK, map[string]any{
		"ok": true,
		"session": map[string]any{
			"session_id":        session.SessionID,
			"requester_address": session.RequesterAddress,
			"model":             session.Model,
			"max_tokens":        session.MaxTokens,
			"node_id":           session.NodeID,
			"node_url":          session.NodeURL,
		},
		"assignment": map[string]any{
			"executor_id":     session.ExecutorAddress,
			"executor_url":    session.ExecutorURL,
			"node_id":         session.NodeID,
			"node_url":        session.NodeURL,
			"node_public_key": session.NodePublicKey,
		},
	})
}

func (s *Server) postTeeChatCompletions(ctx echo.Context) error {
	var req teeEncryptedInferenceRequest
	rawBody, err := readJSONBody(ctx, &req)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid JSON body")
	}
	if req.SessionID == "" || req.EphemeralPublicKey == "" || req.Nonce == "" || req.Ciphertext == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "session_id, ephemeral_public_key, nonce and ciphertext are required")
	}

	sessionResp, err := s.fetchTEESession(req.SessionID)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, err.Error())
	}
	session := sessionResp.Session
	if session.ExecutorURL == "" {
		return echo.NewHTTPError(http.StatusConflict, "assigned executor does not expose inference_url")
	}
	if session.Completed {
		return echo.NewHTTPError(http.StatusConflict, "tee session already completed")
	}

	if !session.Started {
		session, err = s.startTEEInference(session, req, rawBody)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, fmt.Sprintf("failed to start tee inference on chain: %v", err))
		}
		s.teeSessions.put(session)
	}

	forwardURL, err := url.JoinPath(strings.TrimRight(session.ExecutorURL, "/"), teeExecutorInferPath)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "invalid executor URL")
	}
	headers := map[string]string{
		utils.XInferenceIdHeader:      session.InferenceID,
		utils.XTimestampHeader:        strconv.FormatInt(session.RequestTimestamp, 10),
		utils.XRequesterAddressHeader: session.RequesterAddress,
		utils.XTransferAddressHeader:  session.TransferAddress,
		utils.XTASignatureHeader:      session.TransferSignature,
		utils.XPromptHashHeader:       session.PromptHash,
		teeHeaderOriginalPromptHash:   session.OriginalPromptHash,
		teeHeaderModel:                session.Model,
		teeHeaderMaxTokens:            strconv.Itoa(session.MaxTokens),
		teeHeaderNodeURL:              session.NodeURL,
	}

	status, body, err := s.forwardJSONWithHeaders(http.MethodPost, forwardURL, rawBody, headers)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, fmt.Sprintf("failed to relay encrypted inference: %v", err))
	}
	session.Completed = true
	s.teeSessions.put(session)
	ctx.Response().Header().Set(utils.XInferenceIdHeader, session.InferenceID)
	return ctx.Blob(status, "application/json", body)
}

func (s *Server) postTeeExecutorChatCompletions(ctx echo.Context) error {
	var req teeEncryptedInferenceRequest
	rawBody, err := readJSONBody(ctx, &req)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid JSON body")
	}
	if req.SessionID == "" || req.EphemeralPublicKey == "" || req.Nonce == "" || req.Ciphertext == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "session_id, ephemeral_public_key, nonce and ciphertext are required")
	}
	if s.recorder == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "chain recorder is not initialized")
	}

	inferenceID := strings.TrimSpace(ctx.Request().Header.Get(utils.XInferenceIdHeader))
	requestTimestampRaw := strings.TrimSpace(ctx.Request().Header.Get(utils.XTimestampHeader))
	requesterAddress := strings.TrimSpace(ctx.Request().Header.Get(utils.XRequesterAddressHeader))
	transferAddress := strings.TrimSpace(ctx.Request().Header.Get(utils.XTransferAddressHeader))
	transferSignature := strings.TrimSpace(ctx.Request().Header.Get(utils.XTASignatureHeader))
	promptHash := strings.TrimSpace(ctx.Request().Header.Get(utils.XPromptHashHeader))
	originalPromptHash := strings.TrimSpace(ctx.Request().Header.Get(teeHeaderOriginalPromptHash))
	model := strings.TrimSpace(ctx.Request().Header.Get(teeHeaderModel))
	nodeURL := strings.TrimSpace(ctx.Request().Header.Get(teeHeaderNodeURL))
	if inferenceID == "" || requestTimestampRaw == "" || requesterAddress == "" || transferAddress == "" || transferSignature == "" || promptHash == "" || model == "" || nodeURL == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "missing required tee execution headers")
	}
	if originalPromptHash == "" {
		originalPromptHash = promptHash
	}
	requestTimestamp, err := strconv.ParseInt(requestTimestampRaw, 10, 64)
	if err != nil || requestTimestamp <= 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid request timestamp")
	}

	inferURL, err := url.JoinPath(strings.TrimRight(nodeURL, "/"), teeNodeInferPath)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "invalid tee node URL")
	}

	status, body, err := s.forwardJSON(http.MethodPost, inferURL, rawBody)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, fmt.Sprintf("failed to relay encrypted inference to tee node: %v", err))
	}

	completionTokens := teeDefaultCompletionTokenCost
	if used, usedErr := extractCompletionTokensFromTEEEncryptedResponse(body); usedErr == nil && used > 0 {
		completionTokens = uint64(used)
	}

	executorSignature, err := s.calculateSignature(promptHash, requestTimestamp, transferAddress, s.recorder.GetAccountAddress(), calculations.ExecutorAgent)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, fmt.Sprintf("failed to sign tee finish payload: %v", err))
	}
	if err := s.recorder.FinishInference(&inference.MsgFinishInference{
		InferenceId:          inferenceID,
		ResponseHash:         utils.GenerateSHA256HashBytes(body),
		ResponsePayload:      teeConfidentialFinishMarker,
		PromptTokenCount:     teeSyntheticPromptTokenCount,
		CompletionTokenCount: completionTokens,
		TransferredBy:        transferAddress,
		TransferSignature:    transferSignature,
		ExecutorSignature:    executorSignature,
		RequestTimestamp:     requestTimestamp,
		RequestedBy:          requesterAddress,
		Model:                model,
		PromptHash:           promptHash,
		OriginalPromptHash:   originalPromptHash,
	}); err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, fmt.Sprintf("failed to submit finish inference: %v", err))
	}

	return ctx.Blob(status, "application/json", body)
}

func (s *Server) postTeeCloseSession(ctx echo.Context) error {
	var req teeCloseSessionRequest
	if _, err := readJSONBody(ctx, &req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid JSON body")
	}
	if req.SessionID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "session_id is required")
	}

	if session, removed := s.teeSessions.delete(req.SessionID); removed {
		return ctx.JSON(http.StatusOK, map[string]any{
			"ok":            true,
			"session_id":    session.SessionID,
			"inference_id":  session.InferenceID,
			"status":        "closed",
			"spent_tokens":  0, // paid amount is accounted by Start/Finish tx lifecycle
			"refund_tokens": session.MaxTokens,
		})
	}
	return echo.NewHTTPError(http.StatusNotFound, "session not found")
}

func (s *Server) selectTEEProvider(
	ctx context.Context,
	queryClient chainTypes.QueryClient,
	allNodes []*chainTypes.HardwareNodes,
	model string,
	executorID string,
) (*teeProviderCandidate, error) {
	filterParticipant, filterLocalID := parseExecutorIDFilter(executorID)
	candidates := make([]*teeProviderCandidate, 0, 4)

	for _, participantNodes := range allNodes {
		if participantNodes == nil || strings.TrimSpace(participantNodes.Participant) == "" {
			continue
		}
		if filterParticipant != "" && participantNodes.Participant != filterParticipant {
			continue
		}

		participantResp, err := queryClient.Participant(ctx, &chainTypes.QueryGetParticipantRequest{
			Index: participantNodes.Participant,
		})
		if err != nil || participantResp == nil {
			continue
		}
		workerKey := strings.TrimSpace(participantResp.Participant.WorkerPublicKey)
		inferenceURL := strings.TrimSpace(participantResp.Participant.InferenceUrl)
		if workerKey == "" || inferenceURL == "" {
			continue
		}

		for _, node := range participantNodes.HardwareNodes {
			if !supportsModel(node, model) || !isTEENode(node) {
				continue
			}
			if filterLocalID != "" && strings.TrimSpace(node.LocalId) != filterLocalID {
				continue
			}
			nodeURL, ok := buildNodeURL(node)
			if !ok {
				continue
			}
			candidates = append(candidates, &teeProviderCandidate{
				participantAddress: participantNodes.Participant,
				inferenceURL:       inferenceURL,
				nodeLocalID:        node.LocalId,
				nodeURL:            nodeURL,
				nodePublicKey:      workerKey,
			})
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no tee providers available for model %q", model)
	}
	if filterParticipant != "" {
		return candidates[0], nil
	}

	return candidates[0], nil
}

func supportsModel(node *chainTypes.HardwareNode, model string) bool {
	if node == nil {
		return false
	}
	for _, nodeModel := range node.Models {
		if nodeModel == model {
			return true
		}
	}
	return false
}

func isTEENode(node *chainTypes.HardwareNode) bool {
	if node == nil {
		return false
	}
	for _, hw := range node.Hardware {
		if hw == nil {
			continue
		}
		switch strings.ToUpper(strings.TrimSpace(hw.Type)) {
		case "TEE", "TDX", "SEV-SNP", "SEV_SNP", "CONFIDENTIAL":
			return true
		}
	}
	return false
}

func buildNodeURL(node *chainTypes.HardwareNode) (string, bool) {
	if node == nil {
		return "", false
	}
	host := strings.TrimSpace(node.Host)
	port := strings.TrimSpace(node.Port)
	if host == "" || port == "" {
		return "", false
	}
	parsedPort, err := strconv.Atoi(port)
	if err != nil || parsedPort < 1 || parsedPort > 65535 {
		return "", false
	}
	return fmt.Sprintf("http://%s:%s", host, port), true
}

func parseExecutorIDFilter(executorID string) (participantAddress string, localNodeID string) {
	executorID = strings.TrimSpace(executorID)
	if executorID == "" {
		return "", ""
	}
	parts := strings.SplitN(executorID, "/", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

func (s *Server) fetchTEESession(sessionID string) (*teeGetSessionResponse, error) {
	session, found := s.teeSessions.get(sessionID)
	if !found {
		return nil, fmt.Errorf("session not found")
	}

	now := time.Now().UTC().Unix()
	if session.ExpiresAt > 0 && now > session.ExpiresAt {
		s.teeSessions.delete(sessionID)
		return nil, fmt.Errorf("session expired")
	}
	return &teeGetSessionResponse{
		Ok:      true,
		Session: session,
	}, nil
}

func (s *Server) forwardJSON(method, targetURL string, rawBody []byte) (int, []byte, error) {
	return s.forwardJSONWithHeaders(method, targetURL, rawBody, nil)
}

func (s *Server) forwardJSONWithHeaders(method, targetURL string, rawBody []byte, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequest(method, targetURL, bytes.NewReader(rawBody))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		if strings.TrimSpace(value) != "" {
			req.Header.Set(key, value)
		}
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}

func extractCompletionTokensFromTEEEncryptedResponse(rawBody []byte) (int, error) {
	var parsed teeEncryptedInferenceResponse
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		return 0, err
	}
	return parsed.Metadata.UsedTokens, nil
}

func (s *Server) startTEEInference(session teeSession, req teeEncryptedInferenceRequest, rawBody []byte) (teeSession, error) {
	if s.recorder == nil {
		return session, fmt.Errorf("chain recorder is not initialized")
	}

	requestTimestamp := req.RequestTimestamp
	if requestTimestamp <= 0 {
		requestTimestamp = time.Now().UTC().UnixNano()
	}

	requesterAddress := strings.TrimSpace(req.RequesterAddress)
	if requesterAddress == "" {
		requesterAddress = strings.TrimSpace(session.RequesterAddress)
	}
	if requesterAddress == "" {
		requesterAddress = strings.TrimSpace(session.TransferAddress)
	}

	promptHash := strings.TrimSpace(req.PromptHash)
	if promptHash == "" {
		promptHash = utils.GenerateSHA256HashBytes(rawBody)
	}
	originalPromptHash := strings.TrimSpace(req.OriginalPromptHash)
	if originalPromptHash == "" {
		originalPromptHash = promptHash
	}

	inferenceID := strings.TrimSpace(req.DeveloperSignature)
	if inferenceID == "" {
		if requesterAddress != session.TransferAddress {
			return session, fmt.Errorf("developer_signature is required when requester_address differs from transfer address")
		}
		devSignature, err := s.calculateSignature(originalPromptHash, requestTimestamp, session.TransferAddress, "", calculations.Developer)
		if err != nil {
			return session, fmt.Errorf("failed to sign developer inference id: %w", err)
		}
		inferenceID = devSignature
	}

	transferSignature, err := s.calculateSignature(promptHash, requestTimestamp, session.TransferAddress, session.ExecutorAddress, calculations.TransferAgent)
	if err != nil {
		return session, fmt.Errorf("failed to sign transfer request: %w", err)
	}

	nodeVersion := ""
	if s.configManager != nil {
		nodeVersion = s.configManager.GetCurrentNodeVersion()
	}
	nodeVersion = teeNodeVersion(nodeVersion)

	startMsg := &inference.MsgStartInference{
		InferenceId:        inferenceID,
		PromptHash:         promptHash,
		RequestedBy:        requesterAddress,
		Model:              session.Model,
		AssignedTo:         session.ExecutorAddress,
		NodeVersion:        nodeVersion,
		MaxTokens:          uint64(session.MaxTokens),
		PromptTokenCount:   teeSyntheticPromptTokenCount,
		RequestTimestamp:   requestTimestamp,
		OriginalPromptHash: originalPromptHash,
		TransferSignature:  transferSignature,
	}
	if err := s.recorder.StartInference(startMsg); err != nil {
		return session, err
	}

	session.Started = true
	session.InferenceID = inferenceID
	session.RequestTimestamp = requestTimestamp
	session.RequesterAddress = requesterAddress
	session.PromptHash = promptHash
	session.OriginalPromptHash = originalPromptHash
	session.TransferSignature = transferSignature
	return session, nil
}

func teeNodeVersion(baseVersion string) string {
	baseVersion = strings.TrimSpace(baseVersion)
	if baseVersion == "" {
		return "tee"
	}
	return teeInferenceNodeVersionPrefix + baseVersion
}

func readJSONBody(ctx echo.Context, dst any) ([]byte, error) {
	rawBody, err := io.ReadAll(ctx.Request().Body)
	if err != nil {
		return nil, err
	}
	if len(rawBody) == 0 {
		return nil, fmt.Errorf("empty body")
	}
	if err := json.Unmarshal(rawBody, dst); err != nil {
		return nil, err
	}
	return rawBody, nil
}

type teeSessionStore struct {
	mu       sync.RWMutex
	sessions map[string]teeSession
}

func newTEESessionStore() *teeSessionStore {
	return &teeSessionStore{
		sessions: make(map[string]teeSession),
	}
}

func (s *teeSessionStore) put(session teeSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[session.SessionID] = session
}

func (s *teeSessionStore) get(sessionID string) (teeSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, found := s.sessions[sessionID]
	return session, found
}

func (s *teeSessionStore) delete(sessionID string) (teeSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, found := s.sessions[sessionID]
	if found {
		delete(s.sessions, sessionID)
	}
	return session, found
}
