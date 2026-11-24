//go:build integration

package gateway

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
	"github.com/stretchr/testify/require"
)

func TestNativeLogHTTPRequiresAgentScopeAndDurableCipherBeforeReady(t *testing.T) {
	f := newNativeGatewayFixture(t)
	a := nativelogs.Authority{OrganizationID: f.org, EnvironmentID: uuid.NewString(), DeploymentID: uuid.NewString(), CommandDigest: strings.Repeat("a", 64), Target: nativelogs.Target{RunnerID: "native-a", RunnerUUID: f.uuids[0], PipelineVersion: "pipeline/v2", RuntimeVersion: "native/v1", StateBackendType: "local", ToolsDigest: strings.Repeat("b", 64), StateConfigurationDigest: strings.Repeat("c", 64)}, Phase: "apply", ExecutionID: "apply", CaptureRevision: 2}
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	raw := make([]byte, nativelogs.RawLimit)
	// High entropy exercises the actual full-size age/object-store chunk path.
	_, err = io.ReadFull(rand.Reader, raw)
	require.NoError(t, err)
	cipher, err := nativelogs.Encode(nativelogs.Envelope{Version: 1, Authority: a, Status: nativelogs.PendingTransfer, Logs: raw}, identity.Recipient())
	require.NoError(t, err)
	d := nativelogs.Descriptor{Version: 1, Authority: a, Status: nativelogs.PendingTransfer, CiphertextSHA256: nativelogs.Hash(cipher), CiphertextBytes: len(cipher)}
	d.ObjectKey = a.ObjectKey(d.CiphertextSHA256)
	metadata, err := json.Marshal(d)
	require.NoError(t, err)
	header := http.Header{nativelogs.DescriptorHeader: {base64.RawURLEncoding.EncodeToString(metadata)}}
	suffix := "/native-log-artifacts/environments/" + a.EnvironmentID + "/deployments/" + a.DeploymentID
	route, err := hmessaging.NativeRunnerToken(a.Target.RunnerUUID)
	require.NoError(t, err)
	event := hmessaging.EventEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1, EventID: d.EventID(), OrganizationID: f.org, RunnerID: route, DeploymentID: a.DeploymentID, Type: nativelogs.ReadyEventType, CreatedAt: time.Now().UTC(), Payload: metadata}
	ready, err := json.Marshal(event)
	require.NoError(t, err)
	require.Equal(t, 400, f.request(t, "POST", "native-a", "/events", ready, f.private[0], f.uuids[0], "", nil).StatusCode, "ready cannot precede durable artifact")
	require.Equal(t, 401, f.request(t, "PUT", "native-a", suffix, cipher, nil, "", "deployment-token", header).StatusCode)
	require.Equal(t, 400, f.request(t, "PUT", "native-b", suffix, cipher, f.private[1], f.uuids[1], "", header).StatusCode)
	require.Equal(t, 400, f.request(t, "PUT", "native-a", suffix+"-wrong", cipher, f.private[0], f.uuids[0], "", header).StatusCode)
	require.Equal(t, 400, f.request(t, "PUT", "native-a", suffix, cipher[:len(cipher)-1], f.private[0], f.uuids[0], "", header).StatusCode)
	for _, bad := range [][]byte{bytes.Replace(metadata, []byte(`"target":`), []byte(`"Target":`), 1), append(bytes.Clone(metadata), bytes.Repeat([]byte(" "), nativelogs.MetadataLimit)...)} {
		candidate := http.Header{nativelogs.DescriptorHeader: {base64.RawURLEncoding.EncodeToString(bad)}}
		require.Equal(t, 400, f.request(t, "PUT", "native-a", suffix, cipher, f.private[0], f.uuids[0], "", candidate).StatusCode)
	}
	require.Equal(t, 400, f.request(t, "PUT", "native-a", suffix, make([]byte, nativelogs.CiphertextLimit+1), f.private[0], f.uuids[0], "", header).StatusCode)
	store, err := f.js.ObjectStore(nativelogs.Bucket)
	require.NoError(t, err)
	_, err = store.GetInfo(d.ObjectKey)
	require.ErrorIs(t, err, nats.ErrObjectNotFound)
	require.Equal(t, 202, f.request(t, "PUT", "native-a", suffix, cipher, f.private[0], f.uuids[0], "", header).StatusCode)
	require.Equal(t, 202, f.request(t, "POST", "native-a", "/events", ready, f.private[0], f.uuids[0], "", nil).StatusCode)
	// A second actual TLS Gateway shares the store; retry bytes and ready identity
	// remain identical without a process-local idempotency ledger.
	replica, err := NewServer(ServerConfig{Backend: f.server.backend, PublicKeys: f.server.publicKeys, ReceiptKey: bytes.Repeat([]byte{17}, 32), RunnerTokenSalt: "replica"})
	require.NoError(t, err)
	peer := httptest.NewTLSServer(replica.Handler())
	defer peer.Close()
	f.url, f.http = peer.URL, peer.Client()
	require.Equal(t, 202, f.request(t, "PUT", "native-a", suffix, cipher, f.private[0], f.uuids[0], "", header).StatusCode)
	require.Equal(t, 202, f.request(t, "POST", "native-a", "/events", ready, f.private[0], f.uuids[0], "", nil).StatusCode)
	actual, err := store.GetBytes(d.ObjectKey)
	require.NoError(t, err)
	require.Equal(t, cipher, actual)
	decrypted, err := nativelogs.Decode(actual, identity, a)
	require.NoError(t, err)
	require.Equal(t, raw, decrypted.Logs)
	subject, err := hmessaging.RunnerEventSubject(f.org, route, event.Type)
	require.NoError(t, err)
	published, err := f.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
	require.NoError(t, err)
	require.Equal(t, ready, published.Data)
	expected, err := hmessaging.NativeMessageID(f.org, route, "events", event.Type, event.EventID)
	require.NoError(t, err)
	require.Equal(t, expected, published.Header.Get(nats.MsgIdHdr))
	_, err = store.PutBytes(d.ObjectKey, []byte("foreign existing bytes"))
	require.NoError(t, err)
	require.Equal(t, 503, f.request(t, "PUT", "native-a", suffix, cipher, f.private[0], f.uuids[0], "", header).StatusCode)
	actual, err = store.GetBytes(d.ObjectKey)
	require.NoError(t, err)
	require.Equal(t, []byte("foreign existing bytes"), actual)
	require.Equal(t, 400, f.request(t, "POST", "native-a", "/events", ready, f.private[0], f.uuids[0], "", nil).StatusCode)
}
