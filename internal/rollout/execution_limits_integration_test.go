//go:build integration

package rollout

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/lib/pq"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
	"github.com/stretchr/testify/require"
)

const (
	nativeBudgetIaCSeconds       int64 = 5
	nativeBudgetLateDelaySeconds int64 = nativeBudgetIaCSeconds + 2
)

func installBudgetDeadlineWrapper(t *testing.T, operation, late string) {
	t.Helper()
	path, err := exec.LookPath("tofu")
	require.NoError(t, err)
	program, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(program), "\nexec "), "use the existing tracing wrapper")
	quotedLate := "'" + strings.ReplaceAll(late, "'", "'\\''") + "'"
	block := fmt.Sprintf(`
case "$2" in %s)
 printf '%%s\n' 'PRIVATE_NATIVE_BUDGET_LOG_CANARY'
 trap '' INT
 (sleep %d; touch %s) &
 wait
 ;;
esac
exec `, operation, nativeBudgetLateDelaySeconds, quotedLate)
	program = []byte(strings.Replace(string(program), "\nexec ", block, 1))
	require.NoError(t, os.WriteFile(path, program, 0700))
}

func TestPostgresNativeBudgetIaCDeadlineRetainsPrivateLogsAndFences(t *testing.T) {
	for _, test := range []struct {
		name, strategy, operation, phase string
	}{
		{"plan", "plan", "plan", "failed"},
		{"prepare-before-claim", "apply", "init", "failed"},
		{"direct-apply", "apply", "apply", "outcome_unknown"},
		{"reviewed-apply", "reviewed", "apply", "outcome_unknown"},
		{"saved-apply", "apply_saved", "apply", "outcome_unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			strategy := test.strategy
			if strategy == "apply_saved" {
				strategy = "plan"
			}
			f, trace, _ := canonicalFixture(t, strategy, "opentofu")
			late := filepath.Join(t.TempDir(), "unexpected-late-child")
			installBudgetDeadlineWrapper(t, test.operation, late)
			openPostgresFixture(t, f)
			managed, err := age.GenerateX25519Identity()
			require.NoError(t, err)
			f.command.Request.EncryptedLogsRecipient = managed.Recipient().String()
			f.command.ExecutionLimits.ApplySeconds = nativeBudgetIaCSeconds
			if test.strategy == "plan" {
				f.command.ExecutionLimits.PlanSeconds = nativeBudgetIaCSeconds
			}
			if strategy == "plan" {
				f.command.PlanBase = &PlanBase{BaseDeploymentID: emptyPlanBase}
			}
			if strategy == "reviewed" {
				f.command.Approval = Approval{Mode: "policy", MaxAdd: 1}
			}
			if test.strategy == "apply_saved" {
				_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
				require.NoError(t, err)
				waitPhase(t, f.engine, f.command.ID, "completed")
				f.command = savedCommand(t, f, f.command, "budget-saved-child")
			}
			signed := f.signed(t, f.command)
			_, err = f.engine.Submit(t.Context(), signed)
			require.NoError(t, err)
			failed := waitPhase(t, f.engine, f.command.ID, test.phase)
			require.Equal(t, 1, operationCount(t, trace, test.operation), "the selected IaC operation must enter the deadline wrapper")
			require.True(t, f.engine.Ready(), "a subprocess deadline is not a storage failure")
			require.Eventually(t, func() bool {
				rows, queryErr := dbos.ListWorkflows(f.engine.durable, dbos.WithFilterWorkflowIDs(failed.ID))
				return queryErr == nil && len(rows) == 1 && rows[0].Status == dbos.WorkflowStatusSuccess
			}, 5*time.Second, 20*time.Millisecond, "private timeout values must be consumed before the framework checkpoints")
			var logs PlanLogs
			var encrypted []byte
			if test.strategy == "plan" {
				logs, err = f.engine.GetPlanLogs(t.Context(), failed.ID, failed.Subject)
				require.NoError(t, err)
				require.NoError(t, f.engine.db.QueryRow(`SELECT content FROM rollout_plan_logs WHERE rollout=$1`, failed.ID).Scan(&encrypted))
			} else {
				logs, err = f.engine.GetExecutionLogs(t.Context(), failed.ID, "")
				require.NoError(t, err)
				require.NoError(t, f.engine.db.QueryRow(`SELECT content FROM rollout_execution_logs WHERE rollout=$1`, failed.ID).Scan(&encrypted))
			}
			require.Contains(t, string(logs.Content), "PRIVATE_NATIVE_BUDGET_LOG_CANARY")
			require.NotContains(t, string(encrypted), "PRIVATE_NATIVE_BUDGET_LOG_CANARY")
			require.NotContains(t, failed.ErrorCode, "PRIVATE_NATIVE_BUDGET_LOG_CANARY")
			require.NotNil(t, failed.LogCapture)
			require.Equal(t, nativelogs.PendingTransfer, failed.LogCapture.Status)
			require.Equal(t, digest(signed.Payload), failed.LogCapture.Authority.CommandDigest)
			require.Equal(t, *f.command.NativeTarget, failed.LogCapture.Authority.Target)
			frozen := nativeCaptureDelivery(t, f, failed.ID, failed.LogCapture.Authority.CaptureRevision)
			require.NotNil(t, frozen.Log)
			require.Equal(t, *failed.LogCapture, frozen.Log.Descriptor)
			exported, err := nativelogs.Decode(frozen.Log.Ciphertext, managed, frozen.Log.Descriptor.Authority)
			require.NoError(t, err)
			require.Equal(t, logs.Content, exported.Logs)
			require.NotContains(t, string(frozen.Progress), "PRIVATE_NATIVE_BUDGET_LOG_CANARY")
			require.NotContains(t, string(frozen.Result), "PRIVATE_NATIVE_BUDGET_LOG_CANARY")
			var checkpoint string
			require.NoError(t, f.engine.db.QueryRow(`SELECT coalesce(string_agg(output::text,''),'')||coalesce(string_agg(error::text,''),'') FROM `+pq.QuoteIdentifier(f.config.Postgres.SystemSchema)+`.operation_outputs WHERE workflow_uuid=$1`, failed.ID).Scan(&checkpoint))
			require.NotContains(t, checkpoint, "PRIVATE_NATIVE_BUDGET_LOG_CANARY")
			var checkpointErrors int
			require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM `+pq.QuoteIdentifier(f.config.Postgres.SystemSchema)+`.operation_outputs WHERE workflow_uuid=$1 AND error IS NOT NULL`, failed.ID).Scan(&checkpointErrors))
			require.Zero(t, checkpointErrors)
			var claims, owners int
			require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_effects WHERE rollout=$1 AND step='apply' AND status='started'`, failed.ID).Scan(&claims))
			require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_environments WHERE rollout=$1`, failed.ID).Scan(&owners))
			expectedApply := 0
			if test.phase == "outcome_unknown" {
				expectedApply = 1
				require.Equal(t, 1, claims)
				require.Equal(t, 1, owners)
			} else {
				require.Zero(t, claims)
				require.Zero(t, owners)
			}
			require.Equal(t, expectedApply, operationCount(t, trace, "apply"))
			reopenPostgres(t, f)
			require.Equal(t, frozen, nativeCaptureDelivery(t, f, failed.ID, failed.LogCapture.Authority.CaptureRevision))
			_, err = f.engine.Submit(t.Context(), signed)
			require.NoError(t, err)
			waitPhase(t, f.engine, failed.ID, test.phase)
			require.Equal(t, 1, operationCount(t, trace, test.operation), "replay must not re-enter the selected IaC operation")
			require.Equal(t, expectedApply, operationCount(t, trace, "apply"))
			time.Sleep(time.Duration(nativeBudgetLateDelaySeconds)*time.Second + 500*time.Millisecond)
			_, err = os.Stat(late)
			require.ErrorIs(t, err, os.ErrNotExist, "timed-out IaC descendant performed a late effect")
		})
	}
}

func TestPostgresNativeBudgetPostTaskTimeoutClassification(t *testing.T) {
	for _, retry := range []string{"receipt", "read_only", "unsafe"} {
		t.Run(retry, func(t *testing.T) {
			f, trace, _ := canonicalFixture(t, "apply", "opentofu")
			receipt := filepath.Join(t.TempDir(), "effect-receipt")
			late := filepath.Join(t.TempDir(), "unexpected-late-child")
			addTask(t, f, "post_apply", retry, fmt.Sprintf(`import json,sys,pathlib,time,subprocess
r=json.load(sys.stdin)
p=pathlib.Path(%q)
if not p.exists():
 p.write_text(r['effect_id'])
 child="import time,pathlib;time.sleep(3);pathlib.Path("+repr(%q)+").write_text('late')"
 subprocess.Popen([sys.executable,'-c',child],stdin=subprocess.DEVNULL)
 time.sleep(60)
assert p.read_text()==r['effect_id']
print(json.dumps({'version':1,'task_id':r['task_id'],'effect_id':r['effect_id'],'decision':'allow','category':'ok'}))
`, receipt, late))
			openPostgresFixture(t, f)
			f.command.ExecutionLimits.TaskSeconds = 1
			signed := f.signed(t, f.command)
			_, err := f.engine.Submit(t.Context(), signed)
			require.NoError(t, err)
			phase := "post_failed"
			if retry == "unsafe" {
				phase = "outcome_unknown"
			}
			failed := waitPhase(t, f.engine, f.command.ID, phase)
			require.True(t, failed.InfrastructureApplied)
			require.True(t, f.engine.Ready(), "an operation deadline must not become a false storage outage")
			require.Equal(t, 1, operationCount(t, trace, "apply"))
			var owners int
			require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_environments WHERE rollout=$1`, failed.ID).Scan(&owners))
			require.Equal(t, 1, owners)
			before, err := os.ReadFile(receipt)
			require.NoError(t, err)
			if retry == "unsafe" {
				_, err = f.engine.Repair(t.Context(), failed.ID, "task-1", "operator")
				requireCode(t, err, "repair_refused")
			} else {
				require.Equal(t, "task-1", failed.FailedStep)
				require.Equal(t, "task_timed_out", failed.ErrorCode)
				reopenPostgres(t, f)
				f.command.ExecutionLimits.TaskSeconds = 8
				_, err = f.engine.Submit(t.Context(), signed)
				require.NoError(t, err)
				retained, err := f.engine.command(t.Context(), failed.ID)
				require.NoError(t, err)
				require.Equal(t, int64(1), retained.ExecutionLimits.TaskSeconds, "replay cannot adopt a changed current command value")
				_, err = f.engine.Repair(t.Context(), failed.ID, failed.FailedStep, "operator")
				require.NoError(t, err)
				completed := waitPhase(t, f.engine, failed.ID, "completed")
				require.True(t, completed.InfrastructureApplied)
				after, err := os.ReadFile(receipt)
				require.NoError(t, err)
				require.Equal(t, before, after, "retry must retain its original effect identity")
				require.Equal(t, 1, operationCount(t, trace, "apply"))
			}
			time.Sleep(2500 * time.Millisecond)
			_, err = os.Stat(late)
			require.ErrorIs(t, err, os.ErrNotExist, "a cancelled descendant performed a later effect")
		})
	}
}

