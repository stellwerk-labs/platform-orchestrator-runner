//go:build integration

package rolloutagent

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stretchr/testify/require"
)

func TestAgentDrainRetriesFetchedCommandsAndRetainsAuthority(t *testing.T) {
	f := newTransportFixture(t, "reviewed")
	delivery := f.deliver(t, f.signed(t))
	status := f.engine.Drain()
	require.True(t, status.AuthorityAlive)
	require.True(t, status.Draining)
	require.False(t, status.Accepting)
	operation, err := handleDelivery(t.Context(), f.engine, f.client, f.config.OrgID, f.config.RunnerID, delivery)
	require.NoError(t, err)
	require.Equal(t, "retry_command", operation)
	_, err = f.engine.Get(t.Context(), f.command.ID)
	require.Error(t, err, "drain imported a fetched command")
	time.Sleep(1100 * time.Millisecond)
	replayed, err := f.client.NextCommand(t.Context())
	require.NoError(t, err)
	require.Greater(t, replayed.Attempt, delivery.Attempt)
	require.Equal(t, delivery.Command, replayed.Command)
	_, err = f.engine.Decide(t.Context(), f.command.ID, "subject", "approve", "operator")
	require.Error(t, err)
	var refusal *rollout.Error
	require.NotErrorAs(t, err, &refusal, "ordinary drain must not be a permanent business refusal")
	require.True(t, f.engine.Ready())
}

func TestAgentDrainControlsRequireAuthorization(t *testing.T) {
	f := newTransportFixture(t, "reviewed")
	handler := Handler(f.engine, "private-operator-token", nil)
	for _, endpoint := range []struct{ method, path string }{{http.MethodGet, "status"}, {http.MethodGet, "ready"}, {http.MethodPost, "drain"}} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(endpoint.method, "/v1/worker/"+endpoint.path, nil))
		require.Equal(t, http.StatusUnauthorized, response.Code)
	}
	require.True(t, f.engine.Accepting(), "unauthorized drain changed admission")
	request := httptest.NewRequest(http.MethodPost, "/v1/worker/drain", nil)
	request.Header.Set("Authorization", "Bearer private-operator-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	require.False(t, f.engine.Accepting())
	require.True(t, f.engine.Ready())
}
