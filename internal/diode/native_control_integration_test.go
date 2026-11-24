//go:build integration

package diode

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/golib/hnats"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/natstransport"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/signedcontrol"
	"github.com/stretchr/testify/require"
)

func TestNativeControlResultSignedFileCarrierPreservesExactBytes(t *testing.T) {
	require.Equal(t, "1", os.Getenv("AGENT_DELIVERY_ISOLATED"), "use the owned qualification broker")
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
	stream, err := js.StreamInfo(hmessaging.RunnerEventsStreamName)
	require.NoError(t, err)
	stream.Config.Duplicates = time.Second
	_, err = js.UpdateStream(&stream.Config)
	require.NoError(t, err)
	org := "carrier-" + uuid.NewString()
	route, err := hmessaging.NativeRunnerToken(uuid.NewString())
	require.NoError(t, err)
	result := signedcontrol.Result{Version: signedcontrol.ResultVersion, ControlID: uuid.NewString(), RolloutID: uuid.NewString(), RequestDigest: strings.Repeat("a", 64), ExecutionDigest: strings.Repeat("b", 64), Outcome: "applied", Phase: "cancelled", Revision: 2}
	body, err := json.Marshal(result)
	require.NoError(t, err)
	event := hmessaging.EventEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1, EventID: "pipeline-control:" + result.ControlID + ":result", CommandID: "pipeline-control:" + result.ControlID, OrganizationID: org, RunnerID: route, DeploymentID: result.RolloutID, Type: "rollout-control-result", CreatedAt: time.Now().UTC(), Payload: body}
	wire, err := json.Marshal(event)
	require.NoError(t, err)
	subject, err := hmessaging.RunnerEventSubject(org, route, event.Type)
	require.NoError(t, err)
	messageID, err := hmessaging.NativeMessageID(org, route, "events", event.Type, event.EventID)
	require.NoError(t, err)
	publisher, err := natstransport.NewPublisher(connection)
	require.NoError(t, err)
	require.NoError(t, publisher.Publish(t.Context(), hmessaging.Message{ID: messageID, Subject: subject, Data: wire, CreatedAt: event.CreatedAt}))
	original, err := js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
	require.NoError(t, err)
	root := t.TempDir()
	input := filepath.Join(root, "input")
	keyPath := filepath.Join(root, "relay.pem")
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(private)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- RunExporter(ctx, ExportConfig{NATS: natstransport.Config{URL: address}, Stream: hmessaging.RunnerEventsStreamName, Subject: subject, Durable: "carrier-" + uuid.NewString(), OutputDir: input, LedgerPath: filepath.Join(root, "export.ledger"), SigningKey: keyPath})
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Error("owned file exporter did not stop")
		}
	}()
	var framePath string
	require.Eventually(t, func() bool {
		entries, err := os.ReadDir(input)
		if err != nil || len(entries) == 0 {
			return false
		}
		framePath = filepath.Join(input, entries[0].Name())
		return true
	}, 5*time.Second, 20*time.Millisecond)
	data, err := os.ReadFile(framePath)
	require.NoError(t, err)
	var frame Frame
	require.NoError(t, json.Unmarshal(data, &frame))
	require.NoError(t, verifyFrame(frame, public))
	require.Equal(t, wire, frame.Message.Data)
	require.NoError(t, js.DeleteMsg(hmessaging.RunnerEventsStreamName, original.Sequence))
	time.Sleep(1100 * time.Millisecond) // Import republishes beyond real broker deduplication.
	config := ImportConfig{InputDir: input, ProcessedDir: filepath.Join(root, "processed"), QuarantineDir: filepath.Join(root, "quarantine"), LedgerPath: filepath.Join(root, "import.ledger")}
	require.NoError(t, os.MkdirAll(config.ProcessedDir, 0700))
	require.NoError(t, os.MkdirAll(config.QuarantineDir, 0700))
	require.NoError(t, importAvailable(t.Context(), config, public, connection, publisher, map[string]struct{}{}))
	imported, err := js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
	require.NoError(t, err)
	require.Greater(t, imported.Sequence, original.Sequence)
	require.Equal(t, wire, imported.Data)
	require.Equal(t, messageID, imported.Header.Get(nats.MsgIdHdr))
	require.NoError(t, os.WriteFile(filepath.Join(input, "replayed.json"), data, 0600))
	processed, err := readProcessed(config.LedgerPath)
	require.NoError(t, err)
	require.NoError(t, importAvailable(t.Context(), config, public, connection, publisher, processed))
	repeated, err := js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
	require.NoError(t, err)
	require.Equal(t, imported.Sequence, repeated.Sequence)
}
