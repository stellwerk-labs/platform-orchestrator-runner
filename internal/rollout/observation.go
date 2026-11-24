package rollout

import (
	"context"
	"slices"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
)

func validateObservation(command Command) error {
	if (command.Version != PipelineVersion && command.Version != NativePipelineVersion) || command.Strategy != "observation" || command.Request.Mode != "observation" || len(command.Request.Bundle) == 0 || !slices.Contains(command.RequiredCapabilities, rolloutexec.ObservationCapability) {
		return refused("observation_refused")
	}
	return validateObservationAuthority(command)
}

func validateObservationAuthority(command Command) error {
	if command.Recipe != nil || command.Cost != nil || command.SourcePlan != nil || command.PlanBase != nil || len(command.Steps) != 0 || command.Approval != (Approval{}) {
		return refused("observation_refused")
	}
	if command.Request.EncryptedOutputsRecipient != "" || command.Request.MetadataKey != "" {
		return refused("observation_refused")
	}
	return nil
}

func (e *Engine) executeObservation(ctx dbos.Context, id string) (reference, error) {
	result, err := e.stage(ctx, id, "observation", e.observe)
	if err != nil || terminal(result.Phase) {
		return result, err
	}
	return e.stage(ctx, id, "observation-complete", e.completePipeline)
}

func (e *Engine) observe(ctx context.Context, id string) error {
	command, err := e.checkedCommand(ctx, id)
	if err != nil {
		return err
	}
	var count int
	if err = e.db.QueryRowContext(ctx, `SELECT count(*) FROM rollout_pipeline_results WHERE rollout=$1`, id).Scan(&count); err != nil || count != 0 {
		return err
	}
	if err = e.phase(ctx, id, "observing"); err != nil {
		return err
	}
	result, err := e.executor.Observe(ctx, command.Request)
	if err != nil || (e.config.Postgres != nil && ctx.Err() != nil) {
		return refused("observation_unavailable")
	}
	tx, err := e.beginCommand(ctx, id)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = e.insertPipelineResult(ctx, tx, id, result, nil); err != nil {
		return err
	}
	return tx.Commit()
}
