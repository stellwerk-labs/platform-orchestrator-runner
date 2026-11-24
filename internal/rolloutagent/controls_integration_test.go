//go:build integration && (linux || darwin)

package rolloutagent

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gatewayapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/natstransport"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/signedcontrol"
	"github.com/stretchr/testify/require"
)

func controlTransportFixture(t *testing.T) (*haProcessFixture, signedcontrol.Request) {
	t.Helper()
	f := newHAProcessFixture(t)
	f.wire.config.Directory = filepath.Join(f.root, "control-worker")
	f.wire.config.RunnerUUID = f.runnerUUID
	f.wire.config.Postgres = &f.pg
	require.NoError(t, rollout.InitializePostgres(t.Context(), f.wire.config))
	f.initialized = true
	var err error
	f.wire.engine, err = rollout.Open(t.Context(), f.wire.config)
	require.NoError(t, err)
	t.Cleanup(func() { f.wire.closeEngine(t) })
	f.wire.command = f.command(t, "reviewed")
	execution := f.wire.signed(t)
	_, err = f.wire.engine.Submit(t.Context(), execution)
	require.NoError(t, err)
	progress := f.wire.wait(t, "awaiting_approval")
	request := signedcontrol.Request{Version: signedcontrol.Version, ID: uuid.NewString(), RolloutID: f.wire.command.ID, OrgID: f.wire.config.OrgID, EnvironmentID: f.wire.command.EnvironmentID, Target: signedcontrol.Target(*f.wire.command.NativeTarget), ExecutionDigest: signedcontrol.Digest(execution.Payload), ActorID: uuid.NewString(), Action: "deny", Subject: progress.Subject, IssuedAt: time.Now().UTC(), ExpiresAt: f.wire.command.ExpiresAt}
	return f, request
}

func signedControlEnvelope(t *testing.T, f *haProcessFixture, request signedcontrol.Request) hmessaging.CommandEnvelope {
	t.Helper()
	payload, err := json.Marshal(request)
	require.NoError(t, err)
	body, err := json.Marshal(signedcontrol.Signed{Payload: payload, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(f.wire.key, payload))})
	require.NoError(t, err)
	route, err := hmessaging.NativeRunnerToken(f.runnerUUID)
	require.NoError(t, err)
	return hmessaging.CommandEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1, CommandID: "pipeline-control:" + request.ID, OrganizationID: request.OrgID, RunnerID: route, DeploymentID: request.RolloutID, Type: ControlCommandType, CreatedAt: request.IssuedAt, ExpiresAt: request.ExpiresAt, Payload: body}
}

func publishControlEnvelope(t *testing.T, f *haProcessFixture, envelope hmessaging.CommandEnvelope, wire []byte) {
	t.Helper()
	var err error
	if wire == nil {
		wire, err = json.Marshal(envelope)
		require.NoError(t, err)
	}
	subject, err := hmessaging.RunnerCommandSubject(envelope.OrganizationID, envelope.RunnerID)
	require.NoError(t, err)
	messageID, err := hmessaging.NativeMessageID(envelope.OrganizationID, envelope.RunnerID, "commands", envelope.Type, envelope.CommandID)
	require.NoError(t, err)
	publisher, err := natstransport.NewPublisher(f.wire.connection)
	require.NoError(t, err)
	require.NoError(t, publisher.Publish(t.Context(), hmessaging.Message{ID: messageID, Subject: subject, Data: wire, CreatedAt: envelope.CreatedAt, ExpiresAt: envelope.ExpiresAt}))
}

