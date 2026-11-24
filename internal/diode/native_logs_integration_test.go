//go:build integration

package diode

import (
	"context"
	"crypto/ed25519"
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
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/natstransport"
	"github.com/stretchr/testify/require"
)

func TestNativeLogSignedExportImportAndReplay(t *testing.T) {
	t.Run("generic", func(t *testing.T) { nativeLogSignedExportImportAndReplay(t, false) })
	t.Run("bound", func(t *testing.T) { nativeLogSignedExportImportAndReplay(t, true) })
}

func nativeLogSignedExportImportAndReplay(t *testing.T, bound bool) {
	require.Equal(t, "1", os.Getenv("AGENT_DELIVERY_ISOLATED"))
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
	originalConfig := stream.Config
	stream.Config.Duplicates = time.Second
	_, err = js.UpdateStream(&stream.Config)
	require.NoError(t, err)
	t.Cleanup(func() { _, restoreErr := js.UpdateStream(&originalConfig); require.NoError(t, restoreErr) })
	configured, err := js.StreamInfo(hmessaging.RunnerEventsStreamName)
	require.NoError(t, err)
	require.Equal(t, time.Second, configured.Config.Duplicates)
	duplicateWindow := configured.Config.Duplicates
	store, err := js.ObjectStore(nativelogs.Bucket)
	if err != nil {
		store, err = js.CreateObjectStore(&nats.ObjectStoreConfig{Bucket: nativelogs.Bucket, Storage: nats.FileStorage})
	}
	require.NoError(t, err)
	message, attachment, private := nativeLogCarrierFixture(t)
	_, err = store.PutBytes(attachment.Key, attachment.Data)
	require.NoError(t, err)
	publisher, err := natstransport.NewPublisher(connection)
	require.NoError(t, err)
	require.NoError(t, publisher.Publish(t.Context(), message))
	original, err := js.GetLastMsg(hmessaging.RunnerEventsStreamName, message.Subject)
	require.NoError(t, err)
	root := t.TempDir()
	input := filepath.Join(root, "input")
	keyPath := filepath.Join(root, "relay.pem")
	der, err := x509.MarshalPKCS8PrivateKey(private)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- RunExporter(ctx, ExportConfig{NATS: natstransport.Config{URL: address}, Stream: hmessaging.RunnerEventsStreamName, Subject: message.Subject, Durable: "native-log-" + uuid.NewString(), OutputDir: input, LedgerPath: filepath.Join(root, "export.ledger"), SigningKey: keyPath, AttachObject: "log"})
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Error("owned exporter did not stop")
		}
	}()
	var path string
	require.Eventually(t, func() bool {
		entries, err := os.ReadDir(input)
		if err != nil || len(entries) == 0 {
			return false
		}
		path = filepath.Join(input, entries[0].Name())
		return true
	}, 5*time.Second, 20*time.Millisecond)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var frame Frame
	require.NoError(t, json.Unmarshal(data, &frame))
	require.NoError(t, verifyFrame(frame, private.Public().(ed25519.PublicKey)))
	require.Equal(t, message.Data, frame.Message.Data)
	require.Equal(t, attachment.Data, frame.Attachment.Data)
	require.NoError(t, store.Delete(attachment.Key))
	config := ImportConfig{InputDir: input, ProcessedDir: filepath.Join(root, "processed"), QuarantineDir: filepath.Join(root, "quarantine"), LedgerPath: filepath.Join(root, "import.ledger"), ExpectedAttachment: "log"}
	if bound {
		var event hmessaging.EventEnvelope
		require.NoError(t, json.Unmarshal(message.Data, &event))
		config.AuthoritativeOrg = event.OrganizationID
		config.NativeRunnerUUID = strings.TrimPrefix(event.RunnerID, hmessaging.NativeRunnerTokenPrefix)
	}
	require.NoError(t, os.MkdirAll(config.ProcessedDir, 0700))
	require.NoError(t, os.MkdirAll(config.QuarantineDir, 0700))
	processed := map[string]struct{}{}
	// The first accepted import is a real duplicate-ID publication beyond the
	// original publish window. It must visibly advance the broker sequence.
	time.Sleep(duplicateWindow + 100*time.Millisecond)
	require.NoError(t, importAvailable(t.Context(), config, private.Public().(ed25519.PublicKey), connection, publisher, processed))
	actual, err := store.GetBytes(attachment.Key)
	require.NoError(t, err)
	require.Equal(t, attachment.Data, actual)
	info, err := store.GetInfo(attachment.Key)
	require.NoError(t, err)
	published, err := js.GetLastMsg(hmessaging.RunnerEventsStreamName, message.Subject)
	require.NoError(t, err)
	require.Equal(t, message.Data, published.Data)
	require.Equal(t, message.ID, published.Header.Get(nats.MsgIdHdr))
	require.Greater(t, published.Sequence, original.Sequence, "positive control: identical ID is accepted outside the actual broker duplicate window")
	// Simulate process restart by reloading durable carrier deduplication state.
	processed, err = readProcessed(config.LedgerPath)
	require.NoError(t, err)
	require.Equal(t, map[string]struct{}{message.ID: {}}, processed)
	// A broken ledger replay check must now cause a visible extra publication.
	time.Sleep(duplicateWindow + 100*time.Millisecond)
	require.NoError(t, os.WriteFile(filepath.Join(input, "replay.json"), data, 0600))
	require.NoError(t, importAvailable(t.Context(), config, private.Public().(ed25519.PublicKey), connection, publisher, processed))
	replay, err := store.GetInfo(attachment.Key)
	require.NoError(t, err)
	require.Equal(t, info.ModTime, replay.ModTime, "exact replay must not rewrite stored ciphertext")
	republished, err := js.GetLastMsg(hmessaging.RunnerEventsStreamName, message.Subject)
	require.NoError(t, err)
	require.Equal(t, published.Sequence, republished.Sequence, "durable carrier replay must not republish")
	// A validly signed foreign object must be rejected before storage or the
	// processed ledger, even when that carrier identity was already accepted.
	foreign := *attachment
	foreign.Key = "foreign/object"
	bad, err := signFrameWithAttachment(2, message, &foreign, private)
	require.NoError(t, err)
	require.NoError(t, atomicJSON(filepath.Join(input, "foreign.json"), bad))
	before, err := os.ReadFile(config.LedgerPath)
	require.NoError(t, err)
	require.NoError(t, importAvailable(t.Context(), config, private.Public().(ed25519.PublicKey), connection, publisher, processed))
	_, err = store.GetInfo(foreign.Key)
	require.ErrorIs(t, err, nats.ErrObjectNotFound)
	after, err := os.ReadFile(config.LedgerPath)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.NoError(t, os.WriteFile(filepath.Join(input, "conflict.json"), data, 0600))
	_, err = store.PutBytes(attachment.Key, []byte("conflicting stored bytes"))
	require.NoError(t, err)
	require.Error(t, importAvailable(t.Context(), config, private.Public().(ed25519.PublicKey), connection, publisher, processed))
	actual, err = store.GetBytes(attachment.Key)
	require.NoError(t, err)
	require.Equal(t, []byte("conflicting stored bytes"), actual)
	after, err = os.ReadFile(config.LedgerPath)
	require.NoError(t, err)
	require.Equal(t, before, after)
}
