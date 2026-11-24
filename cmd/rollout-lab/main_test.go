package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stretchr/testify/require"
)

func testLab(t *testing.T) *lab {
	t.Helper()
	directory := t.TempDir()
	for _, name := range []string{"imports", "projections"} {
		require.NoError(t, os.Mkdir(filepath.Join(directory, name), 0700))
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return &lab{cfg: config{Directory: directory, Listen: "127.0.0.1:39181", OrgID: "org", RunnerID: "site", StateNamespace: "state-only", Kubeconfig: "/site/kubeconfig"}, key: key, apiToken: "fixture-private-adapter-credential"}
}
func TestSubmissionFreezesSignedRolloutAcrossAdapterRestart(t *testing.T) {
	l := testLab(t)
	d := draft{EnvironmentID: "env", Value: "reviewed", ApprovalMode: "local"}
	first, err := l.prepare(d, "stable-key")
	require.NoError(t, err)
	restarted := &lab{cfg: l.cfg, key: l.key}
	replay, err := restarted.prepare(d, "stable-key")
	require.NoError(t, err)
	require.Equal(t, first, replay)
	var signed rollout.SignedCommand
	require.NoError(t, json.Unmarshal(replay.Envelope.Payload, &signed))
	signature, err := base64.StdEncoding.DecodeString(signed.Signature)
	require.NoError(t, err)
	require.True(t, ed25519.Verify(l.key.Public().(ed25519.PublicKey), signed.Payload, signature))
	var command rollout.Command
	require.NoError(t, json.Unmarshal(signed.Payload, &command))
	require.Contains(t, command.Request.Configuration, `backend "kubernetes"`)
	d.Value = "changed"
	_, err = restarted.prepare(d, "stable-key")
	require.Error(t, err)
	pending, err := restarted.read(first.Initial.ID)
	require.NoError(t, err)
	require.Equal(t, "submitted", pending.Phase)
}
func TestCapabilityDisplayAndSubmissionUseBothRequirementSets(t *testing.T) {
	l := testLab(t)
	tool := rolloutexec.Tool{ID: "check", Name: "Check", Phase: "plan_check"}
	tool.Definition.Capabilities = []string{"network.central"}
	l.cfg.Tools = []rolloutexec.Tool{tool}
	require.NotEmpty(t, l.incompatible(tool))
	require.Error(t, l.validateDraft(draft{Checks: []string{"check"}}))
	l.cfg.Capabilities = []string{"network.central"}
	require.Empty(t, l.incompatible(tool))
	require.NoError(t, l.validateDraft(draft{Checks: []string{"check"}}))
}
func TestImportRefusalDoesNotOverwriteExecutionOrChangedPayload(t *testing.T) {
	l := testLab(t)
	saved, err := l.prepare(draft{EnvironmentID: "env", Value: "marker", ApprovalMode: "local"}, uuid.NewString())
	require.NoError(t, err)
	altered := saved.Envelope
	altered.Payload = []byte(`{}`)
	require.NoError(t, l.recordRefusal(altered, "signature_refused"))
	pending, err := l.read(saved.Initial.ID)
	require.NoError(t, err)
	require.Equal(t, "submitted", pending.Phase)
	require.NoError(t, l.recordRefusal(saved.Envelope, "catalog_refused"))
	refused, err := l.read(saved.Initial.ID)
	require.NoError(t, err)
	require.Equal(t, "refused", refused.Phase)
	require.Equal(t, "catalog_refused", refused.ErrorCode)
	refused.Phase = "completed"
	refused.Revision = 7
	require.NoError(t, atomicJSON(filepath.Join(l.cfg.Directory, "projections", refused.ID+".json"), refused))
	require.NoError(t, l.recordRefusal(saved.Envelope, "signature_refused"))
	final, err := l.read(saved.Initial.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", final.Phase)
}
func TestLoopbackFixtureRejectsCrossSiteMutation(t *testing.T) {
	l := testLab(t)
	request := httptest.NewRequest(http.MethodPost, "/api/runner-rollouts", bytes.NewBufferString(`{}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://untrusted.example")
	request.Header.Set("Authorization", "Bearer "+l.apiToken)
	request.Header.Set("X-Rollout-Actor", uuid.NewString())
	response := httptest.NewRecorder()
	l.handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusForbidden, response.Code)
}

func TestIntegratedAdapterRequiresPrivateCredentialAndAuthenticatedActor(t *testing.T) {
	l := testLab(t)
	l.apiToken = "fixture-private-adapter-credential"
	for _, test := range []struct {
		token, actor string
		status       int
	}{
		{"", "", http.StatusUnauthorized},
		{l.apiToken, "", http.StatusUnauthorized},
		{l.apiToken, "ffffffff-ffff-ffff-ffff-ffffffffffff", http.StatusUnauthorized},
		{l.apiToken, uuid.NewString(), http.StatusOK},
	} {
		request := httptest.NewRequest(http.MethodGet, "/api/runner-rollouts", nil)
		request.Header.Set("Authorization", "Bearer "+test.token)
		request.Header.Set("X-Rollout-Actor", test.actor)
		response := httptest.NewRecorder()
		l.handler().ServeHTTP(response, request)
		require.Equal(t, test.status, response.Code)
	}
}

func TestIntegratedDecisionForwardsAuthenticatedActorToSite(t *testing.T) {
	actor := uuid.NewString()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, actor, body["actor"])
		require.Equal(t, "Bearer local-secret", r.Header.Get("Authorization"))
		reply(w, http.StatusOK, map[string]string{"phase": "approved"})
	}))
	defer site.Close()
	l := testLab(t)
	l.apiToken = "private-adapter-credential"
	l.token = "local-secret"
	l.cfg.OperatorURL = site.URL
	request := httptest.NewRequest(http.MethodPost, "/api/runner-rollouts/"+uuid.NewString()+"/approve", bytes.NewBufferString(`{"subject":"exact"}`))
	request.Header.Set("Authorization", "Bearer "+l.apiToken)
	request.Header.Set("X-Rollout-Actor", actor)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	l.handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	require.True(t, validEnvironment(uuid.NewString()))
	require.False(t, validEnvironment(uuid.Nil.String()))
}
