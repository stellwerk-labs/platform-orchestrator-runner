package rolloutagent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gatewayapi"
	"github.com/stretchr/testify/require"
)

func TestDeliveryDiagnosticsExcludePrivateErrorText(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, err := range []error{errors.New("PRIVATE_RECEIPT_TOKEN_BODY"), &gatewayapi.HTTPStatusError{StatusCode: 401}, context.DeadlineExceeded} {
		reportFailure("ack_command", err)
	}
	require.NotContains(t, output.String(), "PRIVATE_RECEIPT_TOKEN_BODY")
	require.Contains(t, output.String(), `"operation":"ack_command"`)
	require.Contains(t, output.String(), `"class":"http","status":401`)
	require.Contains(t, output.String(), `"class":"timeout"`)
}
