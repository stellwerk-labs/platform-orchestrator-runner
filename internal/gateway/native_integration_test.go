//go:build integration

package gateway

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/golib/hnats"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gatewayapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/natstransport"
	"github.com/stretchr/testify/require"
)

type nativeGatewayFixture struct {
	connection    *nats.Conn
	js            nats.JetStreamContext
	org           string
	uuids         [2]string
	private       [2][]byte
	public        [2][]byte
	legacyPrivate []byte
	legacyPublic  []byte
	central       ed25519.PrivateKey
	records       map[string]map[string]any
	mu            sync.RWMutex
	url           string
	http          *http.Client
	server        *Server
	keyRequests   atomic.Int32
}

func newNativeGatewayFixture(t *testing.T) *nativeGatewayFixture {
	t.Helper()
	require.Equal(t, "1", os.Getenv("AGENT_DELIVERY_ISOLATED"), "native gateway qualification requires its owned broker")
	address := os.Getenv("AGENT_DELIVERY_NATS_URL")
	require.NotEmpty(t, address)
	connection, err := nats.Connect(address, nats.Timeout(2*time.Second))
	require.NoError(t, err)
	t.Cleanup(connection.Close)
	modern, err := hnats.NewJetStream(connection)
	require.NoError(t, err)
	require.NoError(t, hnats.EnsureStandardStreams(t.Context(), modern, 1))
	js, err := connection.JetStream()
	require.NoError(t, err)
	for _, bucket := range []string{runnerBundlesBucket, runnerLogsBucket} {
		if _, err = js.ObjectStore(bucket); err != nil {
			_, err = js.CreateObjectStore(&nats.ObjectStoreConfig{Bucket: bucket, Storage: nats.FileStorage})
			require.NoError(t, err)
		}
	}
	f := &nativeGatewayFixture{connection: connection, js: js, org: "native-" + uuid.NewString(), uuids: [2]string{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}, records: map[string]map[string]any{}}
	_, f.central, err = ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	for i, name := range []string{"native-a", "native-b"} {
		f.private[i], f.public[i] = gatewayKeyPair(t)
		f.records[name] = map[string]any{"uuid": f.uuids[i], "runner_configuration": map[string]any{"type": "native-agent", "key": string(f.public[i]), "native_execution": nil}}
	}
	f.legacyPrivate, f.legacyPublic = gatewayKeyPair(t)
	f.records[f.uuids[0]] = map[string]any{"uuid": uuid.NewString(), "runner_configuration": map[string]any{"type": "kubernetes-agent", "key": string(f.legacyPublic)}}
	// This is a real HTTP parser/cache boundary with Core-shaped responses, not a Core enrollment proof.
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := path.Base(r.URL.Path)
		f.mu.RLock()
		defer f.mu.RUnlock()
		record, ok := f.records[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(record)
	}))
	t.Cleanup(core.Close)
	resolver, err := NewControlPlanePublicKeyResolver(core.URL, core.Client(), 30*time.Millisecond)
	require.NoError(t, err)
	backend, err := NewNATSBackend(connection)
	require.NoError(t, err)
	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.keyRequests.Add(1)
		_, _ = w.Write([]byte(`{"key":"owned-fixture"}`))
	}))
	t.Cleanup(keyServer.Close)
	f.server, err = NewServer(ServerConfig{Backend: backend, PublicKeys: resolver, ReceiptKey: bytes.Repeat([]byte{41}, 32), RunnerTokenSalt: "owned-token-salt", FetchWait: 100 * time.Millisecond, RetainedPlansEnabled: true, RetainedPlanDPURL: keyServer.URL})
	require.NoError(t, err)
	gateway := httptest.NewTLSServer(f.server.Handler())
	t.Cleanup(gateway.Close)
	f.url, f.http = gateway.URL, gateway.Client()
	return f
}

