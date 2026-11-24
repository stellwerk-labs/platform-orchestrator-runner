package rollout

import "context"

// The pending version survives crashes between encryption and removal of old
// SQLite pages. Recovery cannot launch until both the main DB and WAL are clean.
func (e *Engine) compactCommandStorage(ctx context.Context) error {
	var secureDelete bool
	if err := e.db.QueryRowContext(ctx, `PRAGMA secure_delete`).Scan(&secureDelete); err != nil || !secureDelete {
		return refused("command_storage_compaction_required")
	}
	if _, err := e.db.ExecContext(ctx, `VACUUM`); err != nil {
		return refused("command_storage_compaction_required")
	}
	var busy, pages, checkpointed int
	if err := e.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &pages, &checkpointed); err != nil || busy != 0 || pages != 0 || checkpointed != 0 {
		return refused("command_storage_compaction_required")
	}
	result, err := e.db.ExecContext(ctx, `UPDATE rollout_storage SET version=$1 WHERE id=1 AND version=$2`, commandStorageVersion, commandStorageCompactionPending)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return refused("command_storage_compaction_required")
	}
	return nil
}
