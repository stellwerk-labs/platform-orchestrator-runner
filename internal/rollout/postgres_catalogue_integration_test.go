//go:build integration

package rollout

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestPostgresNativeCatalogueUsesOwnedKeyedProfileAcrossWorkers(t *testing.T) {
	f, _, admin := postgresFixture(t, "plan")
	legacy := f.config.Executor.CatalogDigest()
	require.NotEqual(t, legacy, f.engine.ToolsDigest(), "the original unprepared caller must not advertise Native runtime authority")
	require.Equal(t, f.engine.ToolsDigest(), f.command.ToolsDigest)
	var stored string
	table := pq.QuoteIdentifier(f.config.Postgres.ApplicationSchema)
	require.NoError(t, admin.QueryRow(`SELECT catalogue FROM `+table+`.rollout_storage`).Scan(&stored))
	require.Equal(t, f.engine.ToolsDigest(), stored)
	cfg := f.config
	pg := *cfg.Postgres
	cfg.Postgres = &pg
	pg.WorkerID = "worker-same-key"
	cfg.Directory = filepath.Join(t.TempDir(), "independent-worker")
	other, err := Open(t.Context(), cfg)
	require.NoError(t, err)
	defer closeFixture(t, other)
	require.Equal(t, stored, other.ToolsDigest())
	var commands, effects int
	require.NoError(t, admin.QueryRow(`SELECT count(*) FROM `+table+`.rollout_commands`).Scan(&commands))
	require.NoError(t, admin.QueryRow(`SELECT count(*) FROM `+table+`.rollout_effects`).Scan(&effects))
	require.Zero(t, commands)
	require.Zero(t, effects)
}

func TestPostgresNativeCatalogueRefusesRawFormatWithoutRewritingHistory(t *testing.T) {
	f, _, admin := postgresFixture(t, "plan")
	f.command.ToolsDigest = f.config.Executor.CatalogDigest()
	plain, err := json.Marshal(f.command)
	require.NoError(t, err)
	ciphertext, err := f.engine.encrypt(plain)
	require.NoError(t, err)
	progress, err := json.Marshal(Progress{ID: f.command.ID, EnvironmentID: f.command.EnvironmentID, Phase: "awaiting_approval", Revision: 1})
	require.NoError(t, err)
	_, err = f.engine.db.Exec(`INSERT INTO rollout_commands(id,digest,payload,progress,version,environment,strategy) VALUES($1,$2,$3,$4,$5,$6,$7)`, f.command.ID, digest(plain), ciphertext, progress, f.command.Version, f.command.EnvironmentID, f.command.Strategy)
	require.NoError(t, err)
	closeFixture(t, f.engine)
	table := pq.QuoteIdentifier(f.config.Postgres.ApplicationSchema)
	for _, retained := range []struct {
		version int
		code    string
	}{{5, "postgres_storage_format_refused"}, {postgresStorageVersion, "catalog_refused"}} {
		_, err = admin.Exec(`UPDATE `+table+`.rollout_storage SET version=$1,catalogue=$2`, retained.version, f.command.ToolsDigest)
		require.NoError(t, err)
		var before string
		require.NoError(t, admin.QueryRow(`SELECT row_to_json(s)::text FROM `+table+`.rollout_storage s`).Scan(&before))
		engine, err := Open(t.Context(), f.config)
		require.Nil(t, engine)
		requireCode(t, err, retained.code)
		requireCode(t, InitializePostgres(t.Context(), f.config), retained.code)
		var after string
		var ciphertextAfter []byte
		require.NoError(t, admin.QueryRow(`SELECT row_to_json(s)::text FROM `+table+`.rollout_storage s`).Scan(&after))
		require.Equal(t, before, after)
		require.NoError(t, admin.QueryRow(`SELECT payload FROM `+table+`.rollout_commands WHERE id=$1`, f.command.ID).Scan(&ciphertextAfter))
		require.Equal(t, ciphertext, ciphertextAfter)
		require.True(t, bytes.HasPrefix(ciphertextAfter, []byte("age-encryption.org/v1")))
	}
	var workflows, effects int
	require.NoError(t, admin.QueryRow(`SELECT count(*) FROM `+pq.QuoteIdentifier(f.config.Postgres.SystemSchema)+`.workflow_status`).Scan(&workflows))
	require.NoError(t, admin.QueryRow(`SELECT count(*) FROM `+table+`.rollout_effects`).Scan(&effects))
	require.Zero(t, workflows, "old format must refuse before DBOS recovery")
	require.Zero(t, effects)
}
