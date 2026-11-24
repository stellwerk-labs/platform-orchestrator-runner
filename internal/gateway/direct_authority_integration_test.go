//go:build integration

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type directAuthorityPeer struct {
	URL, Org, Runner, Environment, Legacy, Native, Malformed, Salt, Nonce string
	RequestsFile                                                          string `json:"requests_file"`
}

type directAuthorityBackend struct {
	Backend
	gets, puts, deletes, publishes atomic.Int32
}

func (b *directAuthorityBackend) GetBundle(ctx context.Context, key string) (io.ReadCloser, error) {
	b.gets.Add(1)
	return b.Backend.GetBundle(ctx, key)
}
func (b *directAuthorityBackend) PutLog(ctx context.Context, key string, body io.Reader) (StoredObject, error) {
	b.puts.Add(1)
	return b.Backend.PutLog(ctx, key, body)
}
func (b *directAuthorityBackend) DeleteLog(ctx context.Context, key string) error {
	b.deletes.Add(1)
	return b.Backend.DeleteLog(ctx, key)
}
func (b *directAuthorityBackend) Publish(ctx context.Context, message hmessaging.Message) error {
	b.publishes.Add(1)
	return b.Backend.Publish(ctx, message)
}

type directAuthorityResolver struct {
	PublicKeyResolver
	calls atomic.Int32
}

func (r *directAuthorityResolver) Resolve(ctx context.Context, org, runner string) (RunnerAuthority, error) {
	r.calls.Add(1)
	return r.PublicKeyResolver.Resolve(ctx, org, runner)
}

func directAuthorityFixture(t *testing.T, protected bool, authorityURL string) (*nativeGatewayFixture, directAuthorityPeer, *directAuthorityBackend, *directAuthorityResolver) {
	t.Helper()
	require.Equal(t, "1", os.Getenv("GATEWAY_DIRECT_ISOLATED"), "requires an owned DP/PostgreSQL/NATS qualification")
	path := os.Getenv("GATEWAY_DIRECT_DP_PEER_FILE")
	require.NotEmpty(t, path)
	info, err := os.Lstat(path)
	require.NoError(t, err)
	require.True(t, info.Mode().IsRegular())
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	require.Less(t, info.Size(), int64(4096))
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	var peer directAuthorityPeer
	require.NoError(t, json.Unmarshal(body, &peer))
	require.Equal(t, os.Getenv("GATEWAY_DIRECT_DP_PEER_NONCE"), peer.Nonce)
	f := newNativeGatewayFixture(t)
	f.org = peer.Org
	f.mu.Lock()
	f.records[peer.Runner] = map[string]any{"uuid": uuid.NewString(), "runner_configuration": map[string]any{"type": "kubernetes-agent", "key": string(f.legacyPublic)}}
	f.mu.Unlock()
	resolver := &directAuthorityResolver{PublicKeyResolver: f.server.publicKeys}
	backend := &directAuthorityBackend{Backend: f.server.backend}
	cfg := ServerConfig{Backend: backend, PublicKeys: resolver, ReceiptKey: make([]byte, 32), RunnerTokenSalt: peer.Salt,
		RetainedPlansEnabled: true, RetainedPlanDPURL: f.server.retainedPlanDPURL, FetchWait: 100 * time.Millisecond}
	if authorityURL == "" {
		authorityURL = peer.URL
	}
	profile := map[string]any{"DeploymentAuthorityDPURL": authorityURL}
	if protected {
		cfg.PublicKeys = &directAuthorityResolver{PublicKeyResolver: StaticPublicKeyResolver{OrganizationID: peer.Org, RunnerID: "native-a", PublicKey: f.public[0], RunnerUUID: f.uuids[0]}}
		resolver = cfg.PublicKeys.(*directAuthorityResolver)
		cfg.RetainedPlansEnabled, cfg.RetainedPlanDPURL = false, ""
		profile = map[string]any{"DisableDirectDeployments": true}
	}
	// Fixed JSON also lets this same real-effect fixture run against signed old
	// ServerConfig, which ignores the added opt-in fields instead of failing compilation.
	settings, err := json.Marshal(profile)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(settings, &cfg))
	f.server, err = NewServer(cfg)
	require.NoError(t, err)
	front := httptest.NewTLSServer(f.server.Handler())
	t.Cleanup(front.Close)
	f.url, f.http = front.URL, front.Client()
	return f, peer, backend, resolver
}

