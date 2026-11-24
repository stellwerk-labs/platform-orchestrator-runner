//go:build integration && (linux || darwin)

package rolloutagent

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/signedcontrol"
	"github.com/stretchr/testify/require"
)

func TestHANativePostRepairAfterExpirySurvivesProcessReplacement(t *testing.T) {
	f := newHAProcessFixture(t)
	receiptPath := f.wire.addReceiptTask(t)
	f.config["tools"] = f.wire.config.Executor.Tools
	first := f.start(t, "worker-0", true)
	command := f.command(t, "apply")
	command.ExpiresAt = time.Now().UTC().Add(5 * time.Second)
	f.publish(t, command)
	failed := f.progress(t, command.ID, "post_failed")
	before, err := os.ReadFile(receiptPath)
	require.NoError(t, err)
	first.kill(t)
	f.start(t, "worker-0", true)
	f.start(t, "worker-1", true)
	require.Eventually(t, func() bool { return time.Now().After(command.ExpiresAt) }, 6*time.Second, 20*time.Millisecond)
	// A live site slot accepts the signed control while separate OS workers own
	// actual execution. The Agent transport overlay is qualified separately.
	cfg := f.wire.config
	cfg.Directory = filepath.Join(f.root, "post-control-slot")
	cfg.RunnerUUID = f.runnerUUID
	pg := f.pg
	pg.WorkerID = "post-control-slot"
	cfg.Postgres = &pg
	control, err := rollout.Open(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, control.Close(closeCtx))
	})
	payload, err := json.Marshal(command)
	require.NoError(t, err)
	now := time.Now().UTC()
	request := signedcontrol.Request{Version: signedcontrol.Version, ID: uuid.NewString(), RolloutID: command.ID, OrgID: command.OrgID, EnvironmentID: command.EnvironmentID, Target: signedcontrol.Target(*command.NativeTarget), ExecutionDigest: signedcontrol.Digest(payload), ActorID: uuid.NewString(), Action: "repair", FailedStep: failed.FailedStep, Revision: failed.Revision, IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	signed, err := json.Marshal(signedcontrol.Signed{Payload: encoded, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(f.wire.key, encoded))})
	require.NoError(t, err)
	accepted, err := control.AcceptControl(t.Context(), signed)
	require.NoError(t, err)
	var outcome signedcontrol.Result
	require.NoError(t, json.Unmarshal([]byte(accepted.Result), &outcome))
	require.Equal(t, "applied", outcome.Outcome)
	completed := f.progress(t, command.ID, "completed")
	require.Equal(t, request.ID, completed.PostRepair.ControlID)
	after, err := os.ReadFile(receiptPath)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, 1, f.wire.operations(t, "apply"))
	f.stateMarker(t, command.ID)
	replay, err := control.AcceptControl(t.Context(), signed)
	require.NoError(t, err)
	require.Equal(t, accepted, replay)
	require.NoError(t, control.AcknowledgeControl(t.Context(), request.ID, accepted.ResultDigest))
}
