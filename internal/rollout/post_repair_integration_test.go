//go:build integration

package rollout

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/signedcontrol"
	"github.com/stretchr/testify/require"
)

func postRepairRequest(t *testing.T, f *fixture, p Progress, execution SignedCommand, duration time.Duration) signedcontrol.Request {
	t.Helper()
	now := time.Now().UTC()
	return signedcontrol.Request{Version: signedcontrol.Version, ID: uuid.NewString(), RolloutID: p.ID, OrgID: f.command.OrgID, EnvironmentID: p.EnvironmentID, Target: signedcontrol.Target(*f.command.NativeTarget), ExecutionDigest: digest(execution.Payload), ActorID: uuid.NewString(), Action: "repair", FailedStep: p.FailedStep, Revision: p.Revision, IssuedAt: now, ExpiresAt: now.Add(duration)}
}

func TestPostgresNativePostRepairOutlivesOriginalWithoutRepeatingApply(t *testing.T) {
	for _, retry := range []string{"receipt", "read_only"} {
		t.Run(retry, func(t *testing.T) {
			_, err := exec.LookPath("tofu")
			require.NoError(t, err)
			f, trace, _ := canonicalFixture(t, "apply", "opentofu")
			root := t.TempDir()
			calls := filepath.Join(root, "calls")
			permit := filepath.Join(root, "permit")
			addTask(t, f, "post_apply", retry, fmt.Sprintf(`import json,sys,pathlib
r=json.load(sys.stdin)
p=pathlib.Path(%q)
with p.open('a') as f: f.write(r['task_id']+':'+r['effect_id']+'\n')
if r['task_id']=='task-1' and not pathlib.Path(%q).exists(): sys.exit(1)
print(json.dumps({'version':1,'task_id':r['task_id'],'effect_id':r['effect_id'],'decision':'allow','category':'ok'}))`, calls, permit))
			f.command.Steps = append(f.command.Steps, rolloutexec.Step{ID: "task-2", ToolID: "task", Parameters: json.RawMessage(`{}`)})
			admin := openPostgresFixture(t, f)
			restrictControlRuntime(t, f, admin)
			f.command.ID, f.command.EnvironmentID = uuid.NewString(), uuid.NewString()
			f.command.Request.ID, f.command.Request.EnvironmentID = f.command.ID, f.command.EnvironmentID
			managed, err := age.GenerateX25519Identity()
			require.NoError(t, err)
			f.command.Request.EncryptedLogsRecipient = managed.Recipient().String()
			f.command.ExpiresAt = time.Now().UTC().Add(5 * time.Second)
			execution := f.signed(t, f.command)
			_, err = f.engine.Submit(t.Context(), execution)
			require.NoError(t, err)
			failed := waitPhase(t, f.engine, f.command.ID, "post_failed")
			require.NotNil(t, failed.LogCapture)
			require.Equal(t, nativelogs.ApplyPhase, failed.LogCapture.Authority.Phase)
			require.Equal(t, digest(execution.Payload), failed.LogCapture.Authority.CommandDigest)
			frozen := nativeCaptureDelivery(t, f, failed.ID, failed.LogCapture.Authority.CaptureRevision)
			require.NotNil(t, frozen.Log)
			captured, err := nativelogs.Decode(frozen.Log.Ciphertext, managed, frozen.Log.Descriptor.Authority)
			require.NoError(t, err)
			require.NotEmpty(t, captured.Logs)
			state, err := os.ReadFile(f.state)
			require.NoError(t, err)
			request := postRepairRequest(t, f, failed, execution, time.Minute)
			schema := pq.QuoteIdentifier(f.config.Postgres.ApplicationSchema)
			_, err = admin.Exec(`ALTER TABLE ` + schema + `.rollout_decisions ADD CONSTRAINT reject_post_repair CHECK(kind<>'repair')`)
			require.NoError(t, err)
			_, err = f.engine.AcceptControl(t.Context(), signNativeControl(t, f, request))
			require.Error(t, err)
			unchanged, err := f.engine.Get(t.Context(), failed.ID)
			require.NoError(t, err)
			require.Equal(t, failed, unchanged)
			missing, err := f.engine.readControl(t.Context(), request.ID)
			require.NoError(t, err)
			require.Nil(t, missing)
			_, err = admin.Exec(`ALTER TABLE ` + schema + `.rollout_decisions DROP CONSTRAINT reject_post_repair`)
			require.NoError(t, err)
			reopenPostgres(t, f)
			first, err := f.engine.AcceptControl(t.Context(), signNativeControl(t, f, request))
			require.NoError(t, err)
			require.Equal(t, "applied", receiptResult(t, first).Outcome)
			require.Eventually(t, func() bool {
				p, e := f.engine.Get(t.Context(), failed.ID)
				return e == nil && p.Phase == "post_failed" && p.Revision > failed.Revision
			}, 5*time.Second, 20*time.Millisecond)
			waiting, err := f.engine.Get(t.Context(), failed.ID)
			require.NoError(t, err)
			require.Equal(t, request.ID, waiting.PostRepair.ControlID)
			beforeCalls, err := os.ReadFile(calls)
			require.NoError(t, err)
			for _, wake := range []string{"", "wrong", waiting.FailedStep, waiting.FailedStep} {
				require.NoError(t, dbos.Send(f.engine.durable, failed.ID, wake, "repair"))
			}
			require.Never(t, func() bool {
				p, e := f.engine.Get(t.Context(), failed.ID)
				return e != nil || p.Revision != waiting.Revision
			}, 200*time.Millisecond, 20*time.Millisecond)
			afterCalls, err := os.ReadFile(calls)
			require.NoError(t, err)
			require.Equal(t, beforeCalls, afterCalls)
			require.Eventually(t, func() bool { return time.Now().After(f.command.ExpiresAt) }, 6*time.Second, 20*time.Millisecond)
			expired := postRepairRequest(t, f, waiting, execution, time.Minute)
			expired.IssuedAt = time.Now().UTC().Add(-2 * time.Second)
			expired.ExpiresAt = expired.IssuedAt.Add(time.Second)
			refusal, err := f.engine.AcceptControl(t.Context(), signNativeControl(t, f, expired))
			require.NoError(t, err)
			require.Equal(t, "control_expired", receiptResult(t, refusal).Code)
			stillWaiting, err := f.engine.Get(t.Context(), failed.ID)
			require.NoError(t, err)
			require.Equal(t, waiting, stillWaiting)
			require.NoError(t, os.WriteFile(permit, []byte("allow"), 0600))
			latest := postRepairRequest(t, f, waiting, execution, 2*time.Second)
			contender := latest
			contender.ID = uuid.NewString()
			requests := []signedcontrol.Request{latest, contender}
			type controlReply struct {
				index   int
				receipt signedcontrol.Receipt
				err     error
			}
			replies := make(chan controlReply, 2)
			for index, candidate := range requests {
				wire := signNativeControl(t, f, candidate)
				go func(index int, wire []byte) {
					receipt, acceptErr := f.engine.AcceptControl(t.Context(), wire)
					replies <- controlReply{index, receipt, acceptErr}
				}(index, wire)
			}
			applied := 0
			for range 2 {
				reply := <-replies
				require.NoError(t, reply.err)
				outcome := receiptResult(t, reply.receipt)
				if outcome.Outcome == "applied" {
					applied++
					latest = requests[reply.index]
				} else {
					require.Equal(t, "repair_revision_conflict", outcome.Code)
				}
			}
			require.Equal(t, 1, applied)
			completed := waitPhase(t, f.engine, failed.ID, "completed")
			require.Equal(t, latest.ID, completed.PostRepair.ControlID)
			require.Len(t, completed.Steps, 2)
			require.Equal(t, failed.LogCapture, completed.LogCapture)
			require.Equal(t, frozen, nativeCaptureDelivery(t, f, failed.ID, failed.LogCapture.Authority.CaptureRevision))
			var storedPayload []byte
			require.NoError(t, f.engine.db.QueryRow(`SELECT payload FROM rollout_commands WHERE id=$1`, failed.ID).Scan(&storedPayload))
			plain, err := f.engine.decrypt(storedPayload)
			require.NoError(t, err)
			require.Equal(t, []byte(execution.Payload), plain, "post repair must retain original signed bytes")
			var checkpoint string
			require.NoError(t, f.engine.db.QueryRow(`SELECT coalesce(string_agg(output::text,''),'')||coalesce(string_agg(error::text,''),'') FROM `+pq.QuoteIdentifier(f.config.Postgres.SystemSchema)+`.operation_outputs WHERE workflow_uuid=$1`, failed.ID).Scan(&checkpoint))
			require.NotContains(t, checkpoint, string(captured.Logs))
			retained, err := f.engine.command(t.Context(), failed.ID)
			require.NoError(t, err)
			require.JSONEq(t, string(execution.Payload), string(f.signed(t, retained).Payload))
			afterState, err := os.ReadFile(f.state)
			require.NoError(t, err)
			require.Equal(t, state, afterState)
			require.Equal(t, 1, operationCount(t, trace, "apply"))
			require.Eventually(t, func() bool { return time.Now().After(latest.ExpiresAt) }, 3*time.Second, 20*time.Millisecond)
			replay, err := f.engine.AcceptControl(t.Context(), signNativeControl(t, f, request))
			require.NoError(t, err)
			require.Equal(t, first, replay)
			after, err := f.engine.Get(t.Context(), failed.ID)
			require.NoError(t, err)
			require.Equal(t, completed, after)
			require.Equal(t, frozen, nativeCaptureDelivery(t, f, failed.ID, failed.LogCapture.Authority.CaptureRevision))
			var gates int
			require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_environments WHERE rollout=$1`, failed.ID).Scan(&gates))
			require.Zero(t, gates)
		})
	}
}

// Store a genuinely signed/validated command without enqueueing a workflow so
// the real Apply and post stages can be exercised at a precise commit boundary.
func storedPostApply(t *testing.T, f *fixture) {
	t.Helper()
	signed := f.signed(t, f.command)
	command, err := f.engine.verify(signed)
	require.NoError(t, err)
	require.NoError(t, f.engine.validate(command))
	encrypted, err := f.engine.encrypt(signed.Payload)
	require.NoError(t, err)
	p := Progress{ID: command.ID, EnvironmentID: command.EnvironmentID, Phase: "preparing", CreatedAt: time.Now().UTC(), Steps: []StepResult{}}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	_, err = f.engine.db.Exec(`INSERT INTO rollout_commands(id,digest,payload,progress,version,environment,strategy) VALUES($1,$2,$3,$4,$5,$6,$7)`, command.ID, digest(signed.Payload), encrypted, raw, command.Version, command.EnvironmentID, command.Strategy)
	require.NoError(t, err)
	_, err = f.engine.db.Exec(`INSERT INTO rollout_environments(environment,rollout) VALUES($1,$2)`, command.EnvironmentID, command.ID)
	require.NoError(t, err)
	require.NoError(t, f.engine.runStage(t.Context(), command.ID, "direct-apply", f.engine.directApply))
	p, err = f.engine.Get(t.Context(), command.ID)
	require.NoError(t, err)
	require.Equal(t, "applied", p.Phase)
}

func TestPostgresNativePostExpiryAfterCommittedApplyPausesOnlySafeWork(t *testing.T) {
	for _, retry := range []string{"receipt", "unsafe", "none"} {
		t.Run(retry, func(t *testing.T) {
			_, err := exec.LookPath("tofu")
			require.NoError(t, err)
			f, trace, _ := canonicalFixture(t, "apply", "opentofu")
			marker := filepath.Join(t.TempDir(), "never-started")
			if retry != "none" {
				addTask(t, f, "post_apply", retry, fmt.Sprintf("import pathlib;pathlib.Path(%q).write_text('unexpected')", marker))
			}
			openPostgresFixture(t, f)
			f.command.ExpiresAt = time.Now().UTC().Add(3 * time.Second)
			storedPostApply(t, f)
			require.Eventually(t, func() bool { return time.Now().After(f.command.ExpiresAt) }, 4*time.Second, 20*time.Millisecond)
			require.NoError(t, f.engine.runStage(t.Context(), f.command.ID, "post-tasks", f.engine.post))
			p, err := f.engine.Get(t.Context(), f.command.ID)
			require.NoError(t, err)
			switch retry {
			case "receipt":
				require.Equal(t, "post_failed", p.Phase)
				require.Equal(t, "post_authority_expired", p.ErrorCode)
				require.Equal(t, "task-1", p.FailedStep)
			case "unsafe":
				require.Equal(t, "outcome_unknown", p.Phase)
			case "none":
				require.Equal(t, "completed", p.Phase)
			}
			require.NoFileExists(t, marker)
			require.Equal(t, 1, operationCount(t, trace, "apply"))
		})
	}
}

func TestPostgresNativePostWaitContinuesAfterEmptyTimeout(t *testing.T) {
	f, _, _ := postgresFixture(t, "apply")
	closeFixture(t, f.engine)
	durable, err := dbos.NewContext(context.Background(), dbos.Config{AppName: "owned-post-repair-wait", DatabaseURL: f.engine.postgresDSN, DatabaseSchema: f.config.Postgres.SystemSchema, ApplicationVersion: f.engine.runtimeVersion(), ExecutorID: "post-wait:" + uuid.NewString(), SkipMigrations: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, dbos.Shutdown(durable, time.Second)) })
	workflow := func(ctx dbos.Context, expected string) (string, error) {
		return expected, waitPostRepair(ctx, expected, 20*time.Millisecond)
	}
	dbos.RegisterWorkflow(durable, workflow, dbos.WithWorkflowName("owned-post-repair-wait"))
	require.NoError(t, dbos.Launch(durable))
	id := uuid.NewString()
	handle, err := dbos.RunWorkflow(durable, workflow, "post", dbos.WithWorkflowID(id))
	require.NoError(t, err)
	time.Sleep(60 * time.Millisecond)
	require.NoError(t, dbos.Send(durable, id, "", "repair"))
	require.NoError(t, dbos.Send(durable, id, "wrong", "repair"))
	require.NoError(t, dbos.Send(durable, id, "post", "repair"))
	result, err := handle.GetResult(dbos.WithHandleTimeout(5 * time.Second))
	require.NoError(t, err)
	require.Equal(t, "post", result)
}

func TestPostgresNativePostAttemptExpiryBetweenTasksAndAfterLastCommit(t *testing.T) {
	for _, remaining := range []bool{true, false} {
		t.Run(fmt.Sprint("remaining=", remaining), func(t *testing.T) {
			_, err := exec.LookPath("tofu")
			require.NoError(t, err)
			f, trace, _ := canonicalFixture(t, "apply", "opentofu")
			calls := filepath.Join(t.TempDir(), "calls")
			addTask(t, f, "post_apply", "read_only", fmt.Sprintf(`import json,sys,pathlib
r=json.load(sys.stdin)
with pathlib.Path(%q).open('a') as f: f.write(r['task_id']+'\n')
print(json.dumps({'version':1,'task_id':r['task_id'],'effect_id':r['effect_id'],'decision':'allow','category':'ok'}))`, calls))
			if remaining {
				f.command.Steps = append(f.command.Steps, rolloutexec.Step{ID: "task-2", ToolID: "task", Parameters: json.RawMessage(`{}`)})
			}
			openPostgresFixture(t, f)
			f.command.ExecutionLimits.TaskSeconds = 1
			storedPostApply(t, f)
			bounded, cancel, err := f.engine.postStageContext(t.Context(), f.command.ID)
			require.NoError(t, err)
			defer cancel()
			selectedDeadline, ok := bounded.Deadline()
			require.True(t, ok)
			review, outputs, err := f.engine.taskInputs(t.Context(), f.command.ID)
			require.NoError(t, err)
			tool, ok := f.engine.toolForStep(f.command, "task-1")
			require.True(t, ok)
			require.NoError(t, f.engine.task(bounded, f.command, f.command.Steps[0], tool, review, outputs))
			require.Eventually(t, func() bool { return bounded.Err() != nil }, 2*time.Second, 20*time.Millisecond)
			err = f.engine.post(bounded, f.command.ID)
			if remaining {
				var failure *operationDeadline
				require.ErrorAs(t, err, &failure)
				require.Equal(t, "task-2", failure.step)
				require.NoError(t, f.engine.recordOperationDeadline(t.Context(), f.command.ID, failure))
			} else {
				require.NoError(t, err)
			}
			p, err := f.engine.Get(t.Context(), f.command.ID)
			require.NoError(t, err)
			if remaining {
				require.Equal(t, "post_failed", p.Phase)
				require.Equal(t, "task-2", p.FailedStep)
			} else {
				require.Equal(t, "completed", p.Phase)
			}
			actualDeadline, ok := bounded.Deadline()
			require.True(t, ok)
			require.Equal(t, selectedDeadline, actualDeadline)
			data, err := os.ReadFile(calls)
			require.NoError(t, err)
			require.Equal(t, "task-1\n", string(data))
			require.Equal(t, 1, operationCount(t, trace, "apply"))
		})
	}
}

func TestPostgresNativePostRepairGrantDeadlineCancelsActualTask(t *testing.T) {
	_, err := exec.LookPath("tofu")
	require.NoError(t, err)
	f, trace, _ := canonicalFixture(t, "apply", "opentofu")
	root := t.TempDir()
	mode := filepath.Join(root, "mode")
	late := filepath.Join(root, "late-child")
	addTask(t, f, "post_apply", "receipt", fmt.Sprintf(`import json,sys,pathlib,time,subprocess
r=json.load(sys.stdin)
p=pathlib.Path(%q)
if not p.exists(): sys.exit(1)
if p.read_text()=='sleep':
 child="import time,pathlib;time.sleep(4);pathlib.Path("+repr(%q)+").write_text('unexpected')"
 subprocess.Popen([sys.executable,'-c',child],stdin=subprocess.DEVNULL)
 time.sleep(60)
print(json.dumps({'version':1,'task_id':r['task_id'],'effect_id':r['effect_id'],'decision':'allow','category':'ok'}))`, mode, late))
	admin := openPostgresFixture(t, f)
	restrictControlRuntime(t, f, admin)
	f.command.ID, f.command.EnvironmentID = uuid.NewString(), uuid.NewString()
	f.command.Request.ID, f.command.Request.EnvironmentID = f.command.ID, f.command.EnvironmentID
	execution := f.signed(t, f.command)
	_, err = f.engine.Submit(t.Context(), execution)
	require.NoError(t, err)
	failed := waitPhase(t, f.engine, f.command.ID, "post_failed")
	require.NoError(t, os.WriteFile(mode, []byte("sleep"), 0600))
	request := postRepairRequest(t, f, failed, execution, 2*time.Second)
	receipt, err := f.engine.AcceptControl(t.Context(), signNativeControl(t, f, request))
	require.NoError(t, err)
	require.Equal(t, "applied", receiptResult(t, receipt).Outcome)
	require.Eventually(t, func() bool {
		p, e := f.engine.Get(t.Context(), failed.ID)
		return e == nil && p.Phase == "post_failed" && p.ErrorCode == "task_timed_out"
	}, 4*time.Second, 20*time.Millisecond)
	timedOut, err := f.engine.Get(t.Context(), failed.ID)
	require.NoError(t, err)
	require.True(t, timedOut.InfrastructureApplied)
	require.Equal(t, request.ID, timedOut.PostRepair.ControlID)
	require.NoError(t, os.WriteFile(mode, []byte("allow"), 0600))
	latest := postRepairRequest(t, f, timedOut, execution, time.Minute)
	_, err = f.engine.AcceptControl(t.Context(), signNativeControl(t, f, latest))
	require.NoError(t, err)
	waitPhase(t, f.engine, failed.ID, "completed")
	require.Equal(t, 1, operationCount(t, trace, "apply"))
	time.Sleep(3 * time.Second)
	require.NoFileExists(t, late, "expired grant left a descendant capable of a late effect")
}
