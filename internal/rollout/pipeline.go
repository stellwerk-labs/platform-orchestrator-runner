package rollout

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	api "github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
)

func (e *Engine) validateStrategy(command Command) error {
	if command.Version == Version {
		if command.Strategy != "" || len(command.Request.Bundle) > 0 || command.SourcePlan != nil || command.PlanBase != nil {
			return refused("strategy_refused")
		}
		return nil
	}
	if command.Strategy == "observation" || command.Request.Mode == "observation" {
		return validateObservation(command)
	}
	modes := map[string][]string{"apply": {"deploy", "rollback"}, "reviewed": {"deploy", "rollback"}, "apply_saved": {"deploy"}, "plan": {"plan_only"}, "destroy": {"destroy"}}
	if !slices.Contains(modes[command.Strategy], command.Request.Mode) || len(command.Request.Bundle) == 0 {
		return refused("strategy_refused")
	}
	inheritedRecipe := command.Recipe != nil && (command.Strategy == "plan" || command.Strategy == "apply_saved")
	if command.Strategy != "reviewed" && command.Approval.Mode != "" && !inheritedRecipe {
		return refused("strategy_refused")
	}
	if err := validatePlanAuthority(command); err != nil {
		return err
	}
	forbidden := map[string]string{"apply": "plan_check", "destroy": "plan_check", "plan": "post_apply"}[command.Strategy]
	planRecipe := command.Strategy == "plan" && command.Recipe != nil
	for _, step := range command.Steps {
		tool, ok := e.toolForStep(command, step.ID)
		if !ok || tool.Phase == forbidden && !planRecipe {
			return refused("steps_refused")
		}
	}
	return nil
}

func (e *Engine) executePipeline(ctx dbos.Context, id, strategy string) (reference, error) {
	result, err := e.stage(ctx, id, "preparation", e.prepare)
	if err != nil || terminal(result.Phase) {
		return result, err
	}
	if strategy == "plan" {
		return e.executePlanPipeline(ctx, id)
	}
	if strategy == "apply_saved" {
		return e.executeSavedPipeline(ctx, id)
	}

	result, err = e.stage(ctx, id, "direct-apply", e.directApply)
	if err != nil || terminal(result.Phase) {
		return result, err
	}
	return e.executePostTasks(ctx, id)
}

func (e *Engine) executePlanPipeline(ctx dbos.Context, id string) (reference, error) {
	var result reference
	var err error
	for _, stage := range []struct {
		name string
		run  func(context.Context, string) error
	}{{"plan", e.plan}, {"checks", e.check}, {"plan-complete", e.completePipeline}} {
		result, err = e.stage(ctx, id, stage.name, stage.run)
		if err != nil || terminal(result.Phase) {
			return result, err
		}
	}
	return result, nil
}

func (e *Engine) executePostTasks(ctx dbos.Context, id string) (reference, error) {
	for {
		result, err := e.stage(ctx, id, "post-tasks", e.post)
		if err != nil || result.Phase != "post_failed" && result.Phase != "post_retrying" {
			return result, err
		}
		if e.config.Postgres == nil {
			_, err = dbos.Recv[string](ctx, "repair", 7*24*time.Hour)
		} else {
			err = waitPostRepair(ctx, result.FailedStep, 7*24*time.Hour)
		}
		if err != nil {
			return result, err
		}
	}
}

func waitPostRepair(ctx dbos.Context, expected string, timeout time.Duration) error {
	for {
		step, err := dbos.Recv[string](ctx, "repair", timeout)
		if errors.Is(err, dbos.ErrTimeout) {
			continue
		}
		if err != nil {
			return err
		}
		if expected != "" && step == expected {
			return nil
		}
	}
}

func (e *Engine) completePipeline(ctx context.Context, id string) error {
	return e.finish(ctx, id, "completed", "")
}