func (f *nativeGatewayFixture) request(t *testing.T, method, slug, suffix string, body []byte, private []byte, runnerUUID, token string, headers http.Header) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, f.url+"/v1/orgs/"+f.org+"/runners/"+slug+suffix, bytes.NewReader(body))
	require.NoError(t, err)
	if private != nil {
		jwt, signErr := gatewayapi.SignAgentToken(private, f.org, slug, runnerUUID, time.Now())
		require.NoError(t, signErr)
		request.Header.Set("Authorization", "Bearer "+jwt)
	}
	if token != "" {
		request.Header.Set(gatewayapi.DeploymentTokenHeader, token)
	}
	for name, values := range headers {
		request.Header[name] = values
	}
	response, err := f.http.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func (f *nativeGatewayFixture) command(t *testing.T, i int, id string) gatewayapi.CommandResponse {
	t.Helper()
	token, err := hmessaging.NativeRunnerToken(f.uuids[i])
	require.NoError(t, err)
	deployment := uuid.NewString()
	payload, err := json.Marshal(map[string]any{"version": "stellwerk.pipeline/v2", "id": deployment, "org_id": f.org, "runner_id": fmt.Sprintf("native-%c", 'a'+i), "runner_uuid": f.uuids[i], "request": map[string]any{"id": deployment, "runner_uuid": f.uuids[i]}})
	require.NoError(t, err)
	signed, err := json.Marshal(map[string]any{"payload": json.RawMessage(payload), "signature": base64.StdEncoding.EncodeToString(ed25519.Sign(f.central, payload))})
	require.NoError(t, err)
	envelope := hmessaging.CommandEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1, CommandID: id, OrganizationID: f.org, RunnerID: token, DeploymentID: deployment, Type: "execute-rollout-v1", CreatedAt: time.Now(), Payload: signed}
	data, err := json.Marshal(envelope)
	require.NoError(t, err)
	subject, err := hmessaging.RunnerCommandSubject(f.org, token)
	require.NoError(t, err)
	messageID, err := hmessaging.NativeMessageID(f.org, token, "commands", envelope.Type, id)
	require.NoError(t, err)
	publisher, err := natstransport.NewPublisher(f.connection)
	require.NoError(t, err)
	require.NoError(t, publisher.Publish(t.Context(), hmessaging.Message{ID: messageID, Subject: subject, Data: data, CreatedAt: envelope.CreatedAt}))
	response := f.request(t, http.MethodPost, fmt.Sprintf("native-%c", 'a'+i), "/commands/next", nil, f.private[i], f.uuids[i], "", nil)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var delivery gatewayapi.CommandResponse
	require.NoError(t, json.NewDecoder(response.Body).Decode(&delivery))
	return delivery
}

func (f *nativeGatewayFixture) pending(t *testing.T, i int) int {
	t.Helper()
	token, err := hmessaging.NativeRunnerToken(f.uuids[i])
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(f.org + "\x00" + token))
	info, err := f.js.ConsumerInfo(hmessaging.RunnerCommandsStreamName, "runner-"+hex.EncodeToString(digest[:12]))
	require.NoError(t, err)
	return info.NumAckPending
}

