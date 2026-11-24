//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestNativeCatalogueUpdateCLIUsesExplicitAdminAndKeepsPrivateOutput(t *testing.T) {
	binary, dsnFile := os.Getenv("HA_RUNNER_BINARY"), os.Getenv("RUNNER_POSTGRES_TEST_DSN_FILE")
	require.NotEmpty(t, binary, "qualified actual Runner binary is required")
	require.NotEmpty(t, dsnFile, "isolated PostgreSQL fixture is required")
	cfg := catalogueCLIFixture(t)
	cfg.Directory, cfg.Listen = filepath.Join(t.TempDir(), "maintenance-scratch"), "127.0.0.1:48080"
	cfg.RunnerUUID = uuid.NewString()
	cfg.Postgres.DSNFile, cfg.Postgres.WorkerID = dsnFile, "catalogue-cli"
	cfg.Postgres.Concurrency, cfg.Postgres.WorkerConcurrency = 2, 1
	cfg.Postgres.ApplicationSchema = "catalogue_cli_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	cfg.Postgres.SystemSchema = cfg.Postgres.ApplicationSchema + "_dbos"
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	cfg.PublicKeyFile = filepath.Join(t.TempDir(), "verification-key")
	require.NoError(t, os.WriteFile(cfg.PublicKeyFile, []byte(base64.StdEncoding.EncodeToString(public)), 0400))
	configFile := filepath.Join(t.TempDir(), "config.json")
	rawDSN, err := os.ReadFile(dsnFile)
	require.NoError(t, err)
	admin, err := sql.Open("postgres", string(rawDSN))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.Exec(`DROP SCHEMA IF EXISTS ` + pq.QuoteIdentifier(cfg.Postgres.ApplicationSchema) + ` CASCADE; DROP SCHEMA IF EXISTS ` + pq.QuoteIdentifier(cfg.Postgres.SystemSchema) + ` CASCADE`)
		require.NoError(t, err)
		require.NoError(t, admin.Close())
	})
	run := func(success bool, args ...string) []byte {
		t.Helper()
		raw, err := json.Marshal(cfg)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(configFile, raw, 0600))
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, args...)
		command.Env = append(os.Environ(), "ROLLOUT_CONFIG_FILE="+configFile)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err = command.Run()
		if success {
			require.NoError(t, err, "%s", stderr.String())
		} else {
			require.Error(t, err)
			require.Empty(t, stdout.String())
		}
		for _, secret := range []string{string(rawDSN), cfg.Tools[0].Environment["PRIVATE_CREDENTIAL"], cfg.Postgres.IdentityFile} {
			require.NotContains(t, stdout.String()+stderr.String(), secret)
		}
		return stdout.Bytes()
	}
	run(true, "rollout-storage-init")
	var manifest rolloutCatalogueExport
	require.NoError(t, json.Unmarshal(run(true, "rollout-catalogue"), &manifest))
	oldDigest := manifest.NativeExecution.ToolsDigest
	cfg.Tools[0].Environment["PRIVATE_CREDENTIAL"] = "UPDATED_PRIVATE_INVENTORY_CLI_CANARY"
	run(false, "rollout-catalogue-update")
	output := run(true, "--expected-catalogue="+oldDigest, "--admin-dsn-file="+dsnFile, "rollout-catalogue-update")
	require.JSONEq(t, `{"status":"catalogue_ready"}`, string(output))
	require.NoError(t, json.Unmarshal(run(true, "rollout-catalogue"), &manifest))
	require.NotEqual(t, oldDigest, manifest.NativeExecution.ToolsDigest)
	require.JSONEq(t, `{"status":"catalogue_ready"}`, string(run(true, "--expected-catalogue="+oldDigest, "--admin-dsn-file="+dsnFile, "rollout-catalogue-update")))
}
