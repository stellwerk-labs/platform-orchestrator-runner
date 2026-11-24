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

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestPostgresNativePostRetryingCheckpointConsumesRepairAfterRecovery(t *testing.T) {
	_, err := exec.LookPath("tofu")
	require.NoError(t, err)
	f, trace, _ := canonicalFixture(t, "apply", "opentofu")
	permit := filepath.Join(t.TempDir(), "permit")
	addTask(t, f, "post_apply", "receipt", fmt.Sprintf(`import json,sys,pathlib
r=json.load(sys.stdin)
if not pathlib.Path(%q).exists(): sys.exit(1)
print(json.dumps({'version':1,'task_id':r['task_id'],'effect_id':r['effect_id'],'decision':'allow','category':'ok'}))`, permit))
	openPostgresFixture(t, f)
	storedPostApply(t, f)
	state, err := os.ReadFile(f.state)
	require.NoError(t, err)
	config := dbos.Config{AppName: "owned-post-repair-recovery", DatabaseURL: f.engine.postgresDSN, DatabaseSchema: f.config.Postgres.SystemSchema, ApplicationVersion: f.engine.runtimeVersion(), ExecutorID: "post-recovery:" + uuid.NewString(), SkipMigrations: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	first, err := dbos.NewContext(context.Background(), config)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, dbos.Shutdown(first, time.Second)) })
	stageReady := make(chan reference, 1)
	beforeRestart := func(ctx dbos.Context, id string) (reference, error) {
		ctx, err := f.engine.enterWorkflow(ctx, id)
		if err != nil {
			return reference{ID: id}, err
		}
		result, err := f.engine.stage(ctx, id, "post-tasks", func(stageCtx context.Context, id string) error {
			if err := f.engine.post(stageCtx, id); err != nil {
				return err
			}
			failed, err := f.engine.Get(stageCtx, id)
			if err != nil {
				return err
			}
			if failed.Phase != "post_failed" {
				return fmt.Errorf("expected real post failure, got %s", failed.Phase)
			}
			if err := os.WriteFile(permit, []byte("allow"), 0600); err != nil {
				return err
			}
			_, err = f.engine.Repair(t.Context(), id, failed.FailedStep, "operator")
			return err
		})
		if err != nil {
			return result, err
		}
		// Returning from stage means its real post_retrying checkpoint committed.
		stageReady <- result
		<-ctx.Done()
		return result, ctx.Err()
	}
	dbos.RegisterWorkflow(first, beforeRestart, dbos.WithWorkflowName("owned-post-repair-recovery"))
	require.NoError(t, dbos.Launch(first))
	_, err = dbos.RunWorkflow(first, beforeRestart, f.command.ID, dbos.WithWorkflowID(f.command.ID))
	require.NoError(t, err)
	var checkpoint reference
	select {
	case checkpoint = <-stageReady:
	case <-time.After(10 * time.Second):
		t.Fatal("real repaired post stage did not checkpoint")
	}
	require.Equal(t, "post_retrying", checkpoint.Phase)
	require.Equal(t, "task-1", checkpoint.FailedStep)
	schema := pq.QuoteIdentifier(f.config.Postgres.SystemSchema)
	var pending int
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM `+schema+`.notifications WHERE destination_uuid=$1 AND topic='repair' AND NOT consumed`, f.command.ID).Scan(&pending))
	require.Equal(t, 1, pending, "repair must commit its wake before checkpoint replay")
	require.NoError(t, dbos.Shutdown(first, time.Second))
	workflows, err := dbos.ListWorkflows(f.engine.durable, dbos.WithFilterWorkflowIDs(f.command.ID), dbos.WithFilterApplicationName(config.AppName))
	require.NoError(t, err)
	require.Len(t, workflows, 1)
	require.Equal(t, dbos.WorkflowStatusPending, workflows[0].Status)
	second, err := dbos.NewContext(context.Background(), config)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, dbos.Shutdown(second, time.Second)) })
	afterRestart := func(ctx dbos.Context, id string) (reference, error) {
		ctx, err := f.engine.enterWorkflow(ctx, id)
		if err != nil {
			return reference{ID: id}, err
		}
		return f.engine.executePostTasks(ctx, id)
	}
	dbos.RegisterWorkflow(second, afterRestart, dbos.WithWorkflowName("owned-post-repair-recovery"))
	require.NoError(t, dbos.Launch(second))
	handle, err := dbos.RetrieveWorkflow[reference](second, f.command.ID)
	require.NoError(t, err)
	result, err := handle.GetResult(dbos.WithHandleTimeout(10 * time.Second))
	require.NoError(t, err)
	require.Equal(t, "completed", result.Phase)
	completed, err := f.engine.Get(t.Context(), f.command.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", completed.Phase)
	require.True(t, completed.InfrastructureApplied)
	require.Equal(t, 1, operationCount(t, trace, "apply"))
	after, err := os.ReadFile(f.state)
	require.NoError(t, err)
	require.Equal(t, state, after)
	steps, err := dbos.GetWorkflowSteps(second, f.command.ID)
	require.NoError(t, err)
	received := 0
	for _, step := range steps {
		if step.StepName == "DBOS.recv" {
			require.NoError(t, step.Error)
			encoded, ok := step.Output.(string)
			require.True(t, ok, "DBOS listing returns serialized step output")
			var receivedStep string
			require.NoError(t, json.Unmarshal([]byte(encoded), &receivedStep))
			require.Equal(t, checkpoint.FailedStep, receivedStep)
			received++
		}
	}
	require.Equal(t, 1, received, "recovery must consume the existing matching repair wake")
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM `+schema+`.notifications WHERE destination_uuid=$1 AND topic='repair' AND NOT consumed`, f.command.ID).Scan(&pending))
	require.Zero(t, pending)
}