func TestNativeGatewaySitesCannotBorrowCommandReceipts(t *testing.T) {
	f := newNativeGatewayFixture(t)
	a := f.command(t, 0, "same-inner-command")
	b := f.command(t, 1, "same-inner-command")
	var envelopeA, envelopeB hmessaging.CommandEnvelope
	require.NoError(t, json.Unmarshal(a.Command, &envelopeA))
	require.NoError(t, json.Unmarshal(b.Command, &envelopeB))
	require.NotEqual(t, envelopeA.RunnerID, envelopeB.RunnerID)
	for _, action := range []string{"ack", "retry", "reject"} {
		body, err := json.Marshal(map[string]any{"receipt": b.Receipt, "reason": "owned-test"})
		require.NoError(t, err)
		// Ack/retry have strict request bodies, so only their actual contract fields are sent.
		if action != "reject" {
			body, err = json.Marshal(map[string]any{"receipt": b.Receipt})
			require.NoError(t, err)
		}
		response := f.request(t, http.MethodPost, "native-a", "/commands/same-inner-command/"+action, body, f.private[0], f.uuids[0], "", nil)
		require.Equal(t, http.StatusBadRequest, response.StatusCode)
		require.Equal(t, 1, f.pending(t, 1), "foreign receipt changed B's broker custody")
	}
	retry, err := json.Marshal(gatewayapi.RetryRequest{Receipt: b.Receipt})
	require.NoError(t, err)
	response := f.request(t, http.MethodPost, "native-b", "/commands/same-inner-command/retry", retry, f.private[1], f.uuids[1], "", nil)
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	response = f.request(t, http.MethodPost, "native-b", "/commands/next", nil, f.private[1], f.uuids[1], "", nil)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var replay gatewayapi.CommandResponse
	require.NoError(t, json.NewDecoder(response.Body).Decode(&replay))
	require.Greater(t, replay.Attempt, b.Attempt)
	for i, delivery := range []gatewayapi.CommandResponse{a, replay} {
		body, err := json.Marshal(gatewayapi.ReceiptRequest{Receipt: delivery.Receipt})
		require.NoError(t, err)
		response = f.request(t, http.MethodPost, fmt.Sprintf("native-%c", 'a'+i), "/commands/same-inner-command/ack", body, f.private[i], f.uuids[i], "", nil)
		require.Equal(t, http.StatusNoContent, response.StatusCode)
	}
	require.Eventually(t, func() bool { return f.pending(t, 0) == 0 && f.pending(t, 1) == 0 }, 2*time.Second, 20*time.Millisecond)
	rejected := f.command(t, 0, "own-rejection")
	body, err := json.Marshal(gatewayapi.RejectRequest{Receipt: rejected.Receipt, Reason: "owned-positive-refusal"})
	require.NoError(t, err)
	response = f.request(t, http.MethodPost, "native-a", "/commands/own-rejection/reject", body, f.private[0], f.uuids[0], "", nil)
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	require.Eventually(t, func() bool { return f.pending(t, 0) == 0 }, time.Second, 10*time.Millisecond)

}

func TestNativeGatewayRejectUsesDeadLetterPublishMetadata(t *testing.T) {
	f := newNativeGatewayFixture(t)
	route, err := hmessaging.NativeRunnerToken(f.uuids[0])
	require.NoError(t, err)
	now := time.Now().UTC()
	envelope := hmessaging.CommandEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1, CommandID: "reject-publish-metadata",
		OrganizationID: f.org, RunnerID: route, DeploymentID: uuid.NewString(), Type: "execute-rollout-v1",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), Payload: json.RawMessage(`{"owned":"immutable-command"}`)}
	data, err := json.Marshal(envelope)
	require.NoError(t, err)
	subject, err := hmessaging.RunnerCommandSubject(f.org, route)
	require.NoError(t, err)
	messageID, err := hmessaging.NativeMessageID(f.org, route, "commands", envelope.Type, envelope.CommandID)
	require.NoError(t, err)
	message := nats.NewMsg(subject)
	message.Data = data
	message.Header.Set(nats.ExpectedStreamHdr, hmessaging.RunnerCommandsStreamName)
	message.Header.Set(nats.MsgTTLHdr, "1h")
	message.Header.Set(hmessaging.HeaderExpiresAt, envelope.ExpiresAt.Format(time.RFC3339Nano))
	message.Header.Set("pO-eXpIrEs-At", envelope.ExpiresAt.Format(time.RFC3339Nano))
	message.Header.Set("nAtS-Copied-Control", "original-stream-metadata")
	message.Header.Set("Po-Application", "keep-application-metadata")
	message.Header.Set("Traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")
	_, err = f.js.PublishMsg(message, nats.MsgId(messageID))
	require.NoError(t, err)
	before, err := f.js.GetLastMsg(hmessaging.RunnerCommandsStreamName, subject)
	require.NoError(t, err)
	response := f.request(t, http.MethodPost, "native-a", "/commands/next", nil, f.private[0], f.uuids[0], "", nil)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var delivery gatewayapi.CommandResponse
	require.NoError(t, json.NewDecoder(response.Body).Decode(&delivery))
	body, err := json.Marshal(gatewayapi.RejectRequest{Receipt: delivery.Receipt, Reason: "capability_refused"})
	require.NoError(t, err)
	response = f.request(t, http.MethodPost, "native-a", "/commands/"+envelope.CommandID+"/reject", body, f.private[0], f.uuids[0], "", nil)
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	dlqSubject, err := hmessaging.DeadLetterSubject(subject)
	require.NoError(t, err)
	rejected, err := f.js.GetLastMsg(hmessaging.DeadLettersStreamName, dlqSubject)
	require.NoError(t, err)
	require.Equal(t, before.Data, rejected.Data)
	require.Equal(t, before.Header.Get("Po-Application"), rejected.Header.Get("Po-Application"))
	require.Equal(t, before.Header.Get("Traceparent"), rejected.Header.Get("Traceparent"))
	require.Equal(t, messageID+":gateway-dlq", rejected.Header.Get(nats.MsgIdHdr))
	require.Equal(t, "capability_refused", rejected.Header.Get("Po-Dead-Letter-Reason"))
	require.Empty(t, rejected.Header.Get(nats.ExpectedStreamHdr))
	require.Empty(t, rejected.Header.Get(nats.MsgTTLHdr))
	require.Empty(t, hmessaging.Header(rejected.Header).Get(hmessaging.HeaderExpiresAt))
	require.NotContains(t, rejected.Header, "nAtS-Copied-Control")
	require.Eventually(t, func() bool { return f.pending(t, 0) == 0 }, time.Second, 10*time.Millisecond)
}

