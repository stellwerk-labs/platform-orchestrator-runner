package rollout

import (
	"context"
	"database/sql"
	"errors"
)

const commandStorageVersion = 1
const commandStorageCompactionPending = -1
const storageIdentityCheck = "stellwerk.runner.command-storage/v1"

// Migrate before DBOS launch, so unreadable commands can never resume work.
func (e *Engine) initializeCommandStorage(ctx context.Context) error {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS rollout_storage (
 id INTEGER PRIMARY KEY CHECK(id=1), version INTEGER NOT NULL, identity_check BLOB NOT NULL
);`); err != nil {
		return err
	}
	version, err := e.storedCommandVersion(ctx, tx)
	if err != nil {
		return err
	}
	legacy := version == 0
	if legacy {
		if _, err = tx.ExecContext(ctx, `ALTER TABLE rollout_commands ADD COLUMN version TEXT NOT NULL DEFAULT '';
 ALTER TABLE rollout_commands ADD COLUMN environment TEXT NOT NULL DEFAULT '';
 ALTER TABLE rollout_commands ADD COLUMN strategy TEXT NOT NULL DEFAULT '';`); err != nil {
			return err
		}
	}
	if err = e.checkStoredCommands(ctx, tx, legacy); err != nil {
		return err
	}
	if legacy {
		check, encryptErr := e.encrypt([]byte(storageIdentityCheck))
		if encryptErr != nil {
			return encryptErr
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO rollout_storage(id,version,identity_check) VALUES(1,$1,$2)`, commandStorageCompactionPending, check); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if version == commandStorageVersion {
		return nil
	}
	return e.compactCommandStorage(ctx)
}

func (e *Engine) storedCommandVersion(ctx context.Context, tx *sql.Tx) (int, error) {
	var version int
	var check []byte
	err := tx.QueryRowContext(ctx, `SELECT version,identity_check FROM rollout_storage WHERE id=1`).Scan(&version, &check)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, e.verifyLegacyIdentity(ctx, tx)
	}
	if err != nil {
		return 0, err
	}
	if version != commandStorageVersion && version != commandStorageCompactionPending {
		return 0, refused("command_storage_version_refused")
	}
	plain, err := e.decrypt(check)
	if err != nil || string(plain) != storageIdentityCheck {
		return 0, refused("storage_identity_unavailable")
	}
	return version, nil
}

func (e *Engine) verifyLegacyIdentity(ctx context.Context, tx *sql.Tx) error {
	var data []byte
	err := tx.QueryRowContext(ctx, `SELECT data FROM (
 SELECT plan AS data FROM rollout_artifacts UNION ALL SELECT review FROM rollout_artifacts
 UNION ALL SELECT outputs FROM rollout_artifacts UNION ALL SELECT content FROM rollout_cost_artifacts
 UNION ALL SELECT content FROM rollout_plan_logs UNION ALL SELECT body FROM rollout_pipeline_results
 UNION ALL SELECT outputs FROM rollout_pipeline_results UNION ALL SELECT content FROM rollout_execution_logs
) WHERE length(data)>0 LIMIT 1`).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = e.decrypt(data); err != nil {
		return refused("storage_identity_unavailable")
	}
	return nil
}

func (e *Engine) checkStoredCommands(ctx context.Context, tx *sql.Tx, legacy bool) error {
	var invalidID bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM rollout_commands WHERE id IS NULL OR id='')`).Scan(&invalidID); err != nil {
		return err
	}
	if invalidID {
		return refused("stored_command_unavailable")
	}
	var previous string
	for {
		var id, expected, version, environment, strategy string
		var data []byte
		err := tx.QueryRowContext(ctx, `SELECT id,digest,payload,version,environment,strategy FROM rollout_commands WHERE id>$1 ORDER BY id LIMIT 1`, previous).Scan(&id, &expected, &data, &version, &environment, &strategy)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		previous = id
		if legacy {
			err = e.migrateStoredCommand(ctx, tx, id, expected, data)
		} else {
			_, err = e.decryptStoredCommand(id, expected, data, version, environment, strategy)
		}
		if err != nil {
			return err
		}
	}
}

func (e *Engine) migrateStoredCommand(ctx context.Context, tx *sql.Tx, id, expected string, data []byte) error {
	command, err := e.decodeStoredCommand(id, expected, data)
	if err != nil {
		return err
	}
	encrypted, err := e.encrypt(data)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE rollout_commands SET payload=$1,version=$2,environment=$3,strategy=$4 WHERE id=$5`, encrypted, command.Version, command.EnvironmentID, command.Strategy, id)
	return err
}

func (e *Engine) decodeStoredCommand(id, expected string, plain []byte) (Command, error) {
	if len(plain) == 0 || len(plain) > 4<<20 || digest(plain) != expected {
		return Command{}, refused("stored_command_unavailable")
	}
	command, err := e.decodeCommandPayload(plain)
	if err != nil || command.ID != id {
		return Command{}, refused("stored_command_unavailable")
	}
	return command, nil
}

func (e *Engine) loadCommand(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (Command, error) {
	var data []byte
	var expected, version, environment, strategy string
	if err := query.QueryRowContext(ctx, `SELECT digest,payload,version,environment,strategy FROM rollout_commands WHERE id=$1`, id).Scan(&expected, &data, &version, &environment, &strategy); err != nil {
		return Command{}, err
	}
	return e.decryptStoredCommand(id, expected, data, version, environment, strategy)
}

func (e *Engine) decryptStoredCommand(id, expected string, data []byte, version, environment, strategy string) (Command, error) {
	plain, err := e.decrypt(data)
	if err != nil {
		return Command{}, refused("stored_command_unavailable")
	}
	command, err := e.decodeStoredCommand(id, expected, plain)
	if err != nil {
		return Command{}, err
	}
	if command.Version != version || command.EnvironmentID != environment || command.Strategy != strategy {
		return Command{}, refused("stored_command_unavailable")
	}
	return command, nil
}
