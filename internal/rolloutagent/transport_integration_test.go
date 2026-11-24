//go:build integration

package rolloutagent

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gatewayapi"
	api "github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stretchr/testify/require"
)

func TestAgentFrozenResultSurvivesResponseLossAndBrokerRestart(t *testing.T) {
	f := newTransportFixture(t, "apply")
	stream, err := f.js.StreamInfo(hmessaging.RunnerEventsStreamName)
	require.NoError(t, err)
	stream.Config.Duplicates = time.Second
	_, err = f.js.UpdateStream(&stream.Config)
	require.NoError(t, err)
	_, err = f.engine.Submit(t.Context(), f.signed(t))
	require.NoError(t, err)
	f.wait(t, "completed")
	events, err := f.engine.Events(t.Context(), 0)
	require.NoError(t, err)
	last := events[len(events)-1]
	frozen, err := f.engine.PrepareDelivery(t.Context(), last.ID)
	require.NoError(t, err)
	f.dropAfter.Store("deployment-result")
	operation, err := publishEvent(t.Context(), f.engine, f.client, last)
	require.Equal(t, "publish_progress", operation)
	require.Error(t, err, "Gateway persisted the result but its HTTP response must be lost")
	subject, err := hmessaging.RunnerEventSubject(f.config.OrgID, f.config.RunnerID, "deployment-result")
	require.NoError(t, err)
	first, err := f.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
	require.NoError(t, err)
	require.Equal(t, []byte(frozen.Result), first.Data)
	pending, err := f.engine.Events(t.Context(), last.ID-1)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	f.closeEngine(t)
	restartOwnedBroker(t)
	require.Eventually(t, f.connection.IsConnected, 10*time.Second, 100*time.Millisecond)
	f.engine, err = rollout.Open(t.Context(), f.config)
	require.NoError(t, err)
	time.Sleep(2 * time.Second) // Exercise actual publication beyond the 1s broker dedup window.
	operation, err = publishEvent(t.Context(), f.engine, f.client, last)
	require.Equal(t, "ack_event", operation)
	require.NoError(t, err)
	replayed, err := f.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
	require.NoError(t, err)
	require.Greater(t, replayed.Sequence, first.Sequence)
	require.Equal(t, first.Data, replayed.Data)
	require.NotContains(t, string(replayed.Data), "PRIVATE_AGENT_DEPLOYMENT_TOKEN")
	require.NotContains(t, string(replayed.Data), "PRIVATE_AGENT_VALUE")
	pending, err = f.engine.Events(t.Context(), last.ID-1)
	require.NoError(t, err)
	require.Empty(t, pending)
	require.Zero(t, f.legacyResults.Load(), "native delivery called the timestamp-regenerating legacy route")
	require.Equal(t, 1, f.operations(t, "apply"))
}