func (f *nativeGatewayFixture) event(t *testing.T, slug, runnerUUID, transportID, id, kind string, private []byte, headers http.Header) *http.Response {
	t.Helper()
	deployment := uuid.NewSHA1(uuid.NameSpaceOID, []byte(id)).String()
	payload := json.RawMessage(`{}`)
	if strings.HasPrefix(transportID, hmessaging.NativeRunnerTokenPrefix) {
		if kind == "rollout-progress" {
			payload = json.RawMessage(fmt.Sprintf(`{"progress":{"id":%q,"revision":1}}`, deployment))
			id = deployment + ":1"
		} else {
			id = deployment + ":deployment-result"
		}
	}
	data, err := json.Marshal(hmessaging.EventEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1, OrganizationID: f.org, RunnerID: transportID, EventID: id, Type: kind, DeploymentID: deployment, CreatedAt: time.Now(), Payload: payload})
	require.NoError(t, err)
	return f.request(t, http.MethodPost, slug, "/events", data, private, runnerUUID, "", headers)
}

func TestNativeGatewayScopesEventDedupAndRefusesLegacyNamespace(t *testing.T) {
	f := newNativeGatewayFixture(t)
	a, err := hmessaging.NativeRunnerToken(f.uuids[0])
	require.NoError(t, err)
	b, err := hmessaging.NativeRunnerToken(f.uuids[1])
	require.NoError(t, err)
	subjectA, err := hmessaging.RunnerEventSubject(f.org, a, "deployment-result")
	require.NoError(t, err)
	subjectB, err := hmessaging.RunnerEventSubject(f.org, b, "deployment-result")
	require.NoError(t, err)
	response := f.event(t, "native-a", f.uuids[0], a, "same-event", "deployment-result", f.private[0], nil)
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	first, err := f.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subjectA)
	require.NoError(t, err)
	response = f.event(t, "native-b", f.uuids[1], b, "same-event", "deployment-result", f.private[1], nil)
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	second, err := f.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subjectB)
	require.NoError(t, err)
	require.NotEqual(t, first.Sequence, second.Sequence)
	require.NotEqual(t, first.Header.Get(hmessaging.HeaderMessageID), second.Header.Get(hmessaging.HeaderMessageID))
	response = f.event(t, "native-a", f.uuids[0], a, "same-event", "deployment-result", f.private[0], nil)
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	again, err := f.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subjectA)
	require.NoError(t, err)
	require.Equal(t, first.Sequence, again.Sequence, "same-site retry created another broker event")
	nativeID, err := hmessaging.NativeMessageID(f.org, b, "events", "deployment-result", uuid.NewSHA1(uuid.NameSpaceOID, []byte("after-legacy-attempt")).String()+":deployment-result")
	require.NoError(t, err)
	for _, attempt := range []struct {
		id      string
		headers http.Header
	}{{nativeID, nil}, {"legacy-id", http.Header{hmessaging.HeaderMessageID: []string{nativeID}}}} {
		response = f.event(t, f.uuids[0], "", f.uuids[0], attempt.id, "deployment-result", f.legacyPrivate, attempt.headers)
		require.Equal(t, http.StatusBadRequest, response.StatusCode)
	}
	response = f.event(t, "native-b", f.uuids[1], b, "after-legacy-attempt", "deployment-result", f.private[1], nil)
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	after, err := f.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subjectB)
	require.NoError(t, err)
	require.Equal(t, nativeID, after.Header.Get(hmessaging.HeaderMessageID))
	response = f.event(t, f.uuids[0], "", f.uuids[0], "supported-legacy", "deployment-result", f.legacyPrivate, nil)
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	for _, kind := range []string{"log-object-ready", "unknown-event"} {
		response = f.event(t, "native-a", f.uuids[0], a, "rejected", kind, f.private[0], nil)
		require.Equal(t, http.StatusBadRequest, response.StatusCode)
	}
	response = f.event(t, "native-a", f.uuids[0], a, "progress", "rollout-progress", f.private[0], nil)
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	response = f.event(t, "native-a", f.uuids[0], b, "foreign-envelope", "deployment-result", f.private[0], nil)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	response = f.event(t, "native-b", f.uuids[0], b, "wrong-site", "deployment-result", f.private[0], nil)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	foreignID, err := hmessaging.NativeMessageID(f.org, b, "events", "deployment-result", uuid.NewSHA1(uuid.NameSpaceOID, []byte("foreign-header")).String()+":deployment-result")
	require.NoError(t, err)
	response = f.event(t, "native-a", f.uuids[0], a, "own-header", "deployment-result", f.private[0], http.Header{hmessaging.HeaderMessageID: []string{foreignID}})
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	response = f.event(t, "native-b", f.uuids[1], b, "foreign-header", "deployment-result", f.private[1], nil)
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	foreign, err := f.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subjectB)
	require.NoError(t, err)
	require.Equal(t, foreignID, foreign.Header.Get(hmessaging.HeaderMessageID), "A header must not suppress B delivery")
	response = f.event(t, "native-a", "", a, "missing-uuid", "deployment-result", f.private[0], nil)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
}

