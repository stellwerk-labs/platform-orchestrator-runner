//go:build integration

package diode

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/golib/hnats"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/natstransport"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/signedcontrol"
	"github.com/stretchr/testify/require"
)

func writeNativeReverseFrame(t *testing.T, config ImportConfig, name string, message hmessaging.Message, attachment *Attachment, key ed25519.PrivateKey) {
	t.Helper()
	frame, err := signFrameWithAttachment(1, message, attachment, key)
	require.NoError(t, err)
	require.NoError(t, atomicJSON(filepath.Join(config.InputDir, name), frame))
}

func nativeReverseDirectories(t *testing.T, config ImportConfig) ImportConfig {
	t.Helper()
	root := t.TempDir()
	config.InputDir = filepath.Join(root, "input")
	config.ProcessedDir = filepath.Join(root, "processed")
	config.QuarantineDir = filepath.Join(root, "quarantine")
	config.LedgerPath = filepath.Join(root, "import.ledger")
	for _, path := range []string{config.InputDir, config.ProcessedDir, config.QuarantineDir} {
		require.NoError(t, os.MkdirAll(path, 0700))
	}
	return config
}

func TestNativeReverseSharedIngressSiteBinding(t *testing.T) {
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
	stream.Config.Duplicates = time.Minute
	_, err = js.UpdateStream(&stream.Config)
	require.NoError(t, err)
	t.Cleanup(func() { _, restoreErr := js.UpdateStream(&originalConfig); require.NoError(t, restoreErr) })
	store, err := js.ObjectStore(nativelogs.Bucket)
	if err != nil {
		store, err = js.CreateObjectStore(&nats.ObjectStoreConfig{Bucket: nativelogs.Bucket, Storage: nats.FileStorage})
	}
	require.NoError(t, err)
	publisher, err := natstransport.NewPublisher(connection)
	require.NoError(t, err)
	logA, attachment, keyA := nativeLogCarrierFixture(t)
	var eventA hmessaging.EventEnvelope
	require.NoError(t, json.Unmarshal(logA.Data, &eventA))
	configA := nativeReverseDirectories(t, ImportConfig{AuthoritativeOrg: eventA.OrganizationID, NativeRunnerUUID: strings.TrimPrefix(eventA.RunnerID, hmessaging.NativeRunnerTokenPrefix), ExpectedAttachment: "log"})
	legitimateB, configB := nativeReverseMessage(t, "deployment-result")
	configB.AuthoritativeOrg = configA.AuthoritativeOrg
	var eventB hmessaging.EventEnvelope
	require.NoError(t, json.Unmarshal(legitimateB.Data, &eventB))
	eventB.OrganizationID = configB.AuthoritativeOrg
	legitimateB.Data, err = json.Marshal(eventB)
	require.NoError(t, err)
	legitimateB.Subject, err = hmessaging.RunnerEventSubject(eventB.OrganizationID, eventB.RunnerID, eventB.Type)
	require.NoError(t, err)
	legitimateB.ID, err = hmessaging.NativeMessageID(eventB.OrganizationID, eventB.RunnerID, "events", eventB.Type, eventB.EventID)
	require.NoError(t, err)
	configB = nativeReverseDirectories(t, configB)
	publicB, keyB, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	publicA := keyA.Public().(ed25519.PublicKey)
	processedA := map[string]struct{}{}
	before, err := js.StreamInfo(hmessaging.RunnerEventsStreamName)
	require.NoError(t, err)
	eventA.Type = "deployment-result"
	eventA.EventID = eventA.DeploymentID + ":deployment-result"
	eventA.Payload = json.RawMessage(`{}`)
	validA := logA.Clone()
	validA.Data, err = json.Marshal(eventA)
	require.NoError(t, err)
	validA.Subject, err = hmessaging.RunnerEventSubject(eventA.OrganizationID, eventA.RunnerID, eventA.Type)
	require.NoError(t, err)
	validA.ID, err = hmessaging.NativeMessageID(eventA.OrganizationID, eventA.RunnerID, "events", eventA.Type, eventA.EventID)
	require.NoError(t, err)
	started := time.Now()
	forgedID := validA.Clone()
	forgedID.ID = legitimateB.ID
	forgedID.Header = hmessaging.Header{"nAtS-mSg-Id": {legitimateB.ID}, "Nats-Expected-Stream": {"foreign"}}
	writeNativeReverseFrame(t, configA, "01-foreign-id.json", forgedID, nil, keyA)
	writeNativeReverseFrame(t, configA, "02-foreign-content.json", legitimateB, nil, keyA)
	forgedLog := logA.Clone()
	forgedLog.ID = legitimateB.ID
	writeNativeReverseFrame(t, configA, "03-foreign-log-id.json", forgedLog, attachment, keyA)
	require.NoError(t, importAvailable(t.Context(), configA, publicA, connection, publisher, processedA))
	after, err := js.StreamInfo(hmessaging.RunnerEventsStreamName)
	require.NoError(t, err)
	require.Equal(t, before.State, after.State, "rejected A frames must not mutate the shared stream")
	_, err = store.GetInfo(attachment.Key)
	require.ErrorIs(t, err, nats.ErrObjectNotFound, "foreign identity must refuse before log object storage")
	_, err = os.Stat(configA.LedgerPath)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Empty(t, processedA)
	quarantined, err := os.ReadDir(configA.QuarantineDir)
	require.NoError(t, err)
	require.Len(t, quarantined, 3)
	writeNativeReverseFrame(t, configB, "legitimate-b.json", legitimateB, nil, keyB)
	require.NoError(t, importAvailable(t.Context(), configB, publicB, connection, publisher, map[string]struct{}{}))
	require.Less(t, time.Since(started), stream.Config.Duplicates, "B must arrive inside the actual broker duplicate window")
	publishedB, err := js.GetLastMsg(hmessaging.RunnerEventsStreamName, legitimateB.Subject)
	require.NoError(t, err)
	require.Equal(t, legitimateB.Data, publishedB.Data)
	require.Equal(t, legitimateB.ID, publishedB.Header.Get(nats.MsgIdHdr))
	// Otherwise valid A carries hostile mixed-case broker headers. Their removal
	// must be proven independently of the earlier identity refusals.
	validA.Header = hmessaging.Header{"nAtS-mSg-Id": {legitimateB.ID}, "nAtS-eXpEcTeD-sTrEaM": {"foreign"}, "X-Site": {"drop-me"}}
	writeNativeReverseFrame(t, configA, "04-valid-a.json", validA, nil, keyA)
	require.NoError(t, importAvailable(t.Context(), configA, publicA, connection, publisher, processedA))
	publishedA, err := js.GetLastMsg(hmessaging.RunnerEventsStreamName, validA.Subject)
	require.NoError(t, err)
	require.Equal(t, validA.Data, publishedA.Data)
	require.Equal(t, validA.ID, publishedA.Header.Get(nats.MsgIdHdr))
	for name := range publishedA.Header {
		require.False(t, strings.EqualFold(name, "Nats-Expected-Stream") || strings.EqualFold(name, "X-Site"))
	}
	ledgerA, err := readProcessed(configA.LedgerPath)
	require.NoError(t, err)
	require.Equal(t, map[string]struct{}{validA.ID: {}}, ledgerA)
}

