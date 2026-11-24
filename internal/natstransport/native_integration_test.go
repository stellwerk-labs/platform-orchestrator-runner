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
	"github.com/stretchr/testify/require"
)

func nativeBroker(t *testing.T) (*nats.Conn, nats.JetStreamContext, string, string) {
	t.Helper()
	require.Equal(t, "1", os.Getenv("AGENT_DELIVERY_ISOLATED"), "requires an owned broker")
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
	token, err := hmessaging.NativeRunnerToken(uuid.NewString())
	require.NoError(t, err)
	return connection, js, "consumer-" + uuid.NewString(), token
}

func TestNativeConsumerRefusesExistingForeignConfigurationWithoutMutation(t *testing.T) {
	for _, conflict := range []string{"legacy_reserved_consumer", "different_filter", "different_ack_wait"} {
		t.Run(conflict, func(t *testing.T) {
			connection, js, org, token := nativeBroker(t)
			subject, err := hmessaging.RunnerCommandSubject(org, token)
			require.NoError(t, err)
			configuration := nats.ConsumerConfig{Durable: durableName(org, token), Description: "stellwerk.runner.native/v1", FilterSubject: subject, AckPolicy: nats.AckExplicitPolicy, AckWait: defaultAckWait, MaxDeliver: defaultMaxDeliver}
			switch conflict {
			case "legacy_reserved_consumer":
				configuration.Description = ""
			case "different_filter":
				configuration.FilterSubject, err = hmessaging.RunnerCommandSubject(org, "foreign-site")
				require.NoError(t, err)
			case "different_ack_wait":
				configuration.AckWait = time.Minute
			}
			_, err = js.AddConsumer(hmessaging.RunnerCommandsStreamName, &configuration)
			require.NoError(t, err)
			subscription, err := js.PullSubscribe(configuration.FilterSubject, configuration.Durable, nats.Bind(hmessaging.RunnerCommandsStreamName, configuration.Durable), nats.ManualAck())
			require.NoError(t, err)
			_, err = js.Publish(configuration.FilterSubject, []byte("owned-pending-message"))
			require.NoError(t, err)
			pending, err := subscription.Fetch(1, nats.MaxWait(time.Second))
			require.NoError(t, err)
			require.Len(t, pending, 1)
			before, err := js.ConsumerInfo(hmessaging.RunnerCommandsStreamName, configuration.Durable)
			require.NoError(t, err)
			require.Equal(t, 1, before.NumAckPending)
			consumer, err := NewRunnerConsumer(connection, org, token, false)
			require.ErrorContains(t, err, "native consumer configuration refused")
			require.Nil(t, consumer)
			after, err := js.ConsumerInfo(hmessaging.RunnerCommandsStreamName, configuration.Durable)
			require.NoError(t, err)
			require.Equal(t, before.Config, after.Config)
			require.Equal(t, before.Created, after.Created)
			require.Equal(t, before.Delivered, after.Delivered)
			require.Equal(t, before.AckFloor, after.AckFloor)
			require.Equal(t, 1, after.NumAckPending)
			require.NoError(t, pending[0].AckSync())
		})
	}
}

func TestNativeConsumerRebindPreservesPendingCustodyAndCursor(t *testing.T) {
	connection, js, org, token := nativeBroker(t)
	consumer, err := NewRunnerConsumer(connection, org, token, false)
	require.NoError(t, err)
	subject, err := hmessaging.RunnerCommandSubject(org, token)
	require.NoError(t, err)
	_, err = js.Publish(subject, []byte("first"))
	require.NoError(t, err)
	_, err = js.Publish(subject, []byte("second"))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	first, err := consumer.Fetch(ctx)
	require.NoError(t, err)
	require.Equal(t, []byte("first"), first.Message.Data)
	before, err := js.ConsumerInfo(hmessaging.RunnerCommandsStreamName, durableName(org, token))
	require.NoError(t, err)
	replacement, err := NewRunnerConsumer(connection, org, token, false)
	require.NoError(t, err)
	after, err := js.ConsumerInfo(hmessaging.RunnerCommandsStreamName, durableName(org, token))
	require.NoError(t, err)
	require.Equal(t, before.Config, after.Config)
	require.Equal(t, before.Created, after.Created)
	require.Equal(t, before.Delivered, after.Delivered)
	require.Equal(t, 1, after.NumAckPending)
	second, err := replacement.Fetch(ctx)
	require.NoError(t, err)
	require.Equal(t, []byte("second"), second.Message.Data)
	require.Greater(t, second.StreamSequence, first.StreamSequence)
	require.NoError(t, first.Ack())
	require.NoError(t, second.Ack())
	require.Eventually(t, func() bool {
		info, readErr := js.ConsumerInfo(hmessaging.RunnerCommandsStreamName, durableName(org, token))
		return readErr == nil && info.NumAckPending == 0
	}, time.Second, 10*time.Millisecond)
}
