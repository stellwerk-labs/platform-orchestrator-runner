package rollout

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

type workflowOwnerKey struct{}

func (e *Engine) activateSlot(ctx context.Context) error {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return errStorageUnavailable
	}
	defer func() { _ = tx.Rollback() }()
	var catalogue string
	if err = tx.QueryRowContext(ctx, `SELECT catalogue FROM rollout_storage WHERE id=1 FOR SHARE`).Scan(&catalogue); err != nil {
		return errStorageUnavailable
	}
	if catalogue != e.executor.CatalogDigest() {
		return refused("catalog_refused")
	}
	var generation int64
	err = tx.QueryRowContext(ctx, `INSERT INTO rollout_workers(worker,generation) VALUES($1,1)
 ON CONFLICT(worker) DO UPDATE SET generation=rollout_workers.generation+1 RETURNING generation`, e.config.Postgres.WorkerID).Scan(&generation)
	if err != nil {
		return errStorageUnavailable
	}
	if err = tx.Commit(); err != nil {
		return errStorageUnavailable
	}
	e.generation.Store(generation)
	return nil
}

// beginCommand serializes authority, while an ordinary *sql.Tx remains usable
// by public DBOS enqueue/send. Human actions need a live slot, not the workflow's slot.
func (e *Engine) beginCommand(ctx context.Context, ids ...string) (*sql.Tx, error) {
	if e.stopped.Load() {
		return nil, errStorageUnavailable
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		e.storageFailed(ctx)
		return nil, err
	}
	if e.config.Postgres == nil {
		return tx, nil
	}
	if err = e.checkSlot(ctx, tx); err == nil {
		names := slices.Clone(ids)
		slices.Sort(names)
		for _, id := range slices.Compact(names) {
			if err = e.lockCommand(ctx, tx, id); err != nil {
				break
			}
		}
	}
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func (e *Engine) checkSlot(ctx context.Context, tx *sql.Tx) error {
	var generation int64
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM rollout_workers WHERE worker=$1 FOR SHARE`, e.config.Postgres.WorkerID).Scan(&generation); err != nil {
		e.storageFailed(ctx)
		return errStorageUnavailable
	}
	if generation != e.generation.Load() {
		e.stopWorker(errGenerationSuperseded)
		return errGenerationSuperseded
	}
	return nil
}

func (e *Engine) lockCommand(ctx context.Context, tx *sql.Tx, id string) error {
	var owner string
	var generation int64
	err := tx.QueryRowContext(ctx, `SELECT owner_worker,owner_generation FROM rollout_commands WHERE id=$1 FOR UPDATE`, id).Scan(&owner, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if ctx.Value(workflowOwnerKey{}) != nil && (owner != e.config.Postgres.WorkerID || generation != e.generation.Load()) {
		e.stopWorker(errGenerationSuperseded)
		return errGenerationSuperseded
	}
	return nil
}

func (e *Engine) enterWorkflow(ctx dbos.Context, id string) (dbos.Context, error) {
	if e.config.Postgres == nil {
		return ctx, nil
	}
	tx, err := e.beginCommand(ctx, id)
	if err != nil {
		e.storageFailed(ctx)
		return ctx, err
	}
	defer func() { _ = tx.Rollback() }()
	var owner string
	var generation int64
	if err = tx.QueryRowContext(ctx, `SELECT owner_worker,owner_generation FROM rollout_commands WHERE id=$1`, id).Scan(&owner, &generation); err != nil {
		e.storageFailed(ctx)
		return ctx, err
	}
	if owner != "" && (owner != e.config.Postgres.WorkerID || generation != e.generation.Load()) {
		var current int64
		if err = tx.QueryRowContext(ctx, `SELECT generation FROM rollout_workers WHERE worker=$1 FOR SHARE`, owner).Scan(&current); err != nil {
			e.storageFailed(ctx)
			return ctx, err
		}
		if current <= generation {
			e.stopWorker(errGenerationSuperseded)
			return ctx, errGenerationSuperseded
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE rollout_commands SET owner_worker=$1,owner_generation=$2 WHERE id=$3`, e.config.Postgres.WorkerID, e.generation.Load(), id); err != nil {
		e.storageFailed(ctx)
		return ctx, err
	}
	if err = tx.Commit(); err != nil {
		e.storageFailed(ctx)
		return ctx, err
	}
	return dbos.WithValue(ctx, workflowOwnerKey{}, true), nil
}

func (e *Engine) lockImport(ctx context.Context, tx *sql.Tx, command Command) error {
	if e.config.Postgres == nil {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, e.config.Postgres.ApplicationSchema+":environment:"+command.EnvironmentID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, e.config.Postgres.ApplicationSchema+":import:"+command.ID); err != nil {
		return err
	}
	ids := []string{command.ID}
	if command.SourcePlan != nil {
		ids = append(ids, command.SourcePlan.SourcePlanID)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if err := e.lockCommand(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) insertClaim(ctx context.Context, tx *sql.Tx, id, step string) error {
	if e.config.Postgres == nil {
		return insertEffectClaim(ctx, tx, id, step)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO rollout_effects(rollout,step,status,owner_worker,owner_generation) VALUES($1,$2,'started',$3,$4)`, id, step, e.config.Postgres.WorkerID, e.generation.Load()); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO rollout_no_effect_blocks(rollout) VALUES($1) ON CONFLICT(rollout) DO NOTHING`, id)
	return err
}