func TestPostgresNativeBudgetContextRetainsEarlierParentDeadline(t *testing.T) {
	f, _, _ := postgresFixture(t, "apply")
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "completed")
	parentDeadline := time.Now().Add(10 * time.Second)
	parent, cancel := context.WithDeadline(t.Context(), parentDeadline)
	defer cancel()
	bounded, cancelStage, err := f.engine.stageContext(parent, f.command.ID, "apply")
	require.NoError(t, err)
	defer cancelStage()
	actual, ok := bounded.Deadline()
	require.True(t, ok)
	require.Equal(t, parentDeadline, actual)
}

func TestPostgresNativeBudgetObservationTimeoutHasNoRetainedResultOrEffect(t *testing.T) {
	f, trace := observationFixture(t, "opentofu")
	late := filepath.Join(t.TempDir(), "unexpected-observer-child")
	installBudgetDeadlineWrapper(t, "plan", late)
	openPostgresFixture(t, f)
	f.command.ExecutionLimits.PlanSeconds = nativeBudgetIaCSeconds
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	failed := waitPhase(t, f.engine, f.command.ID, "failed")
	require.Equal(t, 1, operationCount(t, trace, "plan"), "observation must enter the plan deadline wrapper")
	require.Equal(t, "observation_unavailable", failed.ErrorCode)
	require.False(t, failed.InfrastructureApplied)
	require.True(t, f.engine.Ready())
	var results, effects int
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_pipeline_results WHERE rollout=$1`, failed.ID).Scan(&results))
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_effects WHERE rollout=$1`, failed.ID).Scan(&effects))
	require.Zero(t, results)
	require.Zero(t, effects)
	require.Zero(t, operationCount(t, trace, "apply"))
	time.Sleep(time.Duration(nativeBudgetLateDelaySeconds)*time.Second + 500*time.Millisecond)
	_, err = os.Stat(late)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestPostgresNativeBudgetExpiresDuringCostWithoutSavingLatePlan(t *testing.T) {
	f, trace, _ := canonicalFixture(t, "apply", "opentofu")
	invoked := filepath.Join(t.TempDir(), "pricing-invoked")
	pricing := filepath.Join(t.TempDir(), "owned-pricing-process")
	quoted := "'" + strings.ReplaceAll(invoked, "'", "'\\''") + "'"
	// Consume the budget in the real installed-process path; this does not
	// assert estimator correctness or substitute a Go executor implementation.
	require.NoError(t, os.WriteFile(pricing, []byte("#!/bin/sh\nprintf invoked > "+quoted+"\nexec /bin/sleep 60\n"), 0700))
	f.config.Executor.Cost = &runner.CostConfiguration{APIKey: "OWNED_BUDGET_FIXTURE_KEY", Binary: pricing, Endpoint: "http://127.0.0.1:1", PricingAccess: "local"}
	f.config.Executor.Capabilities = append(f.config.Executor.Capabilities, rolloutexec.CostCapability, rolloutexec.LocalPricingCapability)
	f.command.RequiredCapabilities = append(f.command.RequiredCapabilities, rolloutexec.CostCapability, rolloutexec.LocalPricingCapability)
	openPostgresFixture(t, f)
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "completed")
	baseline := f.command.ID
	f.command.ID, f.command.Request.ID, f.command.Strategy = "budget-cost-plan", "budget-cost-plan", "plan"
	f.command.Request.Mode = "plan_only"
	f.command.Cost = &CostPolicy{Enabled: true, PolicyRevision: 1}
	f.command.PlanBase = &PlanBase{BaseDeploymentID: baseline}
	f.command.ExecutionLimits.PlanSeconds = 3
	_, err = f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	failed := waitPhase(t, f.engine, f.command.ID, "failed")
	require.FileExists(t, invoked, "the deadline must occur during pricing, after actual Plan")
	require.Equal(t, "plan_failed", failed.ErrorCode)
	require.True(t, f.engine.Ready())
	var plans, costs int
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_artifacts WHERE rollout=$1`, failed.ID).Scan(&plans))
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_cost_artifacts WHERE rollout=$1`, failed.ID).Scan(&costs))
	require.Zero(t, plans)
	require.Zero(t, costs)
	logs, err := f.engine.GetPlanLogs(t.Context(), failed.ID, failed.Subject)
	require.NoError(t, err)
	require.NotEmpty(t, logs.Content)
	require.Equal(t, 1, operationCount(t, trace, "plan"))
	require.Equal(t, 1, operationCount(t, trace, "apply"), "only the baseline may Apply")
}

