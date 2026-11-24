//go:build integration

package natstransport

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/golib/hnats"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnsupportedNATSPreservesNativeCustodyAndLegacyDelivery(t *testing.T) {
	require.Equal(t, "1", os.Getenv("UNSUPPORTED_NATS_ISOLATED"), "requires its separately owned unsupported broker")
	address := os.Getenv("UNSUPPORTED_NATS_URL")
	require.NotEmpty(t, address)
	var connection *nats.Conn
	require.Eventually(t, func() bool {
		var connectErr error
		connection, connectErr = nats.Connect(address, nats.Timeout(500*time.Millisecond))
		return connectErr == nil
	}, 5*time.Second, 50*time.Millisecond)
	t.Cleanup(connection.Close)
	require.Equal(t, "2.9.25", connection.ConnectedServerVersion(), "this fault test must exercise the actual pinned unsupported release")
	modern, err := hnats.NewJetStream(connection)
	require.NoError(t, err)
	require.NoError(t, hnats.EnsureStandardStreams(t.Context(), modern, 1))
	js, err := connection.JetStream()
	require.NoError(t, err)
	org := "unsupported-" + uuid.NewString()
	token, err := hmessaging.NativeRunnerToken(uuid.NewString())
	require.NoError(t, err)
	subject, err := hmessaging.RunnerCommandSubject(org, token)
	require.NoError(t, err)
	durable := durableName(org, token)
	_, err = js.AddConsumer(hmessaging.RunnerCommandsStreamName, &nats.ConsumerConfig{Durable: durable, FilterSubject: subject, AckPolicy: nats.AckExplicitPolicy, AckWait: time.Minute, MaxDeliver: defaultMaxDeliver})
	require.NoError(t, err)
	existing, err := js.PullSubscribe(subject, durable, nats.Bind(hmessaging.RunnerCommandsStreamName, durable), nats.ManualAck())
	require.NoError(t, err)
	_, err = js.Publish(subject, []byte("retained-owner-pending"))
	require.NoError(t, err)
	messages, err := existing.Fetch(1, nats.MaxWait(time.Second))
	require.NoError(t, err)
	require.Len(t, messages, 1)
	before, err := js.ConsumerInfo(hmessaging.RunnerCommandsStreamName, durable)
	require.NoError(t, err)
	require.Equal(t, 1, before.NumAckPending)
	streamBefore, err := js.StreamInfo(hmessaging.RunnerCommandsStreamName)
	require.NoError(t, err)
	consumer, refusal := NewRunnerConsumer(connection, org, token, true)
	after, err := js.ConsumerInfo(hmessaging.RunnerCommandsStreamName, durable)
	require.NoError(t, err)
	streamAfter, err := js.StreamInfo(hmessaging.RunnerCommandsStreamName)
	require.NoError(t, err)
	// Inspect real retained custody before the return-value assertion, so removing
	// the guard fails for actual mutation, not merely different error wording.
	assert.Equal(t, before.Config, after.Config, "native constructor changed existing configuration on unsupported broker")
	assert.Equal(t, streamBefore.Config, streamAfter.Config)
	assert.Equal(t, before.Created, after.Created)
	assert.Equal(t, before.Delivered, after.Delivered)
	assert.Equal(t, before.AckFloor, after.AckFloor)
	assert.Equal(t, 1, after.NumAckPending)
	assert.Nil(t, consumer)
	assert.EqualError(t, refusal, "native consumer requires stable NATS 2.10.0 or later")

	// The unsupported native capability must not disable legacy delivery.
	legacy, err := NewRunnerConsumer(connection, org, "legacy-site", false)
	require.NoError(t, err)
	legacySubject, err := hmessaging.RunnerCommandSubject(org, "legacy-site")
	require.NoError(t, err)
	_, err = js.Publish(legacySubject, []byte("legacy-still-supported"))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	delivery, err := legacy.Fetch(ctx)
	require.NoError(t, err)
	require.Equal(t, []byte("legacy-still-supported"), delivery.Message.Data)
	require.NoError(t, delivery.Ack())
	require.Eventually(t, func() bool {
		info, readErr := js.ConsumerInfo(hmessaging.RunnerCommandsStreamName, durableName(org, "legacy-site"))
		return readErr == nil && info.NumAckPending == 0
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, messages[0].AckSync(), "original pending owner lost custody")
}
