package rollout

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/stretchr/testify/require"
)

func TestRetainedCommandsAreEncryptedAndResume(t *testing.T) {
	f, trace, _ := canonicalFixture(t, "reviewed", "opentofu")
	f.command.Request.DeploymentToken = "PRIVATE_COMMAND_TOKEN_CANARY"
	signed := f.signed(t, f.command)
	_, err := f.engine.Submit(t.Context(), signed)
	require.NoError(t, err)
	waiting := waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
	var stored []byte
	var expected string
	require.NoError(t, f.engine.db.QueryRow(`SELECT digest,payload FROM rollout_commands WHERE id=?`, f.command.ID).Scan(&expected, &stored))
	require.Equal(t, digest(signed.Payload), expected)
	require.Contains(t, string(stored), "age-encryption.org/v1")
	command, err := f.engine.command(t.Context(), f.command.ID)
	require.NoError(t, err)
	require.Equal(t, f.command.Request, command.Request)
	events, err := f.engine.Events(t.Context(), 0)
	require.NoError(t, err)
	public, err := json.Marshal(events)
	require.NoError(t, err)
	secrets := []string{"PRIVATE_COMMAND_TOKEN_CANARY", "PRIVATE_PIPELINE_VALUE", base64.StdEncoding.EncodeToString(f.command.Request.Bundle)}
	for _, secret := range secrets {
		require.NotContains(t, string(stored), secret)
		require.NotContains(t, string(public), secret)
	}
	assertCommandDatabasePrivacy(t, f.config.Directory, secrets)
	closeFixture(t, f.engine)
	f.engine, err = Open(t.Context(), f.config)
	require.NoError(t, err)
	_, err = f.engine.Submit(t.Context(), signed)
	require.NoError(t, err)
	_, err = f.engine.Decide(t.Context(), f.command.ID, waiting.Subject, "approve", "operator")
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "completed")
	require.Equal(t, 1, operationCount(t, trace, "plan"))
	require.Equal(t, 1, operationCount(t, trace, "apply"))
	assertCommandDatabasePrivacy(t, f.config.Directory, secrets)
}

func assertCommandDatabasePrivacy(t *testing.T, directory string, secrets []string) {
	t.Helper()
	for _, name := range []string{"rollouts.sqlite", "rollouts.sqlite-wal", "rollouts.sqlite-shm"} {
		data, err := os.ReadFile(filepath.Join(directory, name))
		if os.IsNotExist(err) {
			continue
		}
		require.NoError(t, err)
		for _, secret := range secrets {
			require.NotContains(t, string(data), secret, name)
		}
	}
}

func TestLegacyCommandStorageMigratesAtomically(t *testing.T) {
	f := newFixture(t)
	f.command.Request.DeploymentToken = "PRIVATE_COMMAND_TOKEN_CANARY"
	signed := f.signed(t, f.command)
	_, err := f.engine.Submit(t.Context(), signed)
	require.NoError(t, err)
	waiting := waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
	closeFixture(t, f.engine)
	editClosedCommandStorage(t, f, func(db *sql.DB) {
		removeCommandStorageVersion(t, db)
		_, err := db.Exec(`UPDATE rollout_commands SET payload=? WHERE id=?`, []byte(signed.Payload), f.command.ID)
		require.NoError(t, err)
		leaveLegacyFreelistSecret(t, db)
	})
	before, err := os.ReadFile(filepath.Join(f.config.Directory, "rollouts.sqlite"))
	require.NoError(t, err)
	require.Contains(t, string(before), "PRIVATE_COMMAND_TOKEN_CANARY")
	require.Contains(t, string(before), "PRIVATE_COMMAND_FREELIST_CANARY")
	backup := filepath.Join(t.TempDir(), "before-upgrade.sqlite")
	require.NoError(t, os.WriteFile(backup, before, 0600))
	f.engine, err = Open(t.Context(), f.config)
	require.NoError(t, err)
	var stored []byte
	require.NoError(t, f.engine.db.QueryRow(`SELECT payload FROM rollout_commands WHERE id=?`, f.command.ID).Scan(&stored))
	require.NotEqual(t, []byte(signed.Payload), stored)
	var version int
	require.NoError(t, f.engine.db.QueryRow(`SELECT version FROM rollout_storage WHERE id=1`).Scan(&version))
	require.Equal(t, commandStorageVersion, version)
	assertCommandDatabasePrivacy(t, f.config.Directory, []string{"PRIVATE_COMMAND_TOKEN_CANARY", "PRIVATE_PLAN_CANARY", "PRIVATE_COMMAND_FREELIST_CANARY"})
	preserved, err := os.ReadFile(backup)
	require.NoError(t, err)
	require.Equal(t, before, preserved, "migration must not rewrite historical backups")
	_, err = f.engine.Decide(t.Context(), f.command.ID, waiting.Subject, "approve", "operator")
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "completed")
}