func TestGatewayDirectAuthorityRefusesNativeLegacyAliasesBeforeObjects(t *testing.T) {
	f, peer, backend, resolver := directAuthorityFixture(t, false, "")
	bundles, err := f.js.ObjectStore(runnerBundlesBucket)
	require.NoError(t, err)
	logs, err := f.js.ObjectStore(runnerLogsBucket)
	require.NoError(t, err)
	for _, deployment := range []string{peer.Native, peer.Malformed} {
		sentinel := []byte("owned-direct-authority-sentinel")
		_, err = bundles.PutBytes(peer.Org+"/"+deployment, sentinel)
		require.NoError(t, err)
		before, err := logs.PutBytes(peer.Environment+"/"+deployment, sentinel)
		require.NoError(t, err)
		stream, err := f.js.StreamInfo(hmessaging.RunnerEventsStreamName)
		require.NoError(t, err)
		token := deploymentToken(peer.Salt, peer.Org, deployment)
		for _, endpoint := range []struct{ method, suffix, body string }{
			{http.MethodGet, "/deployments/" + deployment + "/bundle", ""},
			{http.MethodPost, "/deployments/" + deployment + "/results", "{}"},
			{http.MethodPut, "/environments/" + peer.Environment + "/deployments/" + deployment + "/logs", "overwrite"},
			{http.MethodGet, "/deployments/" + deployment + "/retained-plan-key", ""},
		} {
			response := f.request(t, endpoint.method, peer.Runner, endpoint.suffix, []byte(endpoint.body), nil, "", token, nil)
			assert.Equal(t, http.StatusForbidden, response.StatusCode)
			_ = response.Body.Close()
		}
		after, err := logs.GetInfo(peer.Environment + "/" + deployment)
		require.NoError(t, err)
		assert.Equal(t, before.NUID, after.NUID, "refusal must not replace the actual stored object")
		stored, err := logs.GetBytes(peer.Environment + "/" + deployment)
		require.NoError(t, err)
		assert.Equal(t, sentinel, stored)
		end, err := f.js.StreamInfo(hmessaging.RunnerEventsStreamName)
		require.NoError(t, err)
		assert.Equal(t, stream.State.LastSeq, end.State.LastSeq, "refusal must not publish a broker event")
	}
	assert.Zero(t, backend.gets.Load(), "no actual bundle Get is authorized")
	assert.Zero(t, backend.puts.Load(), "no actual log Put is authorized")
	assert.Zero(t, backend.deletes.Load())
	assert.Zero(t, backend.publishes.Load())
	assert.Zero(t, f.keyRequests.Load())
	assert.Zero(t, resolver.calls.Load(), "mutable registration cannot classify stored authority")
}

