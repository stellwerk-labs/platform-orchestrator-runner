package rolloutagent

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/signedcontrol"
	"github.com/stretchr/testify/require"
)

func controlWireFixture(t *testing.T) (signedcontrol.Request, hmessaging.CommandEnvelope, []byte) {
	t.Helper()
	request := signedcontrol.Request{Version: signedcontrol.Version, ID: "11111111-1111-4111-8111-111111111111", RolloutID: "22222222-2222-4222-8222-222222222222", OrgID: "org", EnvironmentID: "33333333-3333-4333-8333-333333333333", ActorID: "44444444-4444-4444-8444-444444444444", Action: "cancel", ExecutionDigest: strings.Repeat("a", 64), IssuedAt: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2026, 11, 20, 13, 0, 0, 0, time.UTC), Target: signedcontrol.Target{PipelineVersion: rollout.NativePipelineVersion, RuntimeVersion: rollout.NativeRuntimeVersion, RunnerID: "site", RunnerUUID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", StateBackendType: "local", StateConfigurationDigest: strings.Repeat("b", 64), ToolsDigest: strings.Repeat("c", 64)}}
	payload, err := json.Marshal(request)
	require.NoError(t, err)
	signed, err := json.Marshal(signedcontrol.Signed{Payload: payload, Signature: "opaque-to-routing"})
	require.NoError(t, err)
	route, err := hmessaging.NativeRunnerToken(request.Target.RunnerUUID)
	require.NoError(t, err)
	command := hmessaging.CommandEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1, CommandID: "pipeline-control:" + request.ID, OrganizationID: request.OrgID, RunnerID: route, DeploymentID: request.RolloutID, Type: ControlCommandType, CreatedAt: request.IssuedAt, ExpiresAt: request.ExpiresAt, Payload: signed}
	wire, err := json.Marshal(command)
	require.NoError(t, err)
	return request, command, wire
}

func TestControlDeliveryRequiresExactOuterAndInnerIdentity(t *testing.T) {
	_, command, wire := controlWireFixture(t)
	decoded, err := decodeControlDelivery(wire, "org", command.RunnerID)
	require.NoError(t, err)
	require.Equal(t, command, decoded)
	for _, change := range []struct {
		name  string
		apply func(*hmessaging.CommandEnvelope)
	}{
		{"legacy_route", func(c *hmessaging.CommandEnvelope) { c.RunnerID = "site" }},
		{"other_uuid", func(c *hmessaging.CommandEnvelope) { c.RunnerID = "0-bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb" }},
		{"org", func(c *hmessaging.CommandEnvelope) { c.OrganizationID = "other" }},
		{"deployment", func(c *hmessaging.CommandEnvelope) { c.DeploymentID = "55555555-5555-4555-8555-555555555555" }},
		{"control", func(c *hmessaging.CommandEnvelope) {
			c.CommandID = "pipeline-control:55555555-5555-4555-8555-555555555555"
		}},
		{"issue", func(c *hmessaging.CommandEnvelope) { c.CreatedAt = c.CreatedAt.Add(time.Second) }},
		{"expiry", func(c *hmessaging.CommandEnvelope) { c.ExpiresAt = c.ExpiresAt.Add(time.Second) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			candidate := command
			change.apply(&candidate)
			data, err := json.Marshal(candidate)
			require.NoError(t, err)
			_, err = decodeControlDelivery(data, "org", command.RunnerID)
			require.Error(t, err)
		})
	}
}

func TestControlDeliveryRejectsAmbiguousOrOversizedCompleteInput(t *testing.T) {
	_, command, wire := controlWireFixture(t)
	for _, data := range [][]byte{
		bytes.Replace(wire, []byte(`"type":`), []byte(`"type":"control-rollout-v1","type":`), 1),
		bytes.Replace(wire, []byte(`"type":`), []byte(`"TYPE":`), 1),
		bytes.Replace(wire, []byte(`"type":`), []byte(`"unknown":0,"type":`), 1),
		bytes.Replace(wire, []byte(`"signature":`), []byte(`"Signature":`), 1),
		bytes.Replace(wire, []byte(`"action":`), []byte(`"action":"cancel","action":`), 1),
		bytes.Replace(wire, []byte(`"actor_id":`), []byte(`"Actor_ID":`), 1),
		append(append([]byte{}, wire...), []byte(` {}`)...),
		append(append([]byte{}, wire...), bytes.Repeat([]byte(" "), signedcontrol.MaxBytes)...),
	} {
		decoded, err := decodeControlDelivery(data, "org", command.RunnerID)
		require.Error(t, err)
		require.Equal(t, command.CommandID, decoded.CommandID, "codec rejection must keep the receipt route")
	}
}

