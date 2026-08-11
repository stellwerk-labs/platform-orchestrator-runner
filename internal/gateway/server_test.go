package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gatewayapi"
	"github.com/stretchr/testify/require"
)

type fakeBackend struct {
	delivery  CommandDelivery
	fetchErr  error
	acked     string
	retried   string
	rejected  bool
	bundle    []byte
	log       []byte
	published []hmessaging.Message
}

func (b *fakeBackend) FetchCommand(context.Context, string, string) (CommandDelivery, error) {
	if b.fetchErr != nil {
		return CommandDelivery{}, b.fetchErr
	}
	return b.delivery, nil
}
func (b *fakeBackend) Acknowledge(_ context.Context, subject string) error {
	b.acked = subject
	return nil
}
func (b *fakeBackend) Retry(_ context.Context, subject string, _ time.Duration) error {
	b.retried = subject
	return nil
}
func (b *fakeBackend) Reject(_ context.Context, _ CommandDelivery, _ error) error {
	b.rejected = true
	return nil
}
func (b *fakeBackend) RejectReceipt(context.Context, string, uint64, error) error {
	b.rejected = true
	return nil
}
func (b *fakeBackend) GetBundle(context.Context, string) (io.ReadCloser, error) {
	if b.bundle == nil {
		return nil, errors.New("not found")
	}
	return io.NopCloser(bytes.NewReader(b.bundle)), nil
}
func (b *fakeBackend) PutLog(_ context.Context, _ string, content io.Reader) (StoredObject, error) {
	var err error
	b.log, err = io.ReadAll(content)
	return StoredObject{Size: uint64(len(b.log))}, err
}
func (b *fakeBackend) DeleteLog(context.Context, string) error { b.log = nil; return nil }
func (b *fakeBackend) Publish(_ context.Context, message hmessaging.Message) error {
	b.published = append(b.published, message)
	return nil
}

func gatewayKeyPair(t *testing.T) ([]byte, []byte) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	require.NoError(t, err)
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
}

func testServer(t *testing.T, backend *fakeBackend, now time.Time) (*Server, []byte) {
	t.Helper()
	privateKey, publicKey := gatewayKeyPair(t)
	server, err := NewServer(ServerConfig{
		BasePath: "/runner-gateway", Backend: backend,
		PublicKeys: StaticPublicKeyResolver{OrganizationID: "org", RunnerID: "runner", PublicKey: publicKey},
		ReceiptKey: make([]byte, 32), RunnerTokenSalt: "salt", Now: func() time.Time { return now },
	})
	require.NoError(t, err)
	return server, privateKey
}

func authorizedRequest(t *testing.T, method, target string, body io.Reader, privateKey []byte, now time.Time) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, body)
	token, err := gatewayapi.SignAgentToken(privateKey, "org", "runner", now)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

func TestCommandDeliveryAndAcknowledgement(t *testing.T) {
	now := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	envelope := hmessaging.CommandEnvelope{
		ProtocolVersion: hmessaging.ProtocolVersionV1, CommandID: "create-job:deployment:1",
		OrganizationID: "org", RunnerID: "runner", DeploymentID: "deployment",
		Type: hmessaging.CommandTypeCreateJob, CreatedAt: now, ExpiresAt: now.Add(time.Hour), Payload: json.RawMessage(`{}`),
	}
	data, err := json.Marshal(envelope)
	require.NoError(t, err)
	backend := &fakeBackend{delivery: CommandDelivery{
		Message:  hmessaging.Message{ID: envelope.CommandID, Subject: "po.v1.orgs.org.runners.runner.commands", Data: data},
		Attempts: 2, AckSubject: "$JS.ACK.commands.runner.1.1.1.0.0",
		StreamSequence: 42,
	}}
	server, privateKey := testServer(t, backend, now)

	nextResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(nextResponse, authorizedRequest(t, http.MethodPost,
		"/runner-gateway/v1/orgs/org/runners/runner/commands/next", nil, privateKey, now))
	require.Equal(t, http.StatusOK, nextResponse.Code)
	var command gatewayapi.CommandResponse
	require.NoError(t, json.Unmarshal(nextResponse.Body.Bytes(), &command))
	require.Equal(t, uint64(2), command.Attempt)
	require.NotEmpty(t, command.Receipt)

	body, err := json.Marshal(gatewayapi.ReceiptRequest{Receipt: command.Receipt})
	require.NoError(t, err)
	ackResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(ackResponse, authorizedRequest(t, http.MethodPost,
		"/runner-gateway/v1/orgs/org/runners/runner/commands/create-job:deployment:1/ack", bytes.NewReader(body), privateKey, now))
	require.Equal(t, http.StatusNoContent, ackResponse.Code)
	require.Equal(t, backend.delivery.AckSubject, backend.acked)
}

