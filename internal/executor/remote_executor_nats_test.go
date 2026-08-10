package executor

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/natstransport"
	"github.com/stretchr/testify/require"
)

func TestNATSDeadLetterIsPublishedBeforeCommandTermination(t *testing.T) {
	url := os.Getenv("NATS_TEST_URL")
	if url == "" {
		t.Skip("NATS_TEST_URL is not set")
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	orgID, runnerID := "dlq-org-"+suffix, "dlq-runner"
	connection, err := natstransport.Connect(natstransport.Config{URL: url, Name: "dlq-test"})
	require.NoError(t, err)
	defer connection.Close()
	_, err = natstransport.NewRunnerConsumer(connection, orgID, runnerID, true)
	require.NoError(t, err)
	publisher, err := natstransport.NewPublisher(connection, "")
	require.NoError(t, err)
	sourceSubject, err := hmessaging.RunnerCommandSubject(orgID, runnerID)
	require.NoError(t, err)
	dlqSubject, err := hmessaging.DeadLetterSubject(sourceSubject)
	require.NoError(t, err)
	subscriber, err := connection.SubscribeSync(dlqSubject)
	require.NoError(t, err)
	terminated := false
	naked := false
	message := hmessaging.Message{ID: "malformed-" + suffix, Subject: sourceSubject, Data: []byte("{"), CreatedAt: time.Now().UTC()}
	delivery := natstransport.NewDelivery(message, 1, func() error { return nil }, func(time.Duration) error {
		naked = true
		return nil
	}, func() error {
		terminated = true
		return nil
	})
	cause := errors.New("decode command envelope")
	require.ErrorIs(t, deadLetter(t.Context(), publisher, delivery, cause), cause)
	require.True(t, terminated)
	require.False(t, naked)
	received, err := subscriber.NextMsg(5 * time.Second)
	require.NoError(t, err)
	require.Equal(t, message.Data, received.Data)
}
