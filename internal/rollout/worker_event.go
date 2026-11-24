package rollout

import (
	"context"
	"database/sql"
	"errors"
)

func (e *Engine) beginEvent(ctx context.Context, id int64) (*sql.Tx, error) {
	tx, err := e.beginCommand(ctx)
	if err != nil {
		return nil, err
	}
	if e.config.Postgres == nil {
		return tx, nil
	}
	var commandID string
	err = tx.QueryRowContext(ctx, `SELECT rollout FROM rollout_events WHERE id=$1`, id).Scan(&commandID)
	if errors.Is(err, sql.ErrNoRows) {
		err = refused("event_unavailable")
	}
	if err == nil {
		err = e.lockCommand(ctx, tx, commandID)
	}
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}
