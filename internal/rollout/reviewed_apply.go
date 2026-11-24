package rollout

import (
	"context"
	"errors"
)

func (e *Engine) reviewedApply(ctx context.Context, id string, command Command) error {
	var exists int
	if err := e.db.QueryRowContext(ctx, `SELECT count(*) FROM rollout_pipeline_results WHERE rollout=$1`, id).Scan(&exists); err != nil || exists != 0 {
		return err
	}
	var ciphertext []byte
	var expected string
	if err := e.db.QueryRowContext(ctx, `SELECT plan,digest FROM rollout_artifacts WHERE rollout=$1`, id).Scan(&ciphertext, &expected); err != nil || digest(ciphertext) != expected {
		return refused("saved_plan_unavailable")
	}
	outcome, err := e.executor.ApplySavedFrom(ctx, command.Request, command.Request, ciphertext, expected, func() error {
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
	if failure := e.applyDeadline(ctx, outcome, err, "saved_plan_unavailable"); failure != nil {
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
		return refused("saved_plan_unavailable")
	}
	return e.savePipelineOutcome(ctx, id, outcome)
}
