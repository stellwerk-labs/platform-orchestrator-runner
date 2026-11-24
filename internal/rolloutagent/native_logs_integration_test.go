//go:build integration && (linux || darwin)

package rolloutagent

import (
	"bytes"
	"context"
	"crypto"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gateway"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gatewayapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/signedcontrol"
	"github.com/stretchr/testify/require"
)

func nativeLogTransportFixture(t *testing.T) (*haProcessFixture, rollout.Event, rollout.EventDelivery) {
	t.Helper()
	f := newHAProcessFixture(t)
	f.wire.config.Directory = filepath.Join(f.root, "native-log-worker")
	f.wire.config.RunnerUUID = f.runnerUUID
	f.wire.config.Postgres = &f.pg
	require.NoError(t, rollout.InitializePostgres(t.Context(), f.wire.config))
	f.initialized = true
	var err error
	f.wire.engine, err = rollout.Open(t.Context(), f.wire.config)
	require.NoError(t, err)
	t.Cleanup(func() { f.wire.closeEngine(t) })
	f.wire.command = f.command(t, "reviewed")
	managed, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	f.wire.command.Request.EncryptedLogsRecipient = managed.Recipient().String()
	_, err = f.wire.engine.Submit(t.Context(), f.wire.signed(t))
	require.NoError(t, err)
	f.wire.wait(t, "awaiting_approval")
	events, err := f.wire.engine.Events(t.Context(), 0)
	require.NoError(t, err)
	for _, event := range events {
		delivery, err := f.wire.engine.PrepareDelivery(t.Context(), event.ID)
		require.NoError(t, err)
		if delivery.Log != nil {
			return f, event, delivery
		}
	}
	t.Fatal("real plan did not freeze managed ciphertext")
	return nil, rollout.Event{}, rollout.EventDelivery{}
}

