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
	require.Equal(t, "1", os.Getenv("AGENT_DELIVERY_ISOLATED"))
	address := os.Getenv("AGENT_DELIVERY_NATS_URL")
	require.NotEmpty(t, address)
	connection, err := nats.Connect(address, nats.Timeout(2*time.Second))
	require.NoError(t, err)
	defer connection.Close()
	modern, err := hnats.NewJetStream(connection)
	require.NoError(t, err)
	require.NoError(t, hnats.EnsureStandardStreams(t.Context(), modern, 1))
	js, err := connection.JetStream()
	require.NoError(t, err)
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
	require.NoError(t, os.MkdirAll(config.ProcessedDir, 0700))
	require.NoError(t, os.MkdirAll(config.QuarantineDir, 0700))
	processed := map[string]struct{}{}
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
	require.NoError(t, os.WriteFile(filepath.Join(input, "replay.json"), data, 0600))
	require.NoError(t, importAvailable(t.Context(), config, private.Public().(ed25519.PublicKey), connection, publisher, processed))
	replay, err := store.GetInfo(attachment.Key)
	require.NoError(t, err)
	require.Equal(t, info.ModTime, replay.ModTime, "exact replay must not rewrite stored ciphertext")
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