func TestHAControlLostCommandAckAndFrozenResultReplay(t *testing.T) {
	f, request := controlTransportFixture(t)
	stream, err := f.wire.js.StreamInfo(hmessaging.RunnerEventsStreamName)
	require.NoError(t, err)
	stream.Config.Duplicates = time.Second
	_, err = f.wire.js.UpdateStream(&stream.Config)
	require.NoError(t, err)
	envelope := signedControlEnvelope(t, f, request)
	publishControlEnvelope(t, f, envelope, nil)
	delivery, err := f.wire.client.NextCommand(t.Context())
	require.NoError(t, err)
	f.wire.dropBefore.Store("ack")
	operation, err := handleDelivery(t.Context(), f.wire.engine, f.wire.client, request.OrgID, envelope.RunnerID, delivery)
	require.Equal(t, "ack_command", operation)
	require.Error(t, err)
	f.wire.wait(t, "denied")
	f.wire.closeEngine(t)
	capabilities := f.wire.config.Executor.Capabilities
	f.wire.config.Executor.Capabilities = []string{"changed-catalogue"}
	changed, err := rollout.Open(t.Context(), f.wire.config)
	require.ErrorContains(t, err, "catalog_refused", "the owned execution profile cannot change across a replacement")
	require.Nil(t, changed)
	f.wire.config.Executor.Capabilities = capabilities
	f.wire.engine, err = rollout.Open(t.Context(), f.wire.config)
	require.NoError(t, err)
	require.NoError(t, f.wire.client.Retry(t.Context(), envelope.CommandID, delivery.Receipt, 0))
	delivery, err = f.wire.client.NextCommand(t.Context())
	require.NoError(t, err)
	operation, err = handleDelivery(t.Context(), f.wire.engine, f.wire.client, request.OrgID, envelope.RunnerID, delivery)
	require.Equal(t, "ack_command", operation)
	require.NoError(t, err)
	rows, err := f.wire.engine.ControlDeliveries(t.Context(), 0, false)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	f.wire.dropAfter.Store(ControlEventType)
	operation, err = publishControl(t.Context(), f.wire.engine, f.wire.client, rows[0])
	require.Equal(t, "publish_control", operation)
	require.Error(t, err)
	subject, err := hmessaging.RunnerEventSubject(request.OrgID, envelope.RunnerID, ControlEventType)
	require.NoError(t, err)
	first, err := f.wire.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
	require.NoError(t, err)
	f.wire.closeEngine(t)
	f.wire.engine, err = rollout.Open(t.Context(), f.wire.config)
	require.NoError(t, err)
	time.Sleep(1100 * time.Millisecond) // Pass the actual broker deduplication window.
	rows, err = f.wire.engine.ControlDeliveries(t.Context(), 0, false)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	operation, err = publishControl(t.Context(), f.wire.engine, f.wire.client, rows[0])
	require.Equal(t, "ack_control", operation)
	require.NoError(t, err)
	repeated, err := f.wire.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
	require.NoError(t, err)
	require.Greater(t, repeated.Sequence, first.Sequence)
	require.Equal(t, first.Data, repeated.Data)
	var event hmessaging.EventEnvelope
	require.NoError(t, json.Unmarshal(repeated.Data, &event))
	require.Equal(t, rows[0].Receipt.Result, string(event.Payload))
	expected, err := hmessaging.NativeMessageID(request.OrgID, envelope.RunnerID, "events", ControlEventType, event.EventID)
	require.NoError(t, err)
	require.Equal(t, expected, repeated.Header.Get(nats.MsgIdHdr))
	require.Eventually(t, func() bool {
		pending, err := f.wire.engine.ControlDeliveries(t.Context(), 0, false)
		return err == nil && len(pending) == 0
	}, 10*time.Second, 25*time.Millisecond)
	var decisions int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM `+f.table("rollout_decisions")+` WHERE rollout=$1 AND kind='deny'`, request.RolloutID).Scan(&decisions))
	require.Equal(t, 1, decisions)
	require.Zero(t, f.wire.operations(t, "apply"))
	require.Zero(t, f.wire.legacyResults.Load())
	for _, secret := range []string{request.ActorID, request.Subject, "PRIVATE_AGENT_VALUE", "PRIVATE_AGENT_DEPLOYMENT_TOKEN", "no_effect_receipt"} {
		require.NotContains(t, string(repeated.Data), secret)
	}
}

func TestHAControlMalformedCodecRejectsWithoutActionOrReceipt(t *testing.T) {
	f, request := controlTransportFixture(t)
	envelope := signedControlEnvelope(t, f, request)
	wire, err := json.Marshal(envelope)
	require.NoError(t, err)
	wire = []byte(strings.Replace(string(wire), `"type":`, `"unknown":"PRIVATE_CONTROL_BODY","type":`, 1))
	publishControlEnvelope(t, f, envelope, wire)
	delivery, err := f.wire.client.NextCommand(t.Context())
	require.NoError(t, err)
	operation, err := handleDelivery(t.Context(), f.wire.engine, f.wire.client, request.OrgID, envelope.RunnerID, delivery)
	require.Equal(t, "reject_command", operation)
	require.NoError(t, err)
	request.ID = uuid.NewString()
	envelope = signedControlEnvelope(t, f, request)
	var invalid signedcontrol.Signed
	require.NoError(t, json.Unmarshal(envelope.Payload, &invalid))
	invalid.Signature = "AA=="
	envelope.Payload, err = json.Marshal(invalid)
	require.NoError(t, err)
	publishControlEnvelope(t, f, envelope, nil)
	delivery, err = f.wire.client.NextCommand(t.Context())
	require.NoError(t, err)
	operation, err = handleDelivery(t.Context(), f.wire.engine, f.wire.client, request.OrgID, envelope.RunnerID, delivery)
	require.Equal(t, "reject_command", operation)
	require.NoError(t, err)
	commandSubject, err := hmessaging.RunnerCommandSubject(request.OrgID, envelope.RunnerID)
	require.NoError(t, err)
	rejectedSubject, err := hmessaging.DeadLetterSubject(commandSubject)
	require.NoError(t, err)
	rejected, err := f.wire.js.GetLastMsg(hmessaging.DeadLettersStreamName, rejectedSubject)
	require.NoError(t, err)
	require.Equal(t, "invalid_control", rejected.Header.Get("Po-Dead-Letter-Reason"))
	rows, err := f.wire.engine.ControlDeliveries(t.Context(), 0, false)
	require.NoError(t, err)
	require.Empty(t, rows)
	progress, err := f.wire.engine.Get(t.Context(), request.RolloutID)
	require.NoError(t, err)
	require.Equal(t, "awaiting_approval", progress.Phase)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err = f.wire.client.NextCommand(ctx)
	require.ErrorIs(t, err, gatewayapi.ErrNoCommand)
}

func TestHAControlCommittedBeforeLostAckReplaysOnReplacement(t *testing.T) {
	f := newHAProcessFixture(t)
	first := f.start(t, "worker-0", true)
	command := f.command(t, "reviewed")
	f.publish(t, command)
	progress := f.progress(t, command.ID, "awaiting_approval")
	payload, err := json.Marshal(command)
	require.NoError(t, err)
	request := signedcontrol.Request{Version: signedcontrol.Version, ID: uuid.NewString(), RolloutID: command.ID, OrgID: command.OrgID, EnvironmentID: command.EnvironmentID, Target: signedcontrol.Target(*command.NativeTarget), ExecutionDigest: signedcontrol.Digest(payload), ActorID: uuid.NewString(), Action: "deny", Subject: progress.Subject, IssuedAt: time.Now().UTC(), ExpiresAt: command.ExpiresAt}
	route, err := hmessaging.NativeRunnerToken(f.runnerUUID)
	require.NoError(t, err)
	commandSubject, err := hmessaging.RunnerCommandSubject(command.OrgID, route)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		for info := range f.wire.js.ConsumersInfo(hmessaging.RunnerCommandsStreamName) {
			if info.Config.FilterSubject == commandSubject && info.NumAckPending == 0 && info.NumPending == 0 {
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond)
	f.wire.dropBefore.Store("ack")
	envelope := signedControlEnvelope(t, f, request)
	publishControlEnvelope(t, f, envelope, nil)
	f.progress(t, command.ID, "denied")
	require.Eventually(t, func() bool { return f.wire.lastAck.Load().(gatewayapi.ReceiptRequest).Receipt != "" }, 5*time.Second, 20*time.Millisecond)
	first.kill(t)
	// Redeliver through the real Gateway receipt rather than waiting for the
	// production 2-minute broker lease. The action remains committed across SIGKILL.
	ack := f.wire.lastAck.Load().(gatewayapi.ReceiptRequest)
	require.NotEmpty(t, ack.Receipt)
	require.NoError(t, f.wire.client.Retry(t.Context(), envelope.CommandID, ack.Receipt, 0))
	f.start(t, "worker-0", true)
	subject, err := hmessaging.RunnerEventSubject(command.OrgID, envelope.RunnerID, ControlEventType)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		event, err := f.wire.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
		if err != nil {
			return false
		}
		var body hmessaging.EventEnvelope
		return json.Unmarshal(event.Data, &body) == nil && body.CommandID == envelope.CommandID
	}, 15*time.Second, 25*time.Millisecond)
	require.Eventually(t, func() bool {
		for info := range f.wire.js.ConsumersInfo(hmessaging.RunnerCommandsStreamName) {
			if info.Config.FilterSubject == commandSubject && info.NumAckPending == 0 && info.NumPending == 0 {
				return true
			}
		}
		return false
	}, 10*time.Second, 25*time.Millisecond, "replacement did not acknowledge the exact redelivered command")
	var decisions int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM `+f.table("rollout_decisions")+` WHERE rollout=$1 AND kind='deny'`, command.ID).Scan(&decisions))
	require.Equal(t, 1, decisions)
	require.Zero(t, f.wire.operations(t, "apply"))
}

