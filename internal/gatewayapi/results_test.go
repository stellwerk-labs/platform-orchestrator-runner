package gatewayapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resultGateway answers each result POST with the next status of the script and repeats the last
// one; status 0 drops the connection without an answer, like a gateway Pod that goes away.
func resultGateway(t *testing.T, script ...int) (*Client, *atomic.Int64) {
	t.Helper()
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		attempt := int(attempts.Add(1))
		status := script[min(attempt, len(script))-1]
		if status == 0 {
			connection, _, err := response.(http.Hijacker).Hijack()
			if assert.NoError(t, err) {
				assert.NoError(t, connection.Close())
			}
			return
		}
		var body map[string]string
		assert.NoError(t, json.NewDecoder(request.Body).Decode(&body))
		assert.Equal(t, "success", body["status"])
		assert.Equal(t, "token", request.Header.Get(DeploymentTokenHeader))
		response.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(ClientConfig{BaseURL: server.URL, OrganizationID: "org", RunnerID: "runner"})
	require.NoError(t, err)
	client.resultRetryDelay = time.Millisecond
	return client, &attempts
}

func postResult(ctx context.Context, client *Client) error {
	return client.PostResultsRetrying(ctx, "deployment", "token", map[string]string{"status": "success"})
}

func TestPostResultsRetryingSurvivesDroppedConnection(t *testing.T) {
	client, attempts := resultGateway(t, 0, http.StatusAccepted)
	require.NoError(t, postResult(t.Context(), client))
	require.Equal(t, int64(2), attempts.Load())
}

func TestPostResultsRetryingWaitsForUnavailableGateway(t *testing.T) {
	client, attempts := resultGateway(t, http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusAccepted)
	require.NoError(t, postResult(t.Context(), client))
	require.Equal(t, int64(3), attempts.Load())
}

func TestPostResultsRetryingStopsAtDeadlineWithLastStatus(t *testing.T) {
	client, attempts := resultGateway(t, http.StatusServiceUnavailable)
	// The deadline must land in a backoff pause, not inside an attempt, so the last status is returned.
	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()
	err := postResult(ctx, client)
	var status *HTTPStatusError
	require.True(t, errors.As(err, &status), "got %v", err)
	require.Equal(t, http.StatusServiceUnavailable, status.StatusCode)
	require.Greater(t, attempts.Load(), int64(1))
}

func TestPostResultsRetryingDoesNotRepeatDenial(t *testing.T) {
	client, attempts := resultGateway(t, http.StatusForbidden, http.StatusAccepted)
	err := postResult(t.Context(), client)
	var status *HTTPStatusError
	require.True(t, errors.As(err, &status), "got %v", err)
	require.Equal(t, http.StatusForbidden, status.StatusCode)
	require.Equal(t, int64(1), attempts.Load())
}

func TestRetryableResultErrorClassifiesStatuses(t *testing.T) {
	for status, retryable := range map[int]bool{
		http.StatusTooManyRequests: true, http.StatusBadGateway: true, http.StatusServiceUnavailable: true,
		http.StatusGatewayTimeout: true, http.StatusBadRequest: false, http.StatusUnauthorized: false,
		http.StatusForbidden: false, http.StatusInternalServerError: false,
	} {
		require.Equal(t, retryable, retryableResultError(&HTTPStatusError{StatusCode: status}), "status %d", status)
	}
	require.True(t, retryableResultError(errors.New("connection refused")))
}
