package rollout

import (
	"context"
	"database/sql"

	api "github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
)

// Table existence means the atomic upgrade seed completed. Completion checks
// this indexed, permanent fact instead of rescanning historical progress.
func (e *Engine) initializeNoEffectHistory(ctx context.Context) error {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='rollout_no_effect_blocks')`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE rollout_no_effect_blocks (
 rollout TEXT NOT NULL PRIMARY KEY REFERENCES rollout_commands(id)
);`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO rollout_no_effect_blocks(rollout)
 SELECT c.id FROM rollout_commands c WHERE
 EXISTS(SELECT 1 FROM rollout_effects f WHERE f.rollout=c.id)
 OR json_extract(c.progress,'$.phase') IN ('applying','outcome_unknown')
 OR json_extract(c.progress,'$.infrastructure_applied')=1
 OR EXISTS(SELECT 1 FROM rollout_events e WHERE e.rollout=c.id AND (
 json_extract(e.payload,'$.phase') IN ('applying','outcome_unknown')
 OR json_extract(e.payload,'$.infrastructure_applied')=1))`); err != nil {
		return err
	}
	return tx.Commit()
}

func insertEffectClaim(ctx context.Context, tx *sql.Tx, id, step string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO rollout_effects(rollout,step,status) VALUES($1,$2,'started')`, id, step); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO rollout_no_effect_blocks(rollout) VALUES($1) ON CONFLICT(rollout) DO NOTHING`, id)
	return err
}

func (e *Engine) noEffectReceipt(ctx context.Context, tx *sql.Tx, command Command) (*api.PipelineNoEffectReceipt, error) {
	if command.Strategy == "observation" {
		return nil, nil
	}
	var expected string
	var blocked bool
	if err := tx.QueryRowContext(ctx, `SELECT digest,
 EXISTS(SELECT 1 FROM rollout_effects WHERE rollout=$1)
 OR EXISTS(SELECT 1 FROM rollout_no_effect_blocks WHERE rollout=$2)
 FROM rollout_commands WHERE id=$3`, command.ID, command.ID, command.ID).Scan(&expected, &blocked); err != nil {
		return nil, err
	}
	if blocked {
		return nil, nil
	}
	return &api.PipelineNoEffectReceipt{Version: api.StellwerkNoEffectv1, Effect: api.NotStarted, CommandSha256: expected}, nil
}
