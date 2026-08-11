package gateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hmessaging"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gatewayapi"
)

const (
	defaultFetchWait         = 25 * time.Second
	deliveryLease            = 90 * time.Second
	maxResultBytes     int64 = 4 << 20
	defaultMaxLogBytes       = 16 << 20
)

type ServerConfig struct {
	BasePath        string
	Backend         Backend
	PublicKeys      PublicKeyResolver
	ReceiptKey      []byte
	RunnerTokenSalt string
	FetchWait       time.Duration
	MaxLogBytes     int64
	Now             func() time.Time
}

type Server struct {
	basePath        string
	backend         Backend
	publicKeys      PublicKeyResolver
	receipts        *receiptCipher
	runnerTokenSalt string
	fetchWait       time.Duration
	maxLogBytes     int64
	now             func() time.Time
}

func NewServer(config ServerConfig) (*Server, error) {
	if config.Backend == nil || config.PublicKeys == nil {
		return nil, errors.New("gateway backend and public key resolver are required")
	}
	receipts, err := newReceiptCipher(config.ReceiptKey)
	if err != nil {
		return nil, err
	}
	if config.RunnerTokenSalt == "" {
		return nil, errors.New("runner token salt is required")
	}
	basePath := strings.TrimRight(config.BasePath, "/")
	if basePath != "" && !strings.HasPrefix(basePath, "/") {
		return nil, errors.New("gateway base path must be empty or start with a slash")
	}
	if config.FetchWait <= 0 {
		config.FetchWait = defaultFetchWait
	}
	if config.MaxLogBytes <= 0 {
		config.MaxLogBytes = defaultMaxLogBytes
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Server{
		basePath: basePath, backend: config.Backend, publicKeys: config.PublicKeys,
		receipts: receipts, runnerTokenSalt: config.RunnerTokenSalt,
		fetchWait: config.FetchWait, maxLogBytes: config.MaxLogBytes, now: config.Now,
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+s.basePath+"/healthz", func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST "+s.basePath+"/v1/orgs/{organizationID}/runners/{runnerID}/commands/next", s.nextCommand)
	mux.HandleFunc("POST "+s.basePath+"/v1/orgs/{organizationID}/runners/{runnerID}/commands/{commandID}/ack", s.acknowledgeCommand)
	mux.HandleFunc("POST "+s.basePath+"/v1/orgs/{organizationID}/runners/{runnerID}/commands/{commandID}/retry", s.retryCommand)
	mux.HandleFunc("POST "+s.basePath+"/v1/orgs/{organizationID}/runners/{runnerID}/commands/{commandID}/reject", s.rejectCommand)
	mux.HandleFunc("POST "+s.basePath+"/v1/orgs/{organizationID}/runners/{runnerID}/events", s.postEvent)
	mux.HandleFunc("GET "+s.basePath+"/v1/orgs/{organizationID}/runners/{runnerID}/deployments/{deploymentID}/bundle", s.getBundle)
	mux.HandleFunc("POST "+s.basePath+"/v1/orgs/{organizationID}/runners/{runnerID}/deployments/{deploymentID}/results", s.postResults)
	mux.HandleFunc("PUT "+s.basePath+"/v1/orgs/{organizationID}/runners/{runnerID}/environments/{environmentID}/deployments/{deploymentID}/logs", s.putLogs)
	return mux
}

func (s *Server) nextCommand(response http.ResponseWriter, request *http.Request) {
	organizationID, runnerID, ok := s.authenticateAgent(response, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), s.fetchWait)
	defer cancel()
	for {
		delivery, err := s.backend.FetchCommand(ctx, organizationID, runnerID)
		if errors.Is(err, ErrNoCommand) {
			response.WriteHeader(http.StatusNoContent)
			return
		}
		if err != nil {
			writeError(response, http.StatusServiceUnavailable, "fetch runner command")
			return
		}
		envelope, err := decodeCommandEnvelope(delivery.Message)
		if err != nil || envelope.OrganizationID != organizationID || envelope.RunnerID != runnerID || envelope.CommandID == "" {
			if err == nil {
				err = errors.New("command envelope identity does not match the authenticated runner")
			}
			if rejectErr := s.backend.Reject(ctx, delivery, err); rejectErr != nil {
				writeError(response, http.StatusServiceUnavailable, "reject invalid runner command")
				return
			}
			continue
		}
		now := s.now().UTC()
		if !envelope.ExpiresAt.IsZero() && !now.Before(envelope.ExpiresAt) {
			if err := s.backend.Reject(ctx, delivery, errors.New("command expired before delivery")); err != nil {
				writeError(response, http.StatusServiceUnavailable, "reject expired runner command")
				return
			}
			continue
		}
		leaseExpiresAt := now.Add(deliveryLease)
		receipt, err := s.receipts.seal(receiptClaims{
			OrganizationID: organizationID, RunnerID: runnerID, CommandID: envelope.CommandID,
			AckSubject: delivery.AckSubject, StreamSequence: delivery.StreamSequence, ExpiresAt: leaseExpiresAt.Unix(),
		})
		if err != nil {
			writeError(response, http.StatusInternalServerError, "create delivery receipt")
			return
		}
		writeJSON(response, http.StatusOK, gatewayapi.CommandResponse{
			Command: delivery.Message.Data, Receipt: receipt, Attempt: delivery.Attempts, LeaseExpiresAt: leaseExpiresAt,
		})
		return
	}
}

func (s *Server) rejectCommand(response http.ResponseWriter, request *http.Request) {
	organizationID, runnerID, ok := s.authenticateAgent(response, request)
	if !ok {
		return
	}
	var body gatewayapi.RejectRequest
	if !decodeJSON(response, request, &body, 8192) {
		return
	}
	claims, err := s.receipts.open(body.Receipt, s.now())
	if err != nil || claims.OrganizationID != organizationID || claims.RunnerID != runnerID || claims.CommandID != request.PathValue("commandID") {
		writeError(response, http.StatusBadRequest, "delivery receipt is invalid for this command")
		return
	}
	if body.Reason == "" || len(body.Reason) > 4096 {
		writeError(response, http.StatusBadRequest, "rejection reason must contain between 1 and 4096 bytes")
		return
	}
	if err := s.backend.RejectReceipt(request.Context(), claims.AckSubject, claims.StreamSequence, errors.New(body.Reason)); err != nil {
		writeError(response, http.StatusServiceUnavailable, "persist rejected command")
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) postEvent(response http.ResponseWriter, request *http.Request) {
	organizationID, runnerID, ok := s.authenticateAgent(response, request)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxResultBytes+1))
	if err != nil || int64(len(body)) > maxResultBytes {
		writeError(response, http.StatusBadRequest, "runner event exceeds the configured limit")
		return
	}
	var event hmessaging.EventEnvelope
	if err := json.Unmarshal(body, &event); err != nil || event.EventID == "" || event.OrganizationID != organizationID || event.RunnerID != runnerID {
		writeError(response, http.StatusBadRequest, "runner event identity or payload is invalid")
		return
	}
	subject, err := hmessaging.RunnerEventSubject(organizationID, runnerID, event.Type)
	if err != nil {
		writeError(response, http.StatusBadRequest, "runner event type is invalid")
		return
	}
	if err := s.backend.Publish(request.Context(), hmessaging.Message{
		ID: event.EventID, Subject: subject, Data: body, CreatedAt: event.CreatedAt,
	}); err != nil {
		writeError(response, http.StatusServiceUnavailable, "persist runner event")
		return
	}
	response.WriteHeader(http.StatusAccepted)
}