func TestGatewayServesBundleAndPersistsResultsAndLogs(t *testing.T) {
	now := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	backend := &fakeBackend{bundle: []byte("bundle")}
	server, _ := testServer(t, backend, now)
	deploymentID := "344e7709-3e9d-4e69-88ac-c2025c46f7c9"
	environmentID := "ab840dce-c2a0-4bf0-93fe-9fcf998fc3c6"
	token := deploymentToken("salt", "org", deploymentID)

	bundleRequest := httptest.NewRequest(http.MethodGet,
		"/runner-gateway/v1/orgs/org/runners/runner/deployments/"+deploymentID+"/bundle", nil)
	bundleRequest.Header.Set(gatewayapi.DeploymentTokenHeader, token)
	bundleResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(bundleResponse, bundleRequest)
	require.Equal(t, http.StatusOK, bundleResponse.Code)
	require.Equal(t, "bundle", bundleResponse.Body.String())

	resultRequest := httptest.NewRequest(http.MethodPost,
		"/runner-gateway/v1/orgs/org/runners/runner/deployments/"+deploymentID+"/results",
		strings.NewReader(`{"status":"success"}`))
	resultRequest.Header.Set(gatewayapi.DeploymentTokenHeader, token)
	resultResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(resultResponse, resultRequest)
	require.Equal(t, http.StatusAccepted, resultResponse.Code)

	logsRequest := httptest.NewRequest(http.MethodPut,
		"/runner-gateway/v1/orgs/org/runners/runner/environments/"+environmentID+"/deployments/"+deploymentID+"/logs",
		strings.NewReader("encrypted-logs"))
	logsRequest.Header.Set(gatewayapi.DeploymentTokenHeader, token)
	logsResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(logsResponse, logsRequest)
	require.Equal(t, http.StatusAccepted, logsResponse.Code)
	require.Equal(t, []byte("encrypted-logs"), backend.log)
	require.Len(t, backend.published, 2)
}

func TestGatewayEnforcesEncryptedLogLimitBeforePublishingReference(t *testing.T) {
	now := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	deploymentID := "344e7709-3e9d-4e69-88ac-c2025c46f7c9"
	environmentID := "ab840dce-c2a0-4bf0-93fe-9fcf998fc3c6"
	token := deploymentToken("salt", "org", deploymentID)

	backend := &fakeBackend{}
	server, _ := testServer(t, backend, now)
	server.maxLogBytes = 8
	request := httptest.NewRequest(http.MethodPut,
		"/runner-gateway/v1/orgs/org/runners/runner/environments/"+environmentID+"/deployments/"+deploymentID+"/logs",
		strings.NewReader("nine-byte"))
	request.Header.Set(gatewayapi.DeploymentTokenHeader, token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	require.Equal(t, http.StatusRequestEntityTooLarge, response.Code)
	require.Nil(t, backend.log, "an oversized object must be removed")
	require.Empty(t, backend.published, "an oversized object must not be announced")
}