func TestLegacyCommandMigrationRefusesCorruptionWithoutPartialWrites(t *testing.T) {
	f := newFixture(t)
	signed := f.signed(t, f.command)
	_, err := f.engine.importCommand(t.Context(), f.command, signed.Payload)
	require.NoError(t, err)
	closeFixture(t, f.engine)
	editClosedCommandStorage(t, f, func(db *sql.DB) {
		removeCommandStorageVersion(t, db)
		_, err := db.Exec(`UPDATE rollout_commands SET payload=? WHERE id=?`, []byte(signed.Payload), f.command.ID)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO rollout_commands(id,digest,payload,progress) VALUES('z-invalid','wrong',?,'{}')`, []byte(signed.Payload))
		require.NoError(t, err)
	})
	_, err = Open(t.Context(), f.config)
	requireCode(t, err, "stored_command_unavailable")
	editClosedCommandStorage(t, f, func(db *sql.DB) {
		var stored []byte
		require.NoError(t, db.QueryRow(`SELECT payload FROM rollout_commands WHERE id=?`, f.command.ID).Scan(&stored))
		require.Equal(t, []byte(signed.Payload), stored, "failed migration rewrote a preceding row")
		var tables int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='rollout_storage'`).Scan(&tables))
		require.Zero(t, tables, "failed migration committed its version table")
	})
	_, err = os.Stat(f.state)
	require.True(t, os.IsNotExist(err), "failed migration started external execution")
}

func TestCommandStorageRefusesWrongIdentityOrDamagedRowsBeforeRecovery(t *testing.T) {
	for _, problem := range []string{"missing-key", "empty-key", "wrong-key", "legacy-wrong-key", "ciphertext", "digest", "identity", "empty-id", "version", "scope-version", "scope-environment", "scope-strategy"} {
		t.Run(problem, func(t *testing.T) {
			f := newFixture(t)
			signed := f.signed(t, f.command)
			_, err := f.engine.Submit(t.Context(), signed)
			require.NoError(t, err)
			waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
			closeFixture(t, f.engine)
			damageCommandStorage(t, f, problem, signed.Payload)
			_, err = Open(t.Context(), f.config)
			require.Error(t, err)
			_, err = os.Stat(f.state)
			require.True(t, os.IsNotExist(err), "damaged storage resumed Apply")
			if problem == "missing-key" {
				_, err = os.Stat(filepath.Join(f.config.Directory, "identity.agekey"))
				require.True(t, os.IsNotExist(err), "missing identity was silently regenerated")
			}
		})
	}
}

func damageCommandStorage(t *testing.T, f *fixture, problem string, plain []byte) {
	t.Helper()
	key := filepath.Join(f.config.Directory, "identity.agekey")
	switch problem {
	case "missing-key":
		require.NoError(t, os.Remove(key))
	case "empty-key":
		require.NoError(t, os.WriteFile(key, nil, 0600))
	case "wrong-key", "legacy-wrong-key":
		identity, err := age.GenerateX25519Identity()
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(key, []byte(identity.String()), 0600))
	default:
		editClosedCommandStorage(t, f, func(db *sql.DB) {
			queries := map[string]string{
				"ciphertext":        `UPDATE rollout_commands SET payload=x'00'`,
				"digest":            `UPDATE rollout_commands SET digest='changed'`,
				"identity":          `UPDATE rollout_commands SET id='changed'`,
				"empty-id":          `UPDATE rollout_commands SET id=''`,
				"version":           `UPDATE rollout_storage SET version=999`,
				"scope-version":     `UPDATE rollout_commands SET version='changed'`,
				"scope-environment": `UPDATE rollout_commands SET environment='changed'`,
				"scope-strategy":    `UPDATE rollout_commands SET strategy='changed'`,
			}
			_, err := db.Exec(queries[problem])
			require.NoError(t, err)
		})
	}
	if problem == "legacy-wrong-key" {
		editClosedCommandStorage(t, f, func(db *sql.DB) {
			removeCommandStorageVersion(t, db)
			_, err := db.Exec(`UPDATE rollout_commands SET payload=?`, plain)
			require.NoError(t, err)
		})
	}
}

