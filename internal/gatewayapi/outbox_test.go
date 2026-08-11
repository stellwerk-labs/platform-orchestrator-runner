package gatewayapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOutboxPersistsAndFlushesResult(t *testing.T) {
	var available atomic.Bool
	var deliveries atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !available.Load() {
			http.Error(response, "offline", http.StatusServiceUnavailable)
			return
		}
		deliveries.Add(1)
		response.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{BaseURL: server.URL, OrganizationID: "org", RunnerID: "runner"})
	require.NoError(t, err)
	outbox, err := NewOutbox(t.TempDir(), client)
	require.NoError(t, err)

	require.NoError(t, outbox.PostResults(context.Background(), "deployment", "token", map[string]string{"status": "success"}))
	available.Store(true)
	require.NoError(t, outbox.Flush(context.Background()))
	require.Equal(t, int64(1), deliveries.Load())
	require.NoError(t, outbox.Flush(context.Background()))
	require.Equal(t, int64(1), deliveries.Load())
}

func TestOutboxPersistsAndFlushesEncryptedLogs(t *testing.T) {
	var available atomic.Bool
	var deliveries atomic.Int64
	const encryptedLogs = "age-encrypted-log-payload"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !available.Load() {
			http.Error(response, "offline", http.StatusServiceUnavailable)
			return
		}
		require.Equal(t, http.MethodPut, request.Method)
		require.Equal(t, "/v1/orgs/org/runners/runner/environments/environment/deployments/deployment/logs", request.URL.Path)
		require.Equal(t, "token", request.Header.Get(DeploymentTokenHeader))
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		require.Equal(t, encryptedLogs, string(body))
		deliveries.Add(1)
		response.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{BaseURL: server.URL, OrganizationID: "org", RunnerID: "runner"})
	require.NoError(t, err)
	outbox, err := NewOutbox(t.TempDir(), client)
	require.NoError(t, err)

	require.NoError(t, outbox.PutLogs(context.Background(), "environment", "deployment", "token", encryptedLogs))
	available.Store(true)
	require.NoError(t, outbox.Flush(context.Background()))
	require.Equal(t, int64(1), deliveries.Load())
	require.NoError(t, outbox.Flush(context.Background()))
	require.Equal(t, int64(1), deliveries.Load())
}