func TestHAControlSweepContinuesAfterPublicationFailureAndAcksEnqueued(t *testing.T) {
	f, request := controlTransportFixture(t)
	first := signedControlEnvelope(t, f, request)
	_, err := f.wire.engine.AcceptControl(t.Context(), first.Payload)
	require.NoError(t, err)
	time.Sleep(2 * time.Millisecond) // DBOS creation timestamps have millisecond resolution.
	request.ID = uuid.NewString()
	second := signedControlEnvelope(t, f, request)
	_, err = f.wire.engine.AcceptControl(t.Context(), second.Payload)
	require.NoError(t, err)
	rows, err := f.wire.engine.ControlDeliveries(t.Context(), 0, false)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "ENQUEUED", rows[1].Status, "the first receipt must occupy the single receipt worker")
	f.wire.dropBefore.Store(ControlEventType)
	sweep := controlSweep{}
	sweep.flush(t.Context(), f.wire.engine, f.wire.client)
	require.Equal(t, 2, sweep.pending)
	subject, err := hmessaging.RunnerEventSubject(request.OrgID, second.RunnerID, ControlEventType)
	require.NoError(t, err)
	stored, err := f.wire.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
	require.NoError(t, err)
	var event hmessaging.EventEnvelope
	require.NoError(t, json.Unmarshal(stored.Data, &event))
	require.Equal(t, second.CommandID, event.CommandID)
	sweep.flush(t.Context(), f.wire.engine, f.wire.client) // Empty page resets the volatile offset.
	require.Zero(t, sweep.pending)
	// Complete only the first receipt. The second must consume the notification
	// sent while it was ENQUEUED, without a second publication or acknowledgement.
	operation, err := publishControl(t.Context(), f.wire.engine, f.wire.client, rows[0])
	require.NoError(t, err)
	require.Equal(t, "ack_control", operation)
	require.Eventually(t, func() bool {
		rows, err := f.wire.engine.ControlDeliveries(t.Context(), 0, false)
		return err == nil && len(rows) == 0
	}, 10*time.Second, 25*time.Millisecond)
	require.Zero(t, f.wire.operations(t, "apply"))
}

