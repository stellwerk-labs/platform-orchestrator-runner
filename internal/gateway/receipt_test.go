package gateway

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReceiptRoundTripAndAuthentication(t *testing.T) {
	cipher, err := newReceiptCipher(make([]byte, 32))
	require.NoError(t, err)
	now := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	want := receiptClaims{
		OrganizationID: "org", RunnerID: "runner", CommandID: "command",
		AckSubject: "$JS.ACK.stream.consumer.1.1.1.0.0", StreamSequence: 42, ExpiresAt: now.Add(time.Minute).Unix(),
	}
	receipt, err := cipher.seal(want)
	require.NoError(t, err)
	got, err := cipher.open(receipt, now)
	require.NoError(t, err)
	require.Equal(t, want, got)

	tampered := receipt[:len(receipt)-1] + "A"
	_, err = cipher.open(tampered, now)
	require.Error(t, err)
	_, err = cipher.open(receipt, now.Add(2*time.Minute))
	require.ErrorContains(t, err, "expired")
}
