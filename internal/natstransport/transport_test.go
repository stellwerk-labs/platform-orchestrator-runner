package natstransport

import (
	"testing"

	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stretchr/testify/require"
)

func TestNativeConsumerRefusesUnavailableBrokerBeforeBootstrap(t *testing.T) {
	token, err := hmessaging.NativeRunnerToken("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	require.NoError(t, err)
	consumer, err := NewRunnerConsumer(nil, "org", token, true)
	require.Nil(t, consumer)
	require.EqualError(t, err, "native consumer requires stable NATS 2.10.0 or later")
}
