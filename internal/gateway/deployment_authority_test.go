package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestGatewayDirectAuthorityConstructorRejectsContradictoryProfiles(t *testing.T) {
	for _, cfg := range []ServerConfig{
		{DisableDirectDeployments: true, RetainedPlansEnabled: true},
		{DisableDirectDeployments: true, DeploymentAuthorityDPURL: "http://dp:8080"},
		{DeploymentAuthorityDPURL: "http://dp:8080#"},
	} {
		_, err := NewServer(cfg)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "backend", "profile validation must precede backend setup")
	}
}

func TestGatewayDirectAuthorityRequiresOneBoundedAffirmativeReceipt(t *testing.T) {
	deployment, environment := uuid.NewString(), uuid.NewString()
	access := deploymentAccess{Legacy: true, OrganizationID: "O", RunnerID: "R", DeploymentID: deployment, EnvironmentUUID: environment}
	valid, err := json.Marshal(access)
	require.NoError(t, err)
	for _, test := range []struct {
		name, body, contentType string
		status                  int
		valid                   bool
	}{
		{"legacy", string(valid), "application/json; charset=UTF-8", 200, true},
		{"missing", "{}", "application/json", 200, false},
		{"native", strings.Replace(string(valid), `"legacy":true`, `"legacy":false`, 1), "application/json", 200, false},
		{"wrong_org", strings.Replace(string(valid), `"O"`, `"foreign"`, 1), "application/json", 200, false},
		{"wrong_runner", strings.Replace(string(valid), `"R"`, `"foreign"`, 1), "application/json", 200, false},
		{"wrong_deployment", strings.Replace(string(valid), deployment, uuid.NewString(), 1), "application/json", 200, false},
		{"zero_environment", strings.Replace(string(valid), environment, uuid.Nil.String(), 1), "application/json", 200, false},
		{"missing_environment", strings.Replace(string(valid), environment, "", 1), "application/json", 200, false},
		{"unknown_field", strings.TrimSuffix(string(valid), "}") + `,"other":true}`, "application/json", 200, false},
		{"trailing_json", string(valid) + "{}", "application/json", 200, false},
		{"malformed", "{", "application/json", 200, false},
		{"oversized", string(valid) + strings.Repeat(" ", 4096), "application/json", 200, false},
		{"wrong_media_type", string(valid), "text/plain", 200, false},
		{"unavailable", string(valid), "application/json", 503, false},
		{"not_found", string(valid), "application/json", 404, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			token := uuid.NewString()
			var requests atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				require.Equal(t, "/prefix/v1/orgs/O/runners/R/deployments/"+deployment+"/legacy-access", r.URL.Path)
				require.True(t, token == r.Header.Get("X-Deployment-Token"), "owned token header mismatch")
				require.Empty(t, r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", test.contentType)
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			t.Cleanup(peer.Close)
			server, _ := testServer(t, &fakeBackend{}, time.Now())
			server.deploymentAuthorityDPURL = peer.URL + "/prefix/"
			require.Equal(t, 5*time.Second, server.deploymentAuthorityClient.Timeout)
			receipt, err := server.legacyDeploymentAccess(t.Context(), "O", "R", deployment, token)
			if test.valid {
				require.NoError(t, err)
				require.Equal(t, access, receipt)
			} else {
				require.Error(t, err)
				require.Equal(t, deploymentAccess{}, receipt)
			}
			require.EqualValues(t, 1, requests.Load(), "lookup must not retry")
		})
	}
}

func TestGatewayDirectAuthorityNeverForwardsRedirectCredentials(t *testing.T) {
	var calls atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	t.Cleanup(sink.Close)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(peer.Close)
	server, _ := testServer(t, &fakeBackend{}, time.Now())
	server.deploymentAuthorityDPURL = peer.URL
	_, err := server.legacyDeploymentAccess(t.Context(), "O", "R", uuid.NewString(), uuid.NewString())
	require.Error(t, err)
	require.Zero(t, calls.Load())
}

func TestGatewayDirectAuthorityCancelsUnavailableLookup(t *testing.T) {
	entered := make(chan struct{})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	t.Cleanup(peer.Close)
	server, _ := testServer(t, &fakeBackend{}, time.Now())
	server.deploymentAuthorityDPURL = peer.URL
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := server.legacyDeploymentAccess(ctx, "O", "R", uuid.NewString(), uuid.NewString())
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("authority request did not arrive")
	}
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("authority request ignored cancellation")
	}
}