func restartOwnedBroker(t *testing.T) {
	t.Helper()
	container := os.Getenv("AGENT_DELIVERY_NATS_CONTAINER")
	require.NotEmpty(t, container, "restart proof requires the script-owned Docker fixture")
	check := exec.CommandContext(t.Context(), "docker", "inspect", "--format={{index .Config.Labels \"com.stellwerk.test\"}}", container)
	label, err := check.CombinedOutput()
	require.NoError(t, err)
	require.Equal(t, "agent-delivery\n", string(label))
	output, err := exec.CommandContext(t.Context(), "docker", "restart", container).CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestAgentLostAckReplaysAcceptedCommandAfterRestart(t *testing.T) {
	f := newTransportFixture(t, "reviewed")
	delivery := f.deliver(t, f.signed(t))
	f.dropBefore.Store("ack")
	operation, err := handleDelivery(t.Context(), f.engine, f.client, f.config.OrgID, f.config.RunnerID, delivery)
	require.Equal(t, "ack_command", operation)
	require.Error(t, err)
	f.wait(t, "awaiting_approval")
	f.closeEngine(t)
	f.config.Executor.Capabilities = []string{"changed-catalogue"}
	f.engine, err = rollout.Open(t.Context(), f.config)
	require.NoError(t, err)
	// The original action did not reach Gateway. A real NAK makes the broker
	// redeliver now, before the still-valid outer command/90s delivery expiry.
	require.NoError(t, f.client.Retry(t.Context(), "pipeline:"+f.command.ID, delivery.Receipt, 0))
	replayed, err := f.client.NextCommand(t.Context())
	require.NoError(t, err)
	require.Greater(t, replayed.Attempt, delivery.Attempt)
	require.NotEqual(t, delivery.Receipt, replayed.Receipt)
	require.Equal(t, delivery.Command, replayed.Command)
	operation, err = handleDelivery(t.Context(), f.engine, f.client, f.config.OrgID, f.config.RunnerID, replayed)
	require.Equal(t, "ack_command", operation)
	require.NoError(t, err)
	require.Equal(t, "awaiting_approval", f.wait(t, "awaiting_approval").Phase)
	require.Equal(t, 1, f.operations(t, "plan"))
	require.Zero(t, f.operations(t, "apply"))
}

func TestAgentCommandActionErrorsAreReturned(t *testing.T) {
	for _, action := range []string{"ack", "reject", "retry"} {
		t.Run(action, func(t *testing.T) {
			f := newTransportFixture(t, "reviewed")
			signed := f.signed(t)
			if action == "reject" {
				signed.Signature = "invalid-signature"
			}
			delivery := f.deliver(t, signed)
			bad := f.clientConfig
			_, otherKey, err := ed25519.GenerateKey(rand.Reader)
			require.NoError(t, err)
			otherDER, err := x509.MarshalPKCS8PrivateKey(otherKey)
			require.NoError(t, err)
			bad.PrivateKey = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: otherDER})
			client, err := gatewayapi.NewClient(bad)
			require.NoError(t, err)
			var locked *sql.Tx
			if action == "retry" {
				db, err := sql.Open("sqlite", "file:"+filepath.Join(f.config.Directory, "rollouts.sqlite"))
				require.NoError(t, err)
				defer func() { _ = db.Close() }()
				locked, err = db.BeginTx(t.Context(), nil)
				require.NoError(t, err)
				defer func() { _ = locked.Rollback() }()
				_, err = locked.Exec(`UPDATE rollout_site SET runner=runner`)
				require.NoError(t, err)
			}
			operation, err := handleDelivery(t.Context(), f.engine, client, f.config.OrgID, f.config.RunnerID, delivery)
			require.Equal(t, action+"_command", operation)
			require.Error(t, err, "transport action failure was discarded")
			var status *gatewayapi.HTTPStatusError
			require.ErrorAs(t, err, &status)
			require.Equal(t, http.StatusUnauthorized, status.StatusCode)
			if locked != nil {
				require.NoError(t, locked.Rollback())
			}
		})
	}
}

func TestAgentExpiredReceiptDoesNotConfirmAcceptedAck(t *testing.T) {
	f := newTransportFixture(t, "reviewed")
	delivery := f.deliver(t, f.signed(t))
	f.clockOffset.Store(int64(2 * time.Minute)) // Real Gateway validation with a controlled server clock.
	operation, err := handleDelivery(t.Context(), f.engine, f.client, f.config.OrgID, f.config.RunnerID, delivery)
	require.Equal(t, "ack_command", operation)
	var status *gatewayapi.HTTPStatusError
	require.ErrorAs(t, err, &status)
	require.Equal(t, http.StatusBadRequest, status.StatusCode)
	f.wait(t, "awaiting_approval")
	require.Zero(t, f.operations(t, "apply"))
}

func TestAgentPostTaskRepairPublishesFirstSuccessfulResult(t *testing.T) {
	f := newTransportFixture(t, "apply")
	receipt := f.addReceiptTask(t)
	_, err := f.engine.Submit(t.Context(), f.signed(t))
	require.NoError(t, err)
	failed := f.wait(t, "post_failed")
	events, err := f.engine.Events(t.Context(), 0)
	require.NoError(t, err)
	for _, event := range events {
		_, err := publishEvent(t.Context(), f.engine, f.client, event)
		require.NoError(t, err)
	}
	subject, err := hmessaging.RunnerEventSubject(f.config.OrgID, f.config.RunnerID, "deployment-result")
	require.NoError(t, err)
	_, err = f.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
	require.Error(t, err, "failed post tasks prematurely completed the ordinary Deployment")
	f.closeEngine(t)
	f.engine, err = rollout.Open(t.Context(), f.config)
	require.NoError(t, err)
	_, err = f.engine.Repair(t.Context(), failed.ID, failed.FailedStep, "operator")
	require.NoError(t, err)
	f.wait(t, "completed")
	events, err = f.engine.Events(t.Context(), 0)
	require.NoError(t, err)
	for _, event := range events {
		_, err := publishEvent(t.Context(), f.engine, f.client, event)
		require.NoError(t, err)
	}
	message, err := f.js.GetLastMsg(hmessaging.RunnerEventsStreamName, subject)
	require.NoError(t, err)
	var envelope hmessaging.EventEnvelope
	require.NoError(t, json.Unmarshal(message.Data, &envelope))
	var result api.DeploymentResultsUpdateBody
	require.NoError(t, json.Unmarshal(envelope.Payload, &result))
	require.Equal(t, api.Success, result.Status)
	require.Nil(t, result.NoEffectReceipt)
	require.Equal(t, 1, f.operations(t, "apply"))
	data, err := os.ReadFile(receipt)
	require.NoError(t, err)
	require.NotEmpty(t, data)
}
