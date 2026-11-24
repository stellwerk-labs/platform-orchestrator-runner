package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stretchr/testify/require"
)

func TestPlanLogProxyKeepsPrivateSiteAndSubjectBoundary(t *testing.T) {
	for _, test := range []struct {
		name, subject, content string
		status, want           int
	}{
		{"exact log", "exact", "visible saved plan", 200, 200},
		{"changed subject", "different", "PRIVATE_LOG_CANARY", 200, 503},
		{"not captured", "exact", "PRIVATE_ERROR_CANARY", 404, 404},
		{"oversize", "exact", strings.Repeat("x", rolloutexec.PlanLogLimit+1), 200, 413},
	} {
		t.Run(test.name, func(t *testing.T) {
			site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "Bearer private-site-token", r.Header.Get("Authorization"))
				require.Equal(t, "exact", r.URL.Query().Get("subject"))
				w.Header().Set("X-Stellwerk-Plan-Subject", test.subject)
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.content))
			}))
			defer site.Close()
			l := testLab(t)
			l.cfg.OperatorURL = site.URL
			l.token = "private-site-token"
			request := httptest.NewRequest(http.MethodGet, "/api/runner-rollouts/"+uuid.NewString()+"/plan-logs?subject=exact", nil)
			request.Header.Set("Authorization", "Bearer "+l.apiToken)
			request.Header.Set("X-Rollout-Actor", uuid.NewString())
			response := httptest.NewRecorder()
			l.handler().ServeHTTP(response, request)
			require.Equal(t, test.want, response.Code)
			require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			if test.want == 200 {
				require.Equal(t, test.content, response.Body.String())
				require.Equal(t, "exact", response.Header().Get("X-Stellwerk-Plan-Subject"))
			} else {
				require.NotContains(t, response.Body.String(), "PRIVATE_")
			}
		})
	}
}
