package rolloutagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gatewayapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/signedcontrol"
)

const ControlCommandType = "control-rollout-v1"
const ControlEventType = "rollout-control-result"

// The outer envelope is transport routing only. Engine authenticates the original
// signed payload against captured execution authority before replay or mutation.
func decodeControlDelivery(data []byte, org, site string) (hmessaging.CommandEnvelope, error) {
	var command hmessaging.CommandEnvelope
	// Preserve only the Gateway-issued receipt route for a safe terminal reject.
	_ = json.NewDecoder(bytes.NewReader(data)).Decode(&command)
	invalid := &rollout.Error{Code: "invalid_control"}
	if len(data) == 0 || len(data) > signedcontrol.MaxBytes || !controlObject(data, "protocol_version command_id organization_id runner_id deployment_id type created_at expires_at payload") {
		return command, invalid
	}
	if !controlObject(command.Payload, "payload signature") {
		return command, invalid
	}
	var signed signedcontrol.Signed
	if json.Unmarshal(command.Payload, &signed) != nil {
		return command, invalid
	}
	request, err := signedcontrol.DecodeRequest(signed.Payload)
	if err != nil {
		return command, invalid
	}
	route, err := hmessaging.NativeRunnerToken(request.Target.RunnerUUID)
	if err != nil || request.OrgID != org || site != route || !controlEnvelopeMatches(command, request, route) {
		return command, &rollout.Error{Code: "control_scope_refused"}
	}

	return command, nil
}

func controlEnvelopeMatches(command hmessaging.CommandEnvelope, request signedcontrol.Request, route string) bool {
	return command.ProtocolVersion == hmessaging.ProtocolVersionV1 && command.Type == ControlCommandType &&
		command.OrganizationID == request.OrgID && command.RunnerID == route && command.DeploymentID == request.RolloutID &&
		command.CommandID == "pipeline-control:"+request.ID && command.CreatedAt.Equal(request.IssuedAt) && command.ExpiresAt.Equal(request.ExpiresAt)
}

func controlObject(data []byte, names string) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	allowed := strings.Fields(names)
	seen := make(map[string]bool)
	for decoder.More() {
		token, err = decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] || !slices.Contains(allowed, name) {
			return false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(value, []byte("null")) {
			return false
		}
		seen[name] = true
	}
	_, err = decoder.Token()
	return err == nil && len(seen) == len(allowed) && decoder.Decode(new(any)) == io.EOF
}

func handleControlDelivery(ctx context.Context, engine *rollout.Engine, client *gatewayapi.Client, org, site string, delivery gatewayapi.CommandResponse) (string, error) {
	command, err := decodeControlDelivery(delivery.Command, org, site)
	if err == nil {
		_, err = engine.AcceptControl(ctx, command.Payload)
	}
	if err == nil {
		return "ack_command", client.Acknowledge(ctx, command.CommandID, delivery.Receipt)
	}
	var refusal *rollout.Error
	switch {
	case errors.As(err, &refusal):
		return "reject_command", client.Reject(ctx, command.CommandID, delivery.Receipt, refusal.Code)
	case errors.Is(err, signedcontrol.ErrInvalid), errors.Is(err, signedcontrol.ErrSignature):
		return "reject_command", client.Reject(ctx, command.CommandID, delivery.Receipt, "invalid_control")
	default:
		return "retry_command", client.Retry(ctx, command.CommandID, delivery.Receipt, time.Second)
	}
}

func controlEvent(delivery rollout.ControlDelivery) ([]byte, signedcontrol.Request, error) {
	encoded, err := json.Marshal(delivery.Receipt)
	if err != nil {
		return nil, signedcontrol.Request{}, err
	}
	receipt, request, _, err := signedcontrol.DecodeReceipt(encoded)
	if err != nil || delivery.CreatedAt.IsZero() {
		return nil, request, signedcontrol.ErrInvalid
	}
	route, err := hmessaging.NativeRunnerToken(request.Target.RunnerUUID)
	if err != nil {
		return nil, request, err
	}
	// CreatedAt is retained DBOS workflow metadata, so no publication attempt can
	// regenerate the wire timestamp or reserialize the immutable inner result.
	data, err := json.Marshal(hmessaging.EventEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1,
		EventID: "pipeline-control:" + request.ID + ":result", CommandID: "pipeline-control:" + request.ID,
		OrganizationID: request.OrgID, RunnerID: route, DeploymentID: request.RolloutID,
		Type: ControlEventType, CreatedAt: delivery.CreatedAt.UTC(), Payload: json.RawMessage(receipt.Result)})
	return data, request, err
}

func publishControl(ctx context.Context, engine *rollout.Engine, client *gatewayapi.Client, delivery rollout.ControlDelivery) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if !engine.Ready() {
		return "worker_stopped", errors.New("local_runtime_unavailable")
	}
	data, request, err := controlEvent(delivery)
	if err != nil {
		return "prepare_control", err
	}
	if err = client.PublishEventBytes(ctx, data); err != nil {
		return "publish_control", err
	}
	return "ack_control", engine.AcknowledgeControl(ctx, request.ID, delivery.Receipt.ResultDigest)
}

// Offsets are volatile sweeps, never durable delivery state. A status change may
// defer a row to the next sweep; only an accepted publication is acknowledged.
type controlSweep struct{ pending, failed, fullPages int }

func (s *controlSweep) flush(ctx context.Context, engine *rollout.Engine, client *gatewayapi.Client) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s.failed = s.page(ctx, engine, client, s.failed, true)
	s.pending = s.page(ctx, engine, client, s.pending, false)
}

func (s *controlSweep) page(ctx context.Context, engine *rollout.Engine, client *gatewayapi.Client, offset int, failed bool) int {
	rows, err := engine.ControlDeliveries(ctx, offset, failed)
	if err != nil {
		reportFailure("list_controls", err)
		return offset
	}
	if s.needsAttention(rows, failed, time.Now()) {
		slog.Warn("Native control delivery needs attention", "code", "control_delivery_attention", "failed", failed)
	}
	if len(rows) == 0 {
		return 0
	}
	if !failed {
		for _, row := range rows {
			if ctx.Err() != nil {
				break
			}
			if operation, err := publishControl(ctx, engine, client, row); err != nil {
				reportFailure(operation, err)
			}
		}
	}
	return offset + len(rows)
}

func (s *controlSweep) needsAttention(rows []rollout.ControlDelivery, failed bool, now time.Time) bool {
	if failed {
		return len(rows) > 0
	}
	if len(rows) == 100 {
		s.fullPages = min(s.fullPages+1, 10)
	} else {
		s.fullPages = 0
	}
	if s.fullPages >= 10 {
		return true
	}
	for _, row := range rows {
		if now.Sub(row.CreatedAt) >= 15*time.Minute {
			return true
		}
	}
	return false
}