func TestPostgresNativeBudgetFailedBookkeepingKeepsPendingRecovery(t *testing.T) {
	f, trace, _ := canonicalFixture(t, "apply", "opentofu")
	receipt := filepath.Join(t.TempDir(), "receipt")
	addTask(t, f, "post_apply", "receipt", fmt.Sprintf(`import json,sys,pathlib,time
r=json.load(sys.stdin)
p=pathlib.Path(%q)
if not p.exists():
 p.write_text(r['effect_id'])
 time.sleep(60)
assert p.read_text()==r['effect_id']
print(json.dumps({'version':1,'task_id':r['task_id'],'effect_id':r['effect_id'],'decision':'allow','category':'ok'}))`, receipt))
	admin := openPostgresFixture(t, f)
	f.command.ExecutionLimits.TaskSeconds = 1
	schema := pq.QuoteIdentifier(f.config.Postgres.ApplicationSchema)
	_, err := admin.Exec(`CREATE FUNCTION ` + schema + `.refuse_deadline_bookkeeping() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF convert_from(NEW.progress,'UTF8')::jsonb::text LIKE '%task_timed_out%' THEN RAISE EXCEPTION 'PRIVATE_BOOKKEEPING_CANARY'; END IF; RETURN NEW; END; $$;
CREATE TRIGGER refuse_deadline_bookkeeping BEFORE UPDATE ON ` + schema + `.rollout_commands FOR EACH ROW EXECUTE FUNCTION ` + schema + `.refuse_deadline_bookkeeping()`)
	require.NoError(t, err)
	_, err = f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	select {
	case reason := <-f.engine.Fatal():
		require.ErrorIs(t, reason, errStorageUnavailable)
	case <-time.After(10 * time.Second):
		t.Fatal("failed deadline bookkeeping did not revoke worker authority")
	}
	require.False(t, f.engine.Ready())
	closeFixture(t, f.engine)
	_, err = admin.Exec(`DROP TRIGGER refuse_deadline_bookkeeping ON ` + schema + `.rollout_commands`)
	require.NoError(t, err)
	var reopened error
	f.engine, reopened = Open(t.Context(), f.config)
	require.NoError(t, reopened)
	failed := waitPhase(t, f.engine, f.command.ID, "post_failed")
	require.True(t, failed.InfrastructureApplied)
	require.NotContains(t, failed.ErrorCode, "PRIVATE_BOOKKEEPING_CANARY")
	_, err = f.engine.Repair(t.Context(), failed.ID, failed.FailedStep, "operator")
	require.NoError(t, err)
	waitPhase(t, f.engine, failed.ID, "completed")
	require.Equal(t, 1, operationCount(t, trace, "apply"))
}