func TestGatewayDirectAuthorityRejectsWrongEnvironmentBeforeOverwrite(t *testing.T) {
	f, peer, backend, resolver := directAuthorityFixture(t, false, "")
	logs, err := f.js.ObjectStore(runnerLogsBucket)
	require.NoError(t, err)
	foreign := uuid.NewString()
	key := foreign + "/" + peer.Legacy
	sentinel := []byte("owned-foreign-environment-log")
	before, err := logs.PutBytes(key, sentinel)
	require.NoError(t, err)
	stream, err := f.js.StreamInfo(hmessaging.RunnerEventsStreamName)
	require.NoError(t, err)
	response := f.request(t, http.MethodPut, peer.Runner, "/environments/"+foreign+"/deployments/"+peer.Legacy+"/logs", []byte("overwrite"), nil, "", deploymentToken(peer.Salt, peer.Org, peer.Legacy), nil)
	assert.Equal(t, http.StatusForbidden, response.StatusCode)
	after, err := logs.GetInfo(key)
	require.NoError(t, err)
	assert.Equal(t, before.NUID, after.NUID)
	stored, err := logs.GetBytes(key)
	require.NoError(t, err)
	assert.Equal(t, sentinel, stored)
	end, err := f.js.StreamInfo(hmessaging.RunnerEventsStreamName)
	require.NoError(t, err)
	assert.Equal(t, stream.State.LastSeq, end.State.LastSeq)
	assert.Zero(t, backend.puts.Load())
	assert.Zero(t, backend.deletes.Load())
	assert.Zero(t, backend.publishes.Load())
	assert.Zero(t, resolver.calls.Load())
}

func TestGatewayDirectAuthorityPreservesStoredLegacyAccess(t *testing.T) {
	f, peer, backend, resolver := directAuthorityFixture(t, false, "")
	bundles, err := f.js.ObjectStore(runnerBundlesBucket)
	require.NoError(t, err)
	sentinel := []byte("owned-legacy-bundle")
	_, err = bundles.PutBytes(peer.Org+"/"+peer.Legacy, sentinel)
	require.NoError(t, err)
	token := deploymentToken(peer.Salt, peer.Org, peer.Legacy)
	response := f.request(t, http.MethodGet, peer.Runner, "/deployments/"+peer.Legacy+"/bundle", nil, nil, "", token, nil)
	require.Equal(t, http.StatusOK, response.StatusCode)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, sentinel, body)
	response = f.request(t, http.MethodPost, peer.Runner, "/deployments/"+peer.Legacy+"/results", []byte("{}"), nil, "", token, nil)
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	response = f.request(t, http.MethodPut, peer.Runner, "/environments/"+peer.Environment+"/deployments/"+peer.Legacy+"/logs", sentinel, nil, "", token, nil)
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	logs, err := f.js.ObjectStore(runnerLogsBucket)
	require.NoError(t, err)
	stored, err := logs.GetBytes(peer.Environment + "/" + peer.Legacy)
	require.NoError(t, err)
	require.Equal(t, sentinel, stored)
	response = f.request(t, http.MethodGet, peer.Runner, "/deployments/"+peer.Legacy+"/retained-plan-key", nil, nil, "", token, nil)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.EqualValues(t, 1, f.keyRequests.Load(), "this key peer proves forwarding, not real Vault key delivery")
	require.EqualValues(t, 1, backend.gets.Load())
	require.EqualValues(t, 1, backend.puts.Load())
	require.EqualValues(t, 2, backend.publishes.Load())
	require.Zero(t, resolver.calls.Load())
}

func TestGatewayDirectAuthorityStaticNativeHasNoCentralRequests(t *testing.T) {
	f, peer, backend, resolver := directAuthorityFixture(t, true, "")
	before, err := os.ReadFile(peer.RequestsFile)
	require.NoError(t, err)
	token := deploymentToken(peer.Salt, peer.Org, peer.Legacy)
	for _, endpoint := range []struct {
		method, suffix string
		status         int
	}{
		{http.MethodGet, "/deployments/" + peer.Legacy + "/bundle", http.StatusForbidden},
		{http.MethodPost, "/deployments/" + peer.Legacy + "/results", http.StatusForbidden},
		{http.MethodPut, "/environments/" + peer.Environment + "/deployments/" + peer.Legacy + "/logs", http.StatusForbidden},
		{http.MethodGet, "/deployments/" + peer.Legacy + "/retained-plan-key", http.StatusNotFound},
	} {
		response := f.request(t, endpoint.method, peer.Runner, endpoint.suffix, []byte("{}"), nil, "", token, nil)
		require.Equal(t, endpoint.status, response.StatusCode)
	}
	after, err := os.ReadFile(peer.RequestsFile)
	require.NoError(t, err)
	require.Equal(t, before, after, "protected direct paths must make zero actual DP HTTP calls")
	require.Zero(t, resolver.calls.Load())
	require.Zero(t, backend.gets.Load())
	require.Zero(t, backend.puts.Load())
	require.Zero(t, backend.deletes.Load())
	require.Zero(t, backend.publishes.Load())
	require.Zero(t, f.keyRequests.Load())
	f.command(t, 0, "protected-agent-command")
}