func TestNativeGatewayDirectURIRefusalPreservesAllLegacyJobs(t *testing.T) {
	f := newNativeGatewayFixture(t)
	bundles, err := f.js.ObjectStore(runnerBundlesBucket)
	require.NoError(t, err)
	logs, err := f.js.ObjectStore(runnerLogsBucket)
	require.NoError(t, err)
	deployment, environment := uuid.NewString(), uuid.NewString()
	token := deploymentToken("owned-token-salt", f.org, deployment)
	sentinel := []byte("unchanged-private-object")
	_, err = bundles.PutBytes(f.org+"/"+deployment, sentinel)
	require.NoError(t, err)
	_, err = logs.PutBytes(environment+"/"+deployment, sentinel)
	require.NoError(t, err)
	nativeToken, err := hmessaging.NativeRunnerToken(f.uuids[0])
	require.NoError(t, err)
	for _, slug := range []string{"native-a", nativeToken} {
		for _, endpoint := range []struct {
			method, path string
			body         []byte
		}{{http.MethodGet, "/deployments/" + deployment + "/bundle", nil}, {http.MethodPost, "/deployments/" + deployment + "/results", []byte(`{}`)}, {http.MethodPut, "/environments/" + environment + "/deployments/" + deployment + "/logs", []byte("replace")}, {http.MethodGet, "/deployments/" + deployment + "/retained-plan-key", nil}} {
			response := f.request(t, endpoint.method, slug, endpoint.path, endpoint.body, nil, "", token, nil)
			require.Contains(t, []int{http.StatusForbidden, http.StatusBadRequest}, response.StatusCode)
		}
	}
	stored, err := logs.GetBytes(environment + "/" + deployment)
	require.NoError(t, err)
	require.Equal(t, sentinel, stored)
	require.Zero(t, f.keyRequests.Load())
	for _, slug := range []string{"native-a", nativeToken} {
		subject, subjectErr := hmessaging.RunnerEventSubject(f.org, slug, "deployment-result")
		require.NoError(t, subjectErr)
		_, subjectErr = f.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
		require.True(t, errors.Is(subjectErr, nats.ErrMsgNotFound), "denied direct result published an event")
	}
	for i, kind := range []string{"kubernetes", "kubernetes-agent", "kubernetes-eks", "kubernetes-gke", "serverless-ecs"} {
		slug := fmt.Sprintf("legacy-%d", i)
		configuration := map[string]any{"type": kind}
		if kind == "kubernetes-agent" {
			configuration["key"] = string(f.legacyPublic)
		}
		f.mu.Lock()
		f.records[slug] = map[string]any{"uuid": uuid.NewString(), "runner_configuration": configuration}
		f.mu.Unlock()
		legacyDeployment := uuid.NewString()
		legacyToken := deploymentToken("owned-token-salt", f.org, legacyDeployment)
		_, err = bundles.PutBytes(f.org+"/"+legacyDeployment, sentinel)
		require.NoError(t, err)
		response := f.request(t, http.MethodGet, slug, "/deployments/"+legacyDeployment+"/bundle", nil, nil, "", legacyToken, nil)
		require.Equal(t, http.StatusOK, response.StatusCode, kind)
		body, readErr := io.ReadAll(response.Body)
		require.NoError(t, readErr)
		require.Equal(t, sentinel, body)
		response = f.request(t, http.MethodPost, slug, "/deployments/"+legacyDeployment+"/results", []byte(`{}`), nil, "", legacyToken, nil)
		require.Equal(t, http.StatusAccepted, response.StatusCode, kind)
	}
	// These URL guards are partial: authoritative frozen-Deployment alias checks belong to D3.
}

