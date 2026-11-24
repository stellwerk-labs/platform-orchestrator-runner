package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWaitWorkerDrainRefusesReopenedAdmission(t *testing.T) {
	for _, tc := range []struct {
		name    string
		initial rollout.WorkerStatus
		polls   int32
	}{
		{"initial_replacement", rollout.WorkerStatus{AuthorityAlive: true, Accepting: true}, 0},
		{"initial_inconsistent_gate", rollout.WorkerStatus{AuthorityAlive: true, Accepting: true, Draining: true}, 0},
		{"replacement_after_poll", rollout.WorkerStatus{AuthorityAlive: true, Draining: true, ActiveStages: 1}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var polls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				polls.Add(1)
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/v1/worker/status", r.URL.Path)
				assert.Equal(t, "Bearer test-operator", r.Header.Get("Authorization"))
				// A restarted worker at the same endpoint has no active stage yet,
				// but has not been drained. It is unsafe to stop on this response.
				assert.NoError(t, json.NewEncoder(w).Encode(rollout.WorkerStatus{AuthorityAlive: true, Accepting: true}))
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			status, err := waitWorkerDrain(ctx, server.Client(), strings.TrimPrefix(server.URL, "http://"), "test-operator", tc.initial)
			require.ErrorContains(t, err, "worker admission is not closed")
			require.True(t, status.Accepting)
			require.Equal(t, tc.polls, polls.Load(), "never send a new drain to the replacement")
		})
	}
}