func nativeLogFaultGateway(t *testing.T, f *haProcessFixture, fault func(http.ResponseWriter, *http.Request) bool) {
	t.Helper()
	block, _ := pem.Decode(f.wire.clientConfig.PrivateKey)
	require.NotNil(t, block)
	private, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	require.NoError(t, err)
	signer, ok := private.(crypto.Signer)
	require.True(t, ok)
	public, err := x509.MarshalPKIXPublicKey(signer.Public())
	require.NoError(t, err)
	backend, err := gateway.NewNATSBackend(f.wire.connection)
	require.NoError(t, err)
	server, err := gateway.NewServer(gateway.ServerConfig{Backend: backend, PublicKeys: gateway.StaticPublicKeyResolver{OrganizationID: f.wire.config.OrgID, RunnerID: f.wire.config.RunnerID, RunnerUUID: f.runnerUUID, PublicKey: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: public})}, ReceiptKey: make([]byte, 32), RunnerTokenSalt: "native-log-fixture"})
	require.NoError(t, err)
	handler := server.Handler()
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !fault(w, r) {
			handler.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(peer.Close)
	ca := filepath.Join(t.TempDir(), "native-log-gateway.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: peer.Certificate().Raw}), 0600))
	config := f.wire.clientConfig
	config.BaseURL = peer.URL
	config.CAFile = ca
	f.wire.clientConfig = config
	f.wire.client, err = gatewayapi.NewClient(config)
	require.NoError(t, err)
}

// Populate real PG pending delivery rows from an actual capture, preserving its
// opaque attachment. High transport revisions avoid disturbing domain progress.
func nativeLogBacklog(t *testing.T, f *haProcessFixture, original rollout.EventDelivery) []rollout.Event {
	t.Helper()
	identity, err := rollout.LoadStorageIdentity(f.pg.IdentityFile)
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE ` + f.table("rollout_events") + ` SET acknowledged=1`)
	require.NoError(t, err)
	rows := make([]rollout.Event, 0, 100)
	for i := range 100 {
		var envelope hmessaging.EventEnvelope
		require.NoError(t, json.Unmarshal(original.Progress, &envelope))
		var body rollout.Event
		require.NoError(t, json.Unmarshal(envelope.Payload, &body))
		body.Progress.Revision = int64(100000 + i)
		progress, err := json.Marshal(body.Progress)
		require.NoError(t, err)
		require.NoError(t, f.db.QueryRow(`INSERT INTO `+f.table("rollout_events")+`(rollout,revision,payload,acknowledged) VALUES($1,$2,$3,0) RETURNING id`, body.Progress.ID, body.Progress.Revision, progress).Scan(&body.ID))
		envelope.EventID = fmt.Sprintf("%s:%d", body.Progress.ID, body.Progress.Revision)
		envelope.Payload, err = json.Marshal(body)
		require.NoError(t, err)
		delivery := original
		delivery.EventID = body.ID
		delivery.Result = nil
		delivery.Progress, err = json.Marshal(envelope)
		require.NoError(t, err)
		plain, err := json.Marshal(delivery)
		require.NoError(t, err)
		var ciphertext bytes.Buffer
		writer, err := age.Encrypt(&ciphertext, identity.Recipient())
		require.NoError(t, err)
		_, err = writer.Write(plain)
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		_, err = f.db.Exec(`INSERT INTO `+f.table("rollout_deliveries")+`(event_id,content) VALUES($1,$2)`, body.ID, ciphertext.Bytes())
		require.NoError(t, err)
		rows = append(rows, body)
	}
	return rows
}

func TestNativeLogHundredFailedUploadsDoNotHideLaterCompletion(t *testing.T) {
	f, _, frozen := nativeLogTransportFixture(t)
	backlog := nativeLogBacklog(t, f, frozen)
	nativeLogFaultGateway(t, f, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.URL.Path, "/native-log-artifacts/") {
			w.WriteHeader(503)
			return true
		}
		return false
	})
	for _, event := range backlog {
		operation, err := publishEvent(t.Context(), f.wire.engine, f.wire.client, event)
		require.Equal(t, "upload_log", operation)
		require.Error(t, err)
	}
	_, err := f.wire.engine.Cancel(t.Context(), f.wire.command.ID, "operator")
	require.NoError(t, err)
	cursor := eventCursor{}
	// One bounded page per sweep. The second page reaches actual row 101 while
	// every failed attachment remains retained, and an empty page resets retry.
	flushProgressPage(t.Context(), f.wire.engine, f.wire.client, &cursor)
	require.Equal(t, backlog[99].ID, cursor.after)
	flushProgressPage(t.Context(), f.wire.engine, f.wire.client, &cursor)
	require.Greater(t, cursor.after, backlog[99].ID)
	var retained, completion int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FILTER(WHERE id<=$1 AND acknowledged=0),count(*) FILTER(WHERE id>$1 AND acknowledged=0) FROM `+f.table("rollout_events"), backlog[99].ID).Scan(&retained, &completion))
	require.Equal(t, 100, retained)
	require.Zero(t, completion)
	route, err := hmessaging.NativeRunnerToken(f.runnerUUID)
	require.NoError(t, err)
	subject, err := hmessaging.RunnerEventSubject(f.wire.config.OrgID, route, "deployment-result")
	require.NoError(t, err)
	result, err := f.wire.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
	require.NoError(t, err)
	require.NotEmpty(t, result.Data)
	previousBound := cursor.through
	flushProgressPage(t.Context(), f.wire.engine, f.wire.client, &cursor)
	require.Equal(t, backlog[99].ID, cursor.after, "the next finite pass retries retained failures")
	require.Less(t, cursor.through, previousBound)
}