func (s *Server) acknowledgeCommand(response http.ResponseWriter, request *http.Request) {
	s.handleReceipt(response, request, false)
}

func (s *Server) retryCommand(response http.ResponseWriter, request *http.Request) {
	s.handleReceipt(response, request, true)
}

func (s *Server) handleReceipt(response http.ResponseWriter, request *http.Request, retry bool) {
	organizationID, runnerID, ok := s.authenticateAgent(response, request)
	if !ok {
		return
	}
	var receipt string
	var delay time.Duration
	if retry {
		var body gatewayapi.RetryRequest
		if !decodeJSON(response, request, &body, 4096) {
			return
		}
		receipt = body.Receipt
		if body.DelaySeconds < 0 || body.DelaySeconds > 60 {
			writeError(response, http.StatusBadRequest, "retry delay must be between 0 and 60 seconds")
			return
		}
		delay = time.Duration(body.DelaySeconds) * time.Second
	} else {
		var body gatewayapi.ReceiptRequest
		if !decodeJSON(response, request, &body, 4096) {
			return
		}
		receipt = body.Receipt
	}
	claims, err := s.receipts.open(receipt, s.now())
	if err != nil || claims.OrganizationID != organizationID || claims.RunnerID != runnerID || claims.CommandID != request.PathValue("commandID") {
		writeError(response, http.StatusBadRequest, "delivery receipt is invalid for this command")
		return
	}
	if retry {
		err = s.backend.Retry(request.Context(), claims.AckSubject, delay)
	} else {
		err = s.backend.Acknowledge(request.Context(), claims.AckSubject)
	}
	if err != nil {
		writeError(response, http.StatusServiceUnavailable, "persist command acknowledgement")
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) getBundle(response http.ResponseWriter, request *http.Request) {
	organizationID, runnerID, deploymentID, ok := s.authenticateDeployment(response, request)
	if !ok {
		return
	}
	bundle, err := s.backend.GetBundle(request.Context(), organizationID+"/"+deploymentID)
	if err != nil {
		writeError(response, http.StatusNotFound, "deployment bundle is not available")
		return
	}
	defer func() { _ = bundle.Close() }()
	response.Header().Set("Content-Type", "application/gzip")
	response.Header().Set("Cache-Control", "no-store")
	if _, err := io.Copy(response, bundle); err != nil {
		slog.WarnContext(request.Context(), "stream deployment bundle", "err", err, "runner_id", runnerID)
	}
}

func (s *Server) postResults(response http.ResponseWriter, request *http.Request) {
	organizationID, runnerID, deploymentID, ok := s.authenticateDeployment(response, request)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxResultBytes+1))
	if err != nil || int64(len(body)) > maxResultBytes || !json.Valid(body) {
		writeError(response, http.StatusBadRequest, "deployment result must be valid bounded JSON")
		return
	}
	now := s.now().UTC()
	event := hmessaging.EventEnvelope{
		ProtocolVersion: hmessaging.ProtocolVersionV1, EventID: deploymentID + ":deployment-result",
		OrganizationID: organizationID, RunnerID: runnerID, DeploymentID: deploymentID,
		Type: "deployment-result", CreatedAt: now, Payload: body,
	}
	data, err := json.Marshal(event)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "encode deployment result")
		return
	}
	subject, err := hmessaging.RunnerEventSubject(organizationID, runnerID, event.Type)
	if err != nil {
		writeError(response, http.StatusBadRequest, "runner identity is invalid")
		return
	}
	if err := s.backend.Publish(request.Context(), hmessaging.Message{ID: event.EventID, Subject: subject, Data: data, CreatedAt: now}); err != nil {
		writeError(response, http.StatusServiceUnavailable, "persist deployment result")
		return
	}
	response.WriteHeader(http.StatusAccepted)
}

