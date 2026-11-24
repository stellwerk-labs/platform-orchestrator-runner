//go:build integration

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stretchr/testify/require"
)

func TestNativeStopRetainsAlreadyIdleAuthorityFailure(t *testing.T) {
	dsnFile := os.Getenv("RUNNER_POSTGRES_TEST_DSN_FILE")
	require.NotEmpty(t, dsnFile, "the required native lifecycle gate needs its owned PostgreSQL fixture")
	dsn, err := os.ReadFile(dsnFile)
	require.NoError(t, err)
	db, err := sql.Open("postgres", string(dsn))
	require.NoError(t, err)
	app := "native_signal_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	t.Cleanup(func() {
		_, dropErr := db.Exec(`DROP SCHEMA IF EXISTS ` + pq.QuoteIdentifier(app) + ` CASCADE; DROP SCHEMA IF EXISTS ` + pq.QuoteIdentifier(app+"_dbos") + ` CASCADE`)
		require.NoError(t, dropErr)
		require.NoError(t, db.Close())
	})
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	root := t.TempDir()
	key := filepath.Join(root, "site.agekey")
	require.NoError(t, os.WriteFile(key, []byte(identity.String()), 0400))
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	config := rollout.Config{Directory: filepath.Join(root, "native"), OrgID: "org", RunnerID: "site", RunnerUUID: uuid.NewString(), PublicKey: public, Executor: &rolloutexec.Executor{},
		Postgres: &rollout.PostgresConfig{Concurrency: 2, WorkerConcurrency: 1, DSNFile: dsnFile, IdentityFile: key, WorkerID: "worker-0", ApplicationSchema: app, SystemSchema: app + "_dbos"}}
	require.NoError(t, rollout.InitializePostgres(t.Context(), config))
	engine, err := rollout.Open(t.Context(), config)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, engine.Close(ctx))
	})
	require.Zero(t, engine.WorkerStatus().ActiveStages)
	var terminated bool
	require.NoError(t, db.QueryRow(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name=$1`, "stellwerk-native:"+app+":worker-0").Scan(&terminated))
	require.True(t, terminated)
	_, err = engine.Get(t.Context(), "not-an-admitted-command")
	require.Error(t, err)
	require.False(t, engine.Ready(), "owned backend loss must already have stopped authority")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = serveRollout(ctx, engine, &http.Server{Addr: "127.0.0.1:0", Handler: http.NewServeMux(), ReadHeaderTimeout: time.Second})
	require.Error(t, err, "an already-idle stage count must not turn pending authority loss into clean shutdown")
}