func TestNativeHungArtifactAllowsProgressAndControlReceipt(t *testing.T) {
	f, _, frozen := nativeLogTransportFixture(t)
	backlog := nativeLogBacklog(t, f, frozen)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	nativeLogFaultGateway(t, f, func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.Contains(r.URL.Path, "/native-log-artifacts/") {
			return false
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return false
		case <-r.Context().Done():
			return true
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); flushEvents(ctx, f.wire.engine, f.wire.client, true) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("artifact loop ignored cancellation")
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("actual HTTP artifact did not block")
	}
	execution := f.wire.signed(t)
	current := f.wire.wait(t, "awaiting_approval")
	request := signedcontrol.Request{Version: signedcontrol.Version, ID: uuid.NewString(), RolloutID: f.wire.command.ID, OrgID: f.wire.config.OrgID, EnvironmentID: f.wire.command.EnvironmentID, Target: signedcontrol.Target(*f.wire.command.NativeTarget), ExecutionDigest: signedcontrol.Digest(execution.Payload), ActorID: uuid.NewString(), Action: "deny", Subject: current.Subject, IssuedAt: time.Now().UTC(), ExpiresAt: f.wire.command.ExpiresAt}
	envelope := signedControlEnvelope(t, f, request)
	publishControlEnvelope(t, f, envelope, nil)
	command, err := f.wire.client.NextCommand(t.Context())
	require.NoError(t, err)
	_, err = handleDelivery(t.Context(), f.wire.engine, f.wire.client, request.OrgID, envelope.RunnerID, command)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		var count int
		err := f.db.QueryRow(`SELECT count(*) FROM `+f.table("rollout_events")+` WHERE id>$1 AND acknowledged=1`, backlog[99].ID).Scan(&count)
		subject, _ := hmessaging.RunnerEventSubject(request.OrgID, envelope.RunnerID, ControlEventType)
		result, publishErr := f.wire.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
		return err == nil && count > 0 && publishErr == nil && len(result.Data) > 0
	}, 20*time.Second, 25*time.Millisecond, "slow log hid progress or the independent 5-second receipt sweep")
	var pending int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM `+f.table("rollout_events")+` WHERE id<=$1 AND acknowledged=0`, backlog[99].ID).Scan(&pending))
	require.Equal(t, 100, pending)
	close(release)
	require.Eventually(t, func() bool {
		var ack int
		return f.db.QueryRow(`SELECT acknowledged FROM `+f.table("rollout_events")+` WHERE id=$1`, backlog[0].ID).Scan(&ack) == nil && ack == 1
	}, 10*time.Second, 25*time.Millisecond)
}

func TestHANativeLogFrozenCipherSurvivesWorkerAndGatewayReplacement(t *testing.T) {
	f := newHAProcessFixture(t)
	originalGateway := f.wire.clientConfig
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	nativeLogFaultGateway(t, f, func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.Contains(r.URL.Path, "/native-log-artifacts/") {
			return false
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
		return true
	})
	t.Cleanup(func() { close(release) })
	f.writeCommonConfig(t)
	first := f.start(t, "worker-0", true)
	command := f.command(t, "reviewed")
	managed, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	command.Request.EncryptedLogsRecipient = managed.Recipient().String()
	f.publish(t, command)
	f.progress(t, command.ID, "awaiting_approval")
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first worker never reached artifact HTTP")
	}
	rows, err := f.db.Query(`SELECT d.content FROM `+f.table("rollout_deliveries")+` d JOIN `+f.table("rollout_events")+` e ON e.id=d.event_id WHERE e.rollout=$1 ORDER BY e.id`, command.ID)
	require.NoError(t, err)
	keyBytes, err := os.ReadFile(f.pg.IdentityFile)
	require.NoError(t, err)
	sharedIdentity, err := age.ParseX25519Identity(strings.TrimSpace(string(keyBytes)))
	require.NoError(t, err)
	var frozen rollout.EventDelivery
	for rows.Next() {
		var content []byte
		require.NoError(t, rows.Scan(&content))
		plain, err := nativelogs.Decrypt(content, sharedIdentity, nativelogs.DeliveryLimit)
		require.NoError(t, err)
		var candidate rollout.EventDelivery
		require.NoError(t, json.Unmarshal(plain, &candidate))
		if candidate.Log != nil {
			frozen = candidate
		}
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.NotNil(t, frozen.Log)
	first.kill(t)
	f.wire.clientConfig = originalGateway
	f.writeCommonConfig(t)
	second := f.start(t, "worker-1", true)
	store, err := f.wire.js.ObjectStore(nativelogs.Bucket)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		content, err := store.GetBytes(frozen.Log.Descriptor.ObjectKey)
		return err == nil && bytes.Equal(content, frozen.Log.Ciphertext)
	}, 15*time.Second, 25*time.Millisecond, "replacement worker changed or lost frozen ciphertext")
	content, err := store.GetBytes(frozen.Log.Descriptor.ObjectKey)
	require.NoError(t, err)
	envelope, err := nativelogs.Decode(content, managed, frozen.Log.Descriptor.Authority)
	require.NoError(t, err)
	require.NotEmpty(t, envelope.Logs)
	f.action(t, second, command.ID, "cancel", "")
	f.progress(t, command.ID, "cancelled")
	f.waitEventsAcked(t, command.ID)
	require.Equal(t, 1, f.wire.operations(t, "plan"))
	require.Zero(t, f.wire.operations(t, "apply"))
}
