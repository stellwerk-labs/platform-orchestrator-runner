package gateway

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/signedcontrol"
	"github.com/stretchr/testify/require"
)

func TestNativeControlEventBindsMetadataAndReceiptIdentity(t *testing.T) {
	result := signedcontrol.Result{Version: signedcontrol.ResultVersion, ControlID: "11111111-1111-4111-8111-111111111111", RolloutID: "22222222-2222-4222-8222-222222222222", RequestDigest: strings.Repeat("a", 64), ExecutionDigest: strings.Repeat("b", 64), Outcome: "applied", Phase: "cancelled", Revision: 2}
	payload, err := json.Marshal(result)
	require.NoError(t, err)
	event := hmessaging.EventEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1, EventID: "pipeline-control:" + result.ControlID + ":result", CommandID: "pipeline-control:" + result.ControlID, DeploymentID: result.RolloutID, Type: "rollout-control-result", CreatedAt: time.Now().UTC(), Payload: payload}
	require.True(t, validNativeEventIdentity(event))
	for _, change := range []struct {
		name  string
		apply func(*hmessaging.EventEnvelope)
	}{
		{"control_id", func(e *hmessaging.EventEnvelope) {
			e.EventID = "pipeline-control:33333333-3333-4333-8333-333333333333:result"
		}},
		{"command_id", func(e *hmessaging.EventEnvelope) { e.CommandID = "different" }},
		{"rollout", func(e *hmessaging.EventEnvelope) { e.DeploymentID = "33333333-3333-4333-8333-333333333333" }},
		{"unknown_metadata", func(e *hmessaging.EventEnvelope) {
			e.Payload = []byte(strings.Replace(string(payload), `"version":`, `"private_body":"SECRET","version":`, 1))
		}},
		{"duplicate", func(e *hmessaging.EventEnvelope) {
			e.Payload = []byte(strings.Replace(string(payload), `"revision":`, `"revision":1,"revision":`, 1))
		}},
		{"alias", func(e *hmessaging.EventEnvelope) {
			e.Payload = []byte(strings.Replace(string(payload), `"control_id":`, `"Control_ID":`, 1))
		}},

		{"compound_code", func(e *hmessaging.EventEnvelope) {
			e.Payload = []byte(strings.Replace(string(payload), `"code":`, `"code phase":`, 1))
		}},
		{"escaped_compound_code", func(e *hmessaging.EventEnvelope) {
			e.Payload = []byte(strings.Replace(string(payload), `"code":`, `"code\u0020phase":`, 1))
		}},
		{"digest", func(e *hmessaging.EventEnvelope) {
			e.Payload = []byte(strings.Replace(string(payload), strings.Repeat("a", 64), "invalid", 1))
		}},
		{"time", func(e *hmessaging.EventEnvelope) { e.CreatedAt = time.Time{} }},
	} {
		t.Run(change.name, func(t *testing.T) {
			candidate := event
			change.apply(&candidate)
			require.False(t, validNativeEventIdentity(candidate))
		})
	}
}