func TestStoredCommandReadersRefuseChangedScopeMetadata(t *testing.T) {
	f := newFixture(t)
	signed := f.signed(t, f.command)
	_, err := f.engine.importCommand(t.Context(), f.command, signed.Payload)
	require.NoError(t, err)
	for column, original := range map[string]string{"version": f.command.Version, "environment": f.command.EnvironmentID, "strategy": f.command.Strategy} {
		t.Run(column, func(t *testing.T) {
			_, err := f.engine.db.Exec(`UPDATE rollout_commands SET `+column+`='changed' WHERE id=?`, f.command.ID)
			require.NoError(t, err)
			_, err = f.engine.command(t.Context(), f.command.ID)
			requireCode(t, err, "stored_command_unavailable")
			_, err = f.engine.db.Exec(`UPDATE rollout_commands SET `+column+`=? WHERE id=?`, original, f.command.ID)
			require.NoError(t, err)
		})
	}
}

func TestCommandStorageResumesPendingCompactionBeforeRecovery(t *testing.T) {
	f := newFixture(t)
	signed := f.signed(t, f.command)
	_, err := f.engine.importCommand(t.Context(), f.command, signed.Payload)
	require.NoError(t, err)
	closeFixture(t, f.engine)
	var before []byte
	editClosedCommandStorage(t, f, func(db *sql.DB) {
		_, err := db.Exec(`UPDATE rollout_storage SET version=?`, commandStorageCompactionPending)
		require.NoError(t, err)
		require.NoError(t, db.QueryRow(`SELECT payload FROM rollout_commands WHERE id=?`, f.command.ID).Scan(&before))
		leaveLegacyFreelistSecret(t, db)
	})
	f.engine, err = Open(t.Context(), f.config)
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
	var after []byte
	require.NoError(t, f.engine.db.QueryRow(`SELECT payload FROM rollout_commands WHERE id=?`, f.command.ID).Scan(&after))
	require.Equal(t, before, after, "resuming compaction encrypted an already encrypted command again")
	assertCommandDatabasePrivacy(t, f.config.Directory, []string{"PRIVATE_PLAN_CANARY", "PRIVATE_COMMAND_FREELIST_CANARY"})
}

func TestCommandStorageBlocksRecoveryWhenCompactionCannotCheckpoint(t *testing.T) {
	f := newFixture(t)
	signed := f.signed(t, f.command)
	_, err := f.engine.importCommand(t.Context(), f.command, signed.Payload)
	require.NoError(t, err)
	closeFixture(t, f.engine)
	editClosedCommandStorage(t, f, func(db *sql.DB) {
		_, err := db.Exec(`UPDATE rollout_storage SET version=?`, commandStorageCompactionPending)
		require.NoError(t, err)
		leaveLegacyFreelistSecret(t, db)
	})
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.config.Directory, "rollouts.sqlite"))
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	reader, err := db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = reader.Rollback() }()
	var count int
	require.NoError(t, reader.QueryRow(`SELECT count(*) FROM rollout_commands`).Scan(&count))
	_, err = Open(t.Context(), f.config)
	requireCode(t, err, "command_storage_compaction_required")
	var version int
	var progress Progress
	var body []byte
	require.NoError(t, db.QueryRow(`SELECT version FROM rollout_storage WHERE id=1`).Scan(&version))
	require.Equal(t, commandStorageCompactionPending, version)
	require.NoError(t, db.QueryRow(`SELECT progress FROM rollout_commands WHERE id=?`, f.command.ID).Scan(&body))
	require.NoError(t, json.Unmarshal(body, &progress))
	require.Equal(t, "accepted", progress.Phase, "compaction failure launched workflow recovery")
	require.NoError(t, reader.Rollback())
	f.engine, err = Open(t.Context(), f.config)
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
}

func leaveLegacyFreelistSecret(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`PRAGMA secure_delete=OFF; CREATE TABLE old_private_payload(data BLOB)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO old_private_payload(data) VALUES(?)`, []byte(strings.Repeat("PRIVATE_COMMAND_FREELIST_CANARY", 2048)))
	require.NoError(t, err)
	_, err = db.Exec(`DROP TABLE old_private_payload`)
	require.NoError(t, err)
}

func removeCommandStorageVersion(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`DROP TABLE rollout_storage;
 ALTER TABLE rollout_commands DROP COLUMN version;
 ALTER TABLE rollout_commands DROP COLUMN environment;
 ALTER TABLE rollout_commands DROP COLUMN strategy;`)
	require.NoError(t, err)
}

func editClosedCommandStorage(t *testing.T, f *fixture, edit func(*sql.DB)) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.config.Directory, "rollouts.sqlite"))
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	edit(db)
}

func TestMissingIdentityRefusesOrphanedSQLiteFiles(t *testing.T) {
	for _, name := range []string{"rollouts.sqlite", "rollouts.sqlite-wal", "rollouts.sqlite-shm"} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(directory, name), nil, 0600))
			_, err := siteIdentity(directory)
			requireCode(t, err, "storage_identity_unavailable")
			_, err = os.Stat(filepath.Join(directory, "identity.agekey"))
			require.True(t, os.IsNotExist(err))
		})
	}
}