func (e *Engine) directApply(ctx context.Context, id string) error {
	command, err := e.checkedCommand(ctx, id)
	if err != nil {
		return err
	}
	var exists int
	if err = e.db.QueryRowContext(ctx, `SELECT count(*) FROM rollout_pipeline_results WHERE rollout=$1`, id).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return nil
	}
	outcome, err := e.executor.Execute(ctx, command.Request, command.Strategy, func() error {
		if e.config.Postgres != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		status, claimErr := e.claim(ctx, id, "apply", true)
		if claimErr != nil {
			return claimErr
		}
		if status != "new" {
			return refused("apply_outcome_unknown")
		}
		return nil
	})
	if failure := e.applyDeadline(ctx, outcome, err, "execution_prepare_failed"); failure != nil {
		return failure
	}
	if err != nil {
		var refusal *Error
		if logErr := e.saveExecutionLogs(ctx, id, outcome.Logs); logErr != nil {
			if e.config.Postgres != nil && (outcome.Started || errors.As(err, &refusal)) {
				return errors.Join(refused("apply_outcome_unknown"), refused("storage_unavailable"), logErr)
			}
			return refused("storage_unavailable")
		}
		if outcome.Started || errors.As(err, &refusal) {
			return refused("apply_outcome_unknown")
		}
		return refused("execution_prepare_failed")
	}
	return e.savePipelineOutcome(ctx, id, outcome)
}

func (e *Engine) savePipelineOutcome(ctx context.Context, id string, out rolloutexec.Outcome) (resultErr error) {
	// Infrastructure has already executed. An uncommitted outcome retains the
	// permanent Apply claim and must never be interpreted as safe to execute again.
	defer func() {
		if resultErr != nil && e.config.Postgres != nil {
			resultErr = errors.Join(refused("apply_outcome_unknown"), resultErr)
		}
	}()
	encrypted, err := e.encrypt(out.Outputs)
	if err != nil {
		return refused("apply_outcome_unknown")
	}
	tx, err := e.beginCommand(ctx, id)
	if err != nil {
		return refused("apply_outcome_unknown")
	}
	defer func() { _ = tx.Rollback() }()
	if err = e.insertPipelineResult(ctx, tx, id, out.Result, out.Outputs); err != nil {
		return refused("apply_outcome_unknown")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE rollout_artifacts SET outputs=$1 WHERE rollout=$2`, encrypted, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE rollout_effects SET status='done' WHERE rollout=$1 AND step='apply'`, id); err != nil {
		return err
	}
	p, err := readProgress(ctx, tx, id)
	if err != nil {
		return err
	}
	p.Phase, p.InfrastructureApplied, p.Changes, p.ApplyChanges = "applied", out.Applied, out.Changes, out.Changes
	command, err := e.commandInTransaction(ctx, tx, id)
	if err != nil {
		return err
	}
	attachment, err := e.insertExecutionLogs(ctx, tx, command, &p, out.Logs)
	if err != nil {
		return err
	}
	if command.NativeTarget != nil {
		err = e.saveLogCapture(ctx, tx, &p, *p.LogCapture, attachment)
	} else {
		err = saveProgress(ctx, tx, &p)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (e *Engine) insertPipelineResult(ctx context.Context, tx *sql.Tx, id string, result api.DeploymentResultsUpdateBody, outputs []byte) error {
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if len(body) > 4<<20 || len(outputs) > 4<<20 {
		return refused("artifact_too_large")
	}
	encrypted, err := e.encrypt(body)
	if err != nil {
		return err
	}
	localOutputs, err := e.encrypt(outputs)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO rollout_pipeline_results(rollout,body,outputs) VALUES($1,$2,$3)`, id, encrypted, localOutputs)
	return err
}

// Completion reads authority from storage; the supplied progress selects only
// the event revision whose terminal result may be delivered.
func (e *Engine) Completion(ctx context.Context, event Progress) (string, json.RawMessage, bool, error) {
	tx, err := e.beginCommand(ctx, event.ID)
	if err != nil {
		return "", nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	command, body, ready, err := e.completionForRevision(ctx, tx, event.ID, event.Revision)
	if err != nil {
		return "", nil, false, err
	}
	if !ready {
		return "", nil, false, nil
	}
	return command.Request.DeploymentToken, body, true, nil
}
