package integrationtests

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

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
	publisher, err := natstransport.NewPublisher(producerConnection)
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