func (s *Server) putLogs(response http.ResponseWriter, request *http.Request) {
	organizationID, runnerID, deploymentID, ok := s.authenticateDeployment(response, request)
	if !ok {
		return
	}
	environmentID := request.PathValue("environmentID")
	if _, err := uuid.Parse(environmentID); err != nil {
		writeError(response, http.StatusBadRequest, "environment ID must be a UUID")
		return
	}
	key := environmentID + "/" + deploymentID
	hash := sha256.New()
	limited := &boundedReader{reader: request.Body, remaining: s.maxLogBytes + 1}
	stored, err := s.backend.PutLog(request.Context(), key, io.TeeReader(limited, hash))
	if err != nil {
		writeError(response, http.StatusServiceUnavailable, "persist encrypted logs")
		return
	}
	if stored.Size > uint64(s.maxLogBytes) || limited.exceeded {
		_ = s.backend.DeleteLog(request.Context(), key)
		writeError(response, http.StatusRequestEntityTooLarge, "encrypted logs exceed the configured limit")
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"bucket": runnerLogsBucket, "key": key, "size": stored.Size, "sha256": hex.EncodeToString(hash.Sum(nil)),
	})
	now := s.now().UTC()
	event := hmessaging.EventEnvelope{
		ProtocolVersion: hmessaging.ProtocolVersionV1, EventID: deploymentID + ":log-object-ready",
		OrganizationID: organizationID, RunnerID: runnerID, DeploymentID: deploymentID,
		Type: "log-object-ready", CreatedAt: now, Payload: payload,
	}
	data, _ := json.Marshal(event)
	subject, err := hmessaging.RunnerEventSubject(organizationID, runnerID, event.Type)
	if err != nil {
		_ = s.backend.DeleteLog(request.Context(), key)
		writeError(response, http.StatusBadRequest, "runner identity is invalid")
		return
	}
	if err := s.backend.Publish(request.Context(), hmessaging.Message{ID: event.EventID, Subject: subject, Data: data, CreatedAt: now}); err != nil {
		writeError(response, http.StatusServiceUnavailable, "persist log reference")
		return
	}
	response.WriteHeader(http.StatusAccepted)
}