func TestHAControlUnavailableStorageRetriesWithoutRejectingIntent(t *testing.T) {
	f, request := controlTransportFixture(t)
	envelope := signedControlEnvelope(t, f, request)
	publishControlEnvelope(t, f, envelope, nil)
	delivery, err := f.wire.client.NextCommand(t.Context())
	require.NoError(t, err)
	f.wire.closeEngine(t)
	operation, err := handleDelivery(t.Context(), f.wire.engine, f.wire.client, request.OrgID, envelope.RunnerID, delivery)
	require.Equal(t, "retry_command", operation)
	require.NoError(t, err)
	f.wire.engine, err = rollout.Open(t.Context(), f.wire.config)
	require.NoError(t, err)
	delivery, err = f.wire.client.NextCommand(t.Context())
	require.NoError(t, err)
	require.Greater(t, delivery.Attempt, uint64(1))
	operation, err = handleDelivery(t.Context(), f.wire.engine, f.wire.client, request.OrgID, envelope.RunnerID, delivery)
	require.Equal(t, "ack_command", operation)
	require.NoError(t, err)
	f.wire.wait(t, "denied")
	rows, err := f.wire.engine.ControlDeliveries(t.Context(), 0, false)
	require.NoError(t, err)
	require.Len(t, rows, 1)
}
