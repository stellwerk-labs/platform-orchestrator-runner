package integrationtests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/natstransport"
	"github.com/stretchr/testify/require"
)

func TestNATSRunnerCommandBuffersWhileRunnerIsDisconnected(t *testing.T) {
	url := os.Getenv("NATS_TEST_URL")
	if url == "" {
		url = "nats://localhost:4222"
	}
	orgID := fmt.Sprintf("buffer-test-org-%d", time.Now().UnixNano())
	runnerID := "buffer-test-runner"

	bootstrapConnection, err := natstransport.Connect(natstransport.Config{URL: url, Name: "buffer-test-bootstrap"})
	require.NoError(t, err)
	_, err = natstransport.NewRunnerConsumer(bootstrapConnection, orgID, runnerID, true)
	require.NoError(t, err)
	bootstrapConnection.Close()

	producerConnection, err := natstransport.Connect(natstransport.Config{URL: url, Name: "buffer-test-producer"})
	require.NoError(t, err)
	publisher, err := natstransport.NewPublisher(producerConnection, "")
	require.NoError(t, err)
	subject, err := hmessaging.RunnerCommandSubject(orgID, runnerID)
	require.NoError(t, err)
	createdAt := time.Now().UTC()
	messageID := fmt.Sprintf("buffered-command-%d", createdAt.UnixNano())
	require.NoError(t, publisher.Publish(t.Context(), hmessaging.Message{
		ID: messageID, Subject: subject, Data: []byte(`{"protocol_version":"1"}`), CreatedAt: createdAt,
		Header: hmessaging.Header{"Traceparent": {"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}, "X-Contract": {"preserved"}},
	}))
	producerConnection.Close()

	consumerConnection, err := natstransport.Connect(natstransport.Config{URL: url, Name: "buffer-test-consumer"})
	require.NoError(t, err)
	defer consumerConnection.Close()
	consumer, err := natstransport.NewRunnerConsumer(consumerConnection, orgID, runnerID, false)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	delivery, err := consumer.Fetch(ctx)
	require.NoError(t, err)
	require.Equal(t, messageID, delivery.Message.ID)
	require.Equal(t, "preserved", delivery.Message.Header.Get("X-Contract"))
	require.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", delivery.Message.Header.Get("Traceparent"))
	require.NoError(t, delivery.Ack())
}

func TestNATSResultAndLogsOutboxFlushAfterReconnect(t *testing.T) {
	url := os.Getenv("NATS_TEST_URL")
	if url == "" {
		url = "nats://localhost:4222"
	}
	const orgID = "edge-output-test-org"
	const runnerID = "edge-output-test-runner"
	outbox := t.TempDir()

	connection, err := natstransport.Connect(natstransport.Config{URL: url, Name: "edge-output-bootstrap"})
	require.NoError(t, err)
	_, err = natstransport.NewRunnerConsumer(connection, orgID, runnerID, true)
	require.NoError(t, err)
	js, err := connection.JetStream()
	require.NoError(t, err)
	store, err := js.ObjectStore(natstransport.RunnerLogsBucket)
	if errors.Is(err, nats.ErrBucketNotFound) || errors.Is(err, nats.ErrStreamNotFound) {
		store, err = js.CreateObjectStore(&nats.ObjectStoreConfig{Bucket: natstransport.RunnerLogsBucket, Storage: nats.FileStorage})
	}
	require.NoError(t, err)
	subject, err := hmessaging.RunnerEventSubject(orgID, runnerID, "deployment-result")
	require.NoError(t, err)
	subscriber, err := connection.SubscribeSync(subject)
	require.NoError(t, err)

	now := time.Now().UTC()
	resultMessage := hmessaging.Message{
		ID: "deployment-result-offline", Subject: subject, Data: []byte(`{"status":"success"}`), CreatedAt: now,
	}
	require.NoError(t, natstransport.PublishMessage(t.Context(), natstransport.Config{
		URL: "nats://127.0.0.1:1", Name: "edge-output-disconnected", OutboxDir: outbox, ConnectTimeout: 100 * time.Millisecond,
	}, resultMessage))
	spooledMessages, err := filepath.Glob(filepath.Join(outbox, "*.json"))
	require.NoError(t, err)
	require.Len(t, spooledMessages, 1)

	logSubject, err := hmessaging.RunnerEventSubject(orgID, runnerID, "log-object-ready")
	require.NoError(t, err)
	key := fmt.Sprintf("%s/%d", orgID, now.UnixNano())
	payload, err := json.Marshal(map[string]string{"bucket": natstransport.RunnerLogsBucket, "key": key})
	require.NoError(t, err)
	logEvent := hmessaging.EventEnvelope{
		ProtocolVersion: hmessaging.ProtocolVersionV1, EventID: "log-ready-offline",
		OrganizationID: orgID, RunnerID: runnerID, DeploymentID: "deployment-offline",
		Type: "log-object-ready", CreatedAt: now, Payload: payload,
	}
	logEventData, err := json.Marshal(logEvent)
	require.NoError(t, err)
	require.NoError(t, natstransport.PublishLogObject(t.Context(), natstransport.Config{
		URL: "nats://127.0.0.1:1", Name: "edge-log-disconnected", OutboxDir: outbox, ConnectTimeout: 100 * time.Millisecond,
	}, natstransport.LogObject{
		Bucket: natstransport.RunnerLogsBucket, Key: key, EncryptedLog: []byte("encrypted-log"),
		ReadyEvent: hmessaging.Message{ID: logEvent.EventID, Subject: logSubject, Data: logEventData, CreatedAt: now},
	}, false))
	spooledLogs, err := filepath.Glob(filepath.Join(outbox, "logs", "*.json"))
	require.NoError(t, err)
	require.Len(t, spooledLogs, 1)

	reconnected, err := natstransport.Connect(natstransport.Config{URL: url, Name: "edge-output-reconnected"})
	require.NoError(t, err)
	defer reconnected.Close()
	reconnectedPublisher, err := natstransport.NewPublisher(reconnected, outbox)
	require.NoError(t, err)
	require.NoError(t, reconnectedPublisher.Flush(t.Context()))
	require.NoError(t, natstransport.FlushLogObjects(t.Context(), reconnected, outbox))

	result, err := subscriber.NextMsg(5 * time.Second)
	require.NoError(t, err)
	require.Equal(t, resultMessage.Data, result.Data)
	storedLogs, err := store.GetBytes(key)
	require.NoError(t, err)
	require.Equal(t, []byte("encrypted-log"), storedLogs)

	objectRelay, err := natstransport.NewBoundConsumer(reconnected, "OBJ_"+natstransport.RunnerLogsBucket,
		"$O."+natstransport.RunnerLogsBucket+".>", fmt.Sprintf("object-relay-%d", now.UnixNano()))
	require.NoError(t, err)
	relayContext, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	objectDelivery, err := objectRelay.Fetch(relayContext)
	require.NoError(t, err)
	require.NotEmpty(t, objectDelivery.Message.ID)
	if objectDelivery.Message.Header.Get(nats.MsgIdHdr) == "" {
		require.Contains(t, objectDelivery.Message.ID, "stream:OBJ_"+natstransport.RunnerLogsBucket+":")
	}
	require.NoError(t, objectDelivery.Ack())
}