func (s *Server) authenticateAgent(response http.ResponseWriter, request *http.Request) (string, string, bool) {
	organizationID, runnerID := request.PathValue("organizationID"), request.PathValue("runnerID")
	if _, err := hmessaging.RunnerCommandSubject(organizationID, runnerID); err != nil {
		writeError(response, http.StatusBadRequest, "runner identity is invalid")
		return "", "", false
	}
	authorization := strings.Fields(request.Header.Get("Authorization"))
	if len(authorization) != 2 || !strings.EqualFold(authorization[0], "Bearer") {
		writeError(response, http.StatusUnauthorized, "Bearer authorization is required")
		return "", "", false
	}
	publicKey, err := s.publicKeys.Resolve(request.Context(), organizationID, runnerID)
	if err != nil || gatewayapi.VerifyAgentToken(authorization[1], publicKey, organizationID, runnerID, s.now()) != nil {
		writeError(response, http.StatusUnauthorized, "runner authorization is invalid")
		return "", "", false
	}
	return organizationID, runnerID, true
}

func (s *Server) authenticateDeployment(response http.ResponseWriter, request *http.Request) (string, string, string, bool) {
	organizationID, runnerID, deploymentID := request.PathValue("organizationID"), request.PathValue("runnerID"), request.PathValue("deploymentID")
	if _, err := hmessaging.RunnerEventSubject(organizationID, runnerID, "deployment-result"); err != nil {
		writeError(response, http.StatusBadRequest, "runner identity is invalid")
		return "", "", "", false
	}
	if _, err := uuid.Parse(deploymentID); err != nil {
		writeError(response, http.StatusBadRequest, "deployment ID must be a UUID")
		return "", "", "", false
	}
	expected := deploymentToken(s.runnerTokenSalt, organizationID, deploymentID)
	provided := request.Header.Get(gatewayapi.DeploymentTokenHeader)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) != 1 {
		writeError(response, http.StatusUnauthorized, "deployment authorization is invalid")
		return "", "", "", false
	}
	return organizationID, runnerID, deploymentID, true
}

func deploymentToken(secret, organizationID, deploymentID string) string {
	hash := sha256.New()
	_, _ = fmt.Fprint(hash, secret, organizationID, deploymentID)
	return base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
}

type boundedReader struct {
	reader    io.Reader
	remaining int64
	exceeded  bool
}

func (r *boundedReader) Read(buffer []byte) (int, error) {
	if r.remaining <= 0 {
		r.exceeded = true
		return 0, io.EOF
	}
	if int64(len(buffer)) > r.remaining {
		buffer = buffer[:r.remaining]
	}
	read, err := r.reader.Read(buffer)
	r.remaining -= int64(read)
	if r.remaining == 0 {
		probe := []byte{0}
		if n, _ := r.reader.Read(probe); n > 0 {
			r.exceeded = true
		}
	}
	return read, err
}

func decodeJSON(response http.ResponseWriter, request *http.Request, target any, limit int64) bool {
	decoder := json.NewDecoder(io.LimitReader(request.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(response, http.StatusBadRequest, "request body is invalid")
		return false
	}
	return true
}

func writeJSON(response http.ResponseWriter, status int, body any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(body)
}

func writeError(response http.ResponseWriter, status int, message string) {
	writeJSON(response, status, map[string]string{"error": message})
}