func TestGatewayDirectAuthorityAuthenticatesBeforeDPAndObjects(t *testing.T) {
	f, peer, backend, resolver := directAuthorityFixture(t, false, "")
	before, err := os.ReadFile(peer.RequestsFile)
	require.NoError(t, err)
	token := deploymentToken(peer.Salt, peer.Org, peer.Legacy)
	for _, endpoint := range []struct{ method, suffix string }{
		{http.MethodGet, "/deployments/" + peer.Legacy + "/bundle"},
		{http.MethodPost, "/deployments/" + peer.Legacy + "/results"},
		{http.MethodPut, "/environments/" + peer.Environment + "/deployments/" + peer.Legacy + "/logs"},
		{http.MethodGet, "/deployments/" + peer.Legacy + "/retained-plan-key"},
	} {
		for _, values := range [][]string{nil, {"invalid"}, {token, token}} {
			headers := http.Header{"X-Deployment-Token": values}
			response := f.request(t, endpoint.method, peer.Runner, endpoint.suffix, []byte("{}"), nil, "", "", headers)
			require.Equal(t, http.StatusUnauthorized, response.StatusCode)
		}
	}
	after, err := os.ReadFile(peer.RequestsFile)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Zero(t, backend.gets.Load())
	require.Zero(t, backend.puts.Load())
	require.Zero(t, backend.deletes.Load())
	require.Zero(t, backend.publishes.Load())
	require.Zero(t, f.keyRequests.Load())
	require.Zero(t, resolver.calls.Load())
}

func TestGatewayDirectAuthorityWireFailureHasNoObjectEffects(t *testing.T) {
	for _, kind := range []string{"unavailable", "malformed", "redirect"} {
		t.Run(kind, func(t *testing.T) {
			var redirected atomic.Int32
			sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
			t.Cleanup(sink.Close)
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch kind {
				case "unavailable":
					w.WriteHeader(http.StatusServiceUnavailable)
				case "malformed":
					_, _ = w.Write([]byte("{"))
				case "redirect":
					http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
				}
			}))
			t.Cleanup(peer.Close)
			f, authority, backend, resolver := directAuthorityFixture(t, false, peer.URL)
			token := deploymentToken(authority.Salt, authority.Org, authority.Legacy)
			for _, endpoint := range []struct{ method, suffix string }{
				{http.MethodGet, "/deployments/" + authority.Legacy + "/bundle"},
				{http.MethodPost, "/deployments/" + authority.Legacy + "/results"},
				{http.MethodPut, "/environments/" + authority.Environment + "/deployments/" + authority.Legacy + "/logs"},
				{http.MethodGet, "/deployments/" + authority.Legacy + "/retained-plan-key"},
			} {
				response := f.request(t, endpoint.method, authority.Runner, endpoint.suffix, []byte("{}"), nil, "", token, nil)
				require.Equal(t, http.StatusForbidden, response.StatusCode)
			}
			require.Zero(t, backend.gets.Load())
			require.Zero(t, backend.puts.Load())
			require.Zero(t, backend.deletes.Load())
			require.Zero(t, backend.publishes.Load())
			require.Zero(t, f.keyRequests.Load())
			require.Zero(t, resolver.calls.Load())
			require.Zero(t, redirected.Load())
		})
	}
}