func TestNativeReverseOversizeControlDoesNotReserveIdentity(t *testing.T) {
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
	publisher, err := natstransport.NewPublisher(connection)
	require.NoError(t, err)
	message, config := nativeReverseMessage(t, "rollout-control-result")
	config = nativeReverseDirectories(t, config)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	processed := map[string]struct{}{}
	before, err := js.StreamInfo(hmessaging.RunnerEventsStreamName)
	require.NoError(t, err)
	oversized := message.Clone()
	oversized.Data = append(oversized.Data, []byte(strings.Repeat(" ", signedcontrol.MaxBytes+1-len(oversized.Data)))...)
	writeNativeReverseFrame(t, config, "oversized.json", oversized, nil, private)
	require.NoError(t, importAvailable(t.Context(), config, public, connection, publisher, processed))
	after, err := js.StreamInfo(hmessaging.RunnerEventsStreamName)
	require.NoError(t, err)
	require.Equal(t, before.State, after.State)
	require.Empty(t, processed)
	_, err = os.Stat(config.LedgerPath)
	require.ErrorIs(t, err, os.ErrNotExist)
	quarantined, err := os.ReadDir(config.QuarantineDir)
	require.NoError(t, err)
	require.Len(t, quarantined, 1)
	writeNativeReverseFrame(t, config, "conforming.json", message, nil, private)
	require.NoError(t, importAvailable(t.Context(), config, public, connection, publisher, processed))
	published, err := js.GetLastMsg(hmessaging.RunnerEventsStreamName, message.Subject)
	require.NoError(t, err)
	require.Equal(t, message.Data, published.Data)
	ledger, err := readProcessed(config.LedgerPath)
	require.NoError(t, err)
	require.Equal(t, map[string]struct{}{message.ID: {}}, ledger)
}
