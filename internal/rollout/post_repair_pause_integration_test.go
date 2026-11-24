//go:build integration

package rollout

import (
	"bytes"
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

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stretchr/testify/require"
)

func TestPostgresNativePostRepairPausesRetainedAttemptUntilMatchingWake(t *testing.T) {
	_, err := exec.LookPath("tofu")
	require.NoError(t, err)
	f, trace, _ := canonicalFixture(t, "apply", "opentofu")
	root := t.TempDir()
	permit, calls := filepath.Join(root, "permit"), filepath.Join(root, "calls")
	addTask(t, f, "post_apply", "receipt", fmt.Sprintf(`import json,sys,pathlib
r=json.load(sys.stdin)
with pathlib.Path(%q).open('a') as f: f.write(r['task_id']+'\n')
if r['task_id']=='task-1' and not pathlib.Path(%q).exists(): sys.exit(1)
print(json.dumps({'version':1,'task_id':r['task_id'],'effect_id':r['effect_id'],'decision':'allow','category':'ok'}))`, calls, permit))
	f.command.Steps = append(f.command.Steps, rolloutexec.Step{ID: "task-2", ToolID: "task", Parameters: json.RawMessage(`{}`)})
	openPostgresFixture(t, f)
	storedPostApply(t, f)
	state, err := os.ReadFile(f.state)
	require.NoError(t, err)
	config := dbos.Config{AppName: "owned-post-repair-pause", DatabaseURL: f.engine.postgresDSN, DatabaseSchema: f.config.Postgres.SystemSchema, ApplicationVersion: f.engine.runtimeVersion(), ExecutorID: "post-pause:" + uuid.NewString(), SkipMigrations: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	durable, err := dbos.NewContext(context.Background(), config)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, dbos.Shutdown(durable, time.Second)) })
	workflow := func(ctx dbos.Context, id string) (reference, error) {
		ctx, err := f.engine.enterWorkflow(ctx, id)
		if err != nil {
			return reference{ID: id}, err
		}
		result, err := f.engine.stage(ctx, id, "post-tasks", func(attemptCtx context.Context, id string) error {
			// The real stage has admitted this attempt through postStageContext.
			if err := f.engine.tasks(attemptCtx, id, "post_apply"); err != nil {
				return err
			}
			failed, err := f.engine.Get(attemptCtx, id)
			if err != nil {
				return err
			}
			if failed.Phase != "post_failed" || failed.FailedStep != "task-1" {
				return fmt.Errorf("expected actual task-1 failure, got %s/%s", failed.Phase, failed.FailedStep)
			}
			if err := os.WriteFile(permit, []byte("allow"), 0600); err != nil {
				return err
			}
			retrying, err := f.engine.Repair(t.Context(), id, failed.FailedStep, "operator")
			if err != nil {
				return err
			}
			for _, pause := range []struct {
				name string
				run  func(context.Context, string) error
			}{
				{"task loop", f.engine.nativePostTasks},
				{"outer post", f.engine.nativePost},
			} {
				if err := pause.run(attemptCtx, id); err != nil {
					return fmt.Errorf("%s refused accepted repair instead of pausing: %w", pause.name, err)
				}
				paused, err := f.engine.Get(attemptCtx, id)
				if err != nil {
					return err
				}
				if paused.Phase != "post_retrying" || paused.FailedStep != retrying.FailedStep || paused.Revision != retrying.Revision || !paused.InfrastructureApplied {
					return fmt.Errorf("%s changed the paused repair progress", pause.name)
				}
				var owners int
				if err := f.engine.db.QueryRowContext(attemptCtx, `SELECT count(*) FROM rollout_environments WHERE rollout=$1`, id).Scan(&owners); err != nil {
					return err
				}
				if owners != 1 {
					return fmt.Errorf("%s released the paused environment owner", pause.name)
				}
				after, err := os.ReadFile(f.state)
				if err != nil {
					return err
				}
				if !bytes.Equal(state, after) {
					return fmt.Errorf("%s changed infrastructure while paused", pause.name)
				}
				actualCalls, err := os.ReadFile(calls)
				if err != nil {
					return err
				}
				if string(actualCalls) != "task-1\n" {
					return fmt.Errorf("%s ran post work before its matching wake", pause.name)
				}
			}
			return nil
		})
		if err != nil {
			return result, err
		}
		if result.Phase != "post_retrying" || result.FailedStep != "task-1" {
			return result, fmt.Errorf("expected paused checkpoint, got %s/%s (%s)", result.Phase, result.FailedStep, result.ErrorCode)
		}
		if err := waitPostRepair(ctx, result.FailedStep, time.Second); err != nil {
			return result, err
		}
		return f.engine.executePostTasks(ctx, id)
	}
	dbos.RegisterWorkflow(durable, workflow, dbos.WithWorkflowName("owned-post-repair-pause"))
	require.NoError(t, dbos.Launch(durable))
	handle, err := dbos.RunWorkflow(durable, workflow, f.command.ID, dbos.WithWorkflowID(f.command.ID))
	require.NoError(t, err)
	result, err := handle.GetResult(dbos.WithHandleTimeout(15 * time.Second))
	require.NoError(t, err)
	require.Equal(t, "completed", result.Phase)
	completed, err := f.engine.Get(t.Context(), f.command.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", completed.Phase)
	require.Empty(t, completed.FailedStep)
	require.True(t, completed.InfrastructureApplied)
	var owners int
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_environments WHERE rollout=$1`, f.command.ID).Scan(&owners))
	require.Zero(t, owners)
	after, err := os.ReadFile(f.state)
	require.NoError(t, err)
	require.Equal(t, state, after)
	require.Equal(t, 1, operationCount(t, trace, "apply"))
	actualCalls, err := os.ReadFile(calls)
	require.NoError(t, err)
	require.Equal(t, "task-1\ntask-1\ntask-2\n", string(actualCalls))
	steps, err := dbos.GetWorkflowSteps(durable, f.command.ID)
	require.NoError(t, err)
	received := 0
	for _, step := range steps {
		if step.StepName == "DBOS.recv" {
			require.NoError(t, step.Error)
			encoded, ok := step.Output.(string)
			require.True(t, ok)
			var failedStep string
			require.NoError(t, json.Unmarshal([]byte(encoded), &failedStep))
			require.Equal(t, "task-1", failedStep)
			received++
		}
	}
	require.Equal(t, 1, received)
}