func TestNativeGatewayKeyRotationAndDisabledCompletion(t *testing.T) {
	f := newNativeGatewayFixture(t)
	token, err := hmessaging.NativeRunnerToken(f.uuids[0])
	require.NoError(t, err)
	delivery := f.command(t, 0, "before-rotation")
	replacement, public := gatewayKeyPair(t)
	f.mu.Lock()
	f.records["native-a"]["runner_configuration"] = map[string]any{"type": "native-agent", "key": string(public), "native_execution": nil}
	f.mu.Unlock()
	require.Eventually(t, func() bool {
		response := f.event(t, "native-a", f.uuids[0], token, "rotated", "deployment-result", replacement, nil)
		_ = response.Body.Close()
		return response.StatusCode == http.StatusAccepted
	}, 2*time.Second, 25*time.Millisecond)
	response := f.event(t, "native-a", f.uuids[0], token, "old-key", "deployment-result", f.private[0], nil)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	body, err := json.Marshal(gatewayapi.ReceiptRequest{Receipt: delivery.Receipt})
	require.NoError(t, err)
	response = f.request(t, http.MethodPost, "native-a", "/commands/before-rotation/ack", body, replacement, f.uuids[0], "", nil)
	require.Equal(t, http.StatusNoContent, response.StatusCode, "disabled catalogue must retain same-UUID completion custody")
}