func TestControlEventBytesAreStableFromRetainedReceiptTimestamp(t *testing.T) {
	request, _, _ := controlWireFixture(t)
	payload, err := json.Marshal(request)
	require.NoError(t, err)
	receipt, err := signedcontrol.Freeze(request, payload, "applied", "", "cancelled", 3)
	require.NoError(t, err)
	delivery := rollout.ControlDelivery{Receipt: receipt, Status: "ENQUEUED", CreatedAt: request.IssuedAt.Add(time.Second)}
	first, _, err := controlEvent(delivery)
	require.NoError(t, err)
	delivery.Status = "PENDING"
	repeated, _, err := controlEvent(delivery)
	require.NoError(t, err)
	require.Equal(t, first, repeated)
	var event hmessaging.EventEnvelope
	require.NoError(t, json.Unmarshal(first, &event))
	require.Equal(t, receipt.Result, string(event.Payload))
	require.Equal(t, "pipeline-control:"+request.ID+":result", event.EventID)
	require.Equal(t, delivery.CreatedAt, event.CreatedAt)
	require.NotContains(t, string(first), request.ActorID)
	require.NotContains(t, string(first), request.EnvironmentID)
	delivery.CreatedAt = time.Time{}
	_, _, err = controlEvent(delivery)
	require.Error(t, err)
	delivery.CreatedAt = request.IssuedAt
	delivery.Receipt.Result += " "
	_, _, err = controlEvent(delivery)
	require.Error(t, err)
}

func TestControlSweepAttentionIsBoundedAndKeepsIndependentOffsets(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	sweep := controlSweep{pending: 200, failed: 100}
	rows := make([]rollout.ControlDelivery, 100)
	for i := range rows {
		rows[i].CreatedAt = now
	}
	for range 9 {
		require.False(t, sweep.needsAttention(rows, false, now))
	}
	require.True(t, sweep.needsAttention(rows, false, now))
	for range 20 {
		require.True(t, sweep.needsAttention(rows, false, now))
	}
	require.Equal(t, 10, sweep.fullPages)
	require.True(t, sweep.needsAttention(rows[:1], true, now))
	require.Equal(t, 10, sweep.fullPages, "failed attention must not change the pending sweep")
	require.False(t, sweep.needsAttention(nil, true, now))
	require.False(t, sweep.needsAttention(nil, false, now))
	require.Zero(t, sweep.fullPages)
	rows[0].CreatedAt = now.Add(-15 * time.Minute)
	require.True(t, sweep.needsAttention(rows[:1], false, now))
	require.Equal(t, 200, sweep.pending)
	require.Equal(t, 100, sweep.failed)
}

func TestControlDeliveryRejectsCompoundFieldNames(t *testing.T) {
	request, command, _ := controlWireFixture(t)
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	payload, err := json.Marshal(request)
	require.NoError(t, err)
	command.Payload, err = json.Marshal(signedcontrol.Signed{Payload: payload, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload))})
	require.NoError(t, err)
	_, _, err = signedcontrol.Decode(command.Payload, public)
	require.NoError(t, err)
	wire, err := json.Marshal(command)
	require.NoError(t, err)
	for _, trial := range []struct {
		member      string
		replacement string
	}{
		{`"type":`, `"type created_at":`},
		{`"signature":`, `"payload signature":`},
	} {
		for _, escaped := range []bool{false, true} {
			replacement := trial.replacement
			if escaped {
				replacement = strings.ReplaceAll(replacement, " ", `\u0020`)
			}
			t.Run(replacement, func(t *testing.T) {
				bad := bytes.Replace(wire, []byte(trial.member), []byte(replacement), 1)
				decoded, err := decodeControlDelivery(bad, request.OrgID, command.RunnerID)
				require.EqualError(t, err, "invalid_control")
				require.Equal(t, command.CommandID, decoded.CommandID)
			})
		}
	}
}
