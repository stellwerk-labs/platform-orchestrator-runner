package rolloutagent

import (
	"encoding/json"
	"testing"

	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gatewayapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stretchr/testify/require"
)

func TestNativeDeliveryRequiresItsExactTransportAuthority(t *testing.T) {
	token, err := hmessaging.NativeRunnerToken("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	require.NoError(t, err)
	payload, err := json.Marshal(rollout.SignedCommand{Payload: json.RawMessage(`{"id":"deployment"}`), Signature: "opaque-to-envelope-validation"})
	require.NoError(t, err)
	envelope := hmessaging.CommandEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1, CommandID: "command", OrganizationID: "org", RunnerID: token, DeploymentID: "deployment", Type: CommandType, Payload: payload}
	for _, scope := range []string{token, "site", "0-bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"} {
		t.Run(scope, func(t *testing.T) {
			candidate := envelope
			candidate.RunnerID = scope
			body, marshalErr := json.Marshal(candidate)
			require.NoError(t, marshalErr)
			_, _, decodeErr := decodeDelivery(gatewayapi.CommandResponse{Command: body}, "org", token)
			if scope == token {
				require.NoError(t, decodeErr)
				return
			}
			var refusal *rollout.Error
			require.ErrorAs(t, decodeErr, &refusal)
			require.Equal(t, "command_scope_refused", refusal.Code)
		})
	}
}