func TestNativeGatewayRejectsUnscopedBrokerCommandsBeforeDelivery(t *testing.T) {
	f := newNativeGatewayFixture(t)
	token, err := hmessaging.NativeRunnerToken(f.uuids[0])
	require.NoError(t, err)
	foreign, err := hmessaging.NativeRunnerToken(f.uuids[1])
	require.NoError(t, err)
	subject, err := hmessaging.RunnerCommandSubject(f.org, token)
	require.NoError(t, err)
	publisher, err := natstransport.NewPublisher(f.connection)
	require.NoError(t, err)
	for _, test := range []struct {
		name     string
		protocol string
		runner   string
		scoped   bool
	}{
		{"legacy_message_id", hmessaging.ProtocolVersionV1, token, false}, {"wrong_protocol", "unknown", token, true}, {"foreign_envelope", hmessaging.ProtocolVersionV1, foreign, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			envelope := hmessaging.CommandEnvelope{ProtocolVersion: test.protocol, CommandID: test.name, OrganizationID: f.org, RunnerID: test.runner, DeploymentID: uuid.NewString(), Type: "execute-rollout-v1", CreatedAt: time.Now(), Payload: json.RawMessage(`{}`)}
			body, marshalErr := json.Marshal(envelope)
			require.NoError(t, marshalErr)
			messageID := envelope.CommandID
			if test.scoped {
				messageID, marshalErr = hmessaging.NativeMessageID(f.org, token, "commands", envelope.Type, envelope.CommandID)
				require.NoError(t, marshalErr)
			}
			require.NoError(t, publisher.Publish(t.Context(), hmessaging.Message{ID: messageID, Subject: subject, Data: body, CreatedAt: envelope.CreatedAt}))
			response := f.request(t, http.MethodPost, "native-a", "/commands/next", nil, f.private[0], f.uuids[0], "", nil)
			require.Equal(t, http.StatusNoContent, response.StatusCode)
			require.Eventually(t, func() bool { return f.pending(t, 0) == 0 }, time.Second, 10*time.Millisecond)
		})
	}
	f.command(t, 0, "valid-after-refusals")
}

func TestNativeGatewayRejectsMisboundIdentityBeforeBrokerDedup(t *testing.T) {
	f := newNativeGatewayFixture(t)
	token, err := hmessaging.NativeRunnerToken(f.uuids[0])
	require.NoError(t, err)
	a, b := uuid.NewString(), uuid.NewString()
	for _, kind := range []string{"deployment-result", "rollout-progress"} {
		t.Run(kind, func(t *testing.T) {
			event := hmessaging.EventEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1, OrganizationID: f.org, RunnerID: token, DeploymentID: a, Type: kind, EventID: b + ":deployment-result", CreatedAt: time.Now(), Payload: json.RawMessage(`{}`)}
			if kind == "rollout-progress" {
				event.EventID = b + ":2"
				event.Payload = json.RawMessage(fmt.Sprintf(`{"progress":{"id":%q,"revision":2}}`, a))
			}
			subject, subjectErr := hmessaging.RunnerEventSubject(f.org, token, kind)
			require.NoError(t, subjectErr)
			before, subjectErr := f.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
			require.True(t, errors.Is(subjectErr, nats.ErrMsgNotFound))
			require.Nil(t, before)
			send := func() *http.Response {
				data, marshalErr := json.Marshal(event)
				require.NoError(t, marshalErr)
				return f.request(t, http.MethodPost, "native-a", "/events", data, f.private[0], f.uuids[0], "", nil)
			}
			require.Equal(t, http.StatusBadRequest, send().StatusCode)
			_, subjectErr = f.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
			require.True(t, errors.Is(subjectErr, nats.ErrMsgNotFound), "refusal published and consumed broker identity")
			event.DeploymentID = b
			if kind == "rollout-progress" {
				// A matching envelope/revision cannot borrow another payload identity either.
				require.Equal(t, http.StatusBadRequest, send().StatusCode)
				event.Payload = json.RawMessage(fmt.Sprintf(`{"progress":{"id":%q,"revision":1}}`, b))
				require.Equal(t, http.StatusBadRequest, send().StatusCode)
				event.Payload = json.RawMessage(fmt.Sprintf(`{"progress":{"id":%q,"revision":2}}`, b))
			}
			require.Equal(t, http.StatusAccepted, send().StatusCode)
			stored, subjectErr := f.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
			require.NoError(t, subjectErr)
			expected, identityErr := hmessaging.NativeMessageID(f.org, token, "events", kind, event.EventID)
			require.NoError(t, identityErr)
			require.Equal(t, expected, stored.Header.Get(hmessaging.HeaderMessageID))
			require.Equal(t, http.StatusAccepted, send().StatusCode)
			replay, replayErr := f.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
			require.NoError(t, replayErr)
			require.Equal(t, stored.Sequence, replay.Sequence)
		})
	}
}
