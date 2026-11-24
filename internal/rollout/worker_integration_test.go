//go:build integration

package rollout

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func secondWorker(t *testing.T, f *fixture, id string) *Engine {
	t.Helper()
	cfg := f.config
	pg := *cfg.Postgres
	pg.WorkerID = id
	cfg.Postgres = &pg
	cfg.Directory = filepath.Join(t.TempDir(), id)
	executor := *cfg.Executor
	cfg.Executor = &executor
	engine, err := Open(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { closeFixture(t, engine) })
	return engine
}

func TestPostgresAtomicEnqueueRollsBackNativeAcceptance(t *testing.T) {
	f, _, admin := postgresFixture(t, "apply")
	system := pq.QuoteIdentifier(f.config.Postgres.SystemSchema)
	_, err := admin.Exec(`CREATE FUNCTION ` + system + `.refuse_enqueue() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'PRIVATE_QUEUE_CANARY'; END; $$;
 CREATE TRIGGER refuse_enqueue BEFORE INSERT ON ` + system + `.workflow_status FOR EACH ROW EXECUTE FUNCTION ` + system + `.refuse_enqueue()`)
	require.NoError(t, err)
	_, err = f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.EqualError(t, err, "storage_unavailable")
	var refusal *Error
	require.False(t, errors.As(err, &refusal))
	require.NotContains(t, err.Error(), "PRIVATE_QUEUE_CANARY")
	require.False(t, f.engine.Ready())
	for _, table := range []string{"rollout_commands", "rollout_environments", "rollout_events", "rollout_plan_uses"} {
		var count int
		require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM `+table).Scan(&count))
		require.Zero(t, count, table+" survived failed queue admission")
	}
	var workflows int
	require.NoError(t, admin.QueryRow(`SELECT count(*) FROM `+system+`.workflow_status`).Scan(&workflows))
	require.Zero(t, workflows)
}

func TestPostgresConcurrentFirstImportHasOneWorkflow(t *testing.T) {
	f, trace, admin := postgresFixture(t, "apply")
	peer := secondWorker(t, f, "worker-1")
	signed := f.signed(t, f.command)
	results := make(chan error, 2)
	for _, engine := range []*Engine{f.engine, peer} {
		go func() { _, err := engine.Submit(t.Context(), signed); results <- err }()
	}
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	waitPhase(t, f.engine, f.command.ID, "completed")
	require.Equal(t, 1, operationCount(t, trace, "apply"))
	var workflows, commands int
	require.NoError(t, admin.QueryRow(`SELECT count(*) FROM `+pq.QuoteIdentifier(f.config.Postgres.SystemSchema)+`.workflow_status`).Scan(&workflows))
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_commands`).Scan(&commands))
	require.Equal(t, 1, workflows)
	require.Equal(t, 1, commands)
	require.True(t, f.engine.Ready())
	require.True(t, peer.Ready())
}

func TestPostgresExpectedEnvironmentRaceDoesNotStopWorker(t *testing.T) {
	f, _, _ := postgresFixture(t, "reviewed")
	peer := secondWorker(t, f, "worker-1")
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
	other := f.command
	other.ID, other.Request.ID = "busy-candidate", "busy-candidate"
	_, err = peer.Submit(t.Context(), f.signed(t, other))
	requireCode(t, err, "environment_busy")
	require.True(t, peer.Ready())
}

func assertWorkerStopped(t *testing.T, f *fixture, err error) {
	t.Helper()
	require.Error(t, err)
	select {
	case reason := <-f.engine.Fatal():
		require.EqualError(t, reason, "storage_unavailable")
	case <-time.After(3 * time.Second):
		t.Fatal("native database failure did not stop the worker")
	}
	require.False(t, f.engine.Ready())
	_, err = f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.EqualError(t, err, "storage_unavailable")
}

func TestPostgresNativeQueryFailureStopsWorker(t *testing.T) {
	f, _, _ := postgresFixture(t, "plan")
	_, err := f.engine.db.Exec(`SELECT 1/0 /* PRIVATE_QUERY_CANARY */`)
	assertWorkerStopped(t, f, err)
}

func TestPostgresNativeCommitFailureStopsWorker(t *testing.T) {
	f, _, _ := postgresFixture(t, "plan")
	_, err := f.engine.db.Exec(`CREATE TABLE tracer_parent(id INTEGER PRIMARY KEY); CREATE TABLE tracer_child(id INTEGER REFERENCES tracer_parent(id) DEFERRABLE INITIALLY DEFERRED)`)
	require.NoError(t, err)
	tx, err := f.engine.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO tracer_child(id) VALUES(99)`)
	require.NoError(t, err)
	assertWorkerStopped(t, f, tx.Commit())
}

func TestPostgresNativeIterationFailureStopsWorker(t *testing.T) {
	f, _, _ := postgresFixture(t, "plan")
	rows, err := f.engine.db.Query(`SELECT 100/(value-2) FROM generate_series(1,3) AS value`)
	if err == nil {
		for rows.Next() {
			var value int
			require.NoError(t, rows.Scan(&value))
		}
		err = rows.Err()
		require.NoError(t, rows.Close())
	}
	assertWorkerStopped(t, f, err)
}

func TestPostgresRejectedSlotConnectionsAreClosed(t *testing.T) {
	f, _, admin := postgresFixture(t, "plan")
	for range 3 {
		cfg := f.config
		cfg.Directory = filepath.Join(t.TempDir(), "duplicate")
		executor := *cfg.Executor
		cfg.Executor = &executor
		engine, err := Open(t.Context(), cfg)
		require.Nil(t, engine)
		require.EqualError(t, err, "storage_unavailable")
	}
	var connections int
	require.NoError(t, admin.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE application_name=$1`, "stellwerk-native:"+f.config.Postgres.ApplicationSchema+":"+f.config.Postgres.WorkerID).Scan(&connections))
	require.Equal(t, 1, connections, "failed AfterConnect leaked native sessions")
	require.True(t, f.engine.Ready())
}

func TestPostgresCancelledRequestPreservesLiveOwnership(t *testing.T) {
	f, _, admin := postgresFixture(t, "plan")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := f.engine.db.ExecContext(ctx, `SELECT pg_sleep(30)`); done <- err }()
	require.Eventually(t, func() bool {
		var active bool
		err := admin.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND state='active' AND query='SELECT pg_sleep(30)')`, "stellwerk-native:"+f.config.Postgres.ApplicationSchema+":"+f.config.Postgres.WorkerID).Scan(&active)
		return err == nil && active
	}, 3*time.Second, 20*time.Millisecond)
	cancel()
	require.Error(t, <-done)
	tx, err := f.engine.beginCommand(t.Context())
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.True(t, f.engine.Ready(), "human request cancellation became a worker failure")
}

func TestPostgresQueueRegistrationNeverOverwritesLimits(t *testing.T) {
	f, _, admin := postgresFixture(t, "plan")
	closeFixture(t, f.engine)
	pg := *f.config.Postgres
	pg.Concurrency = 5
	cfg := f.config
	cfg.Postgres = &pg
	err := InitializePostgres(t.Context(), cfg)
	requireCode(t, err, "queue_configuration_refused")
	engine, err := Open(t.Context(), cfg)
	require.Nil(t, engine)
	requireCode(t, err, "queue_configuration_refused")
	var global, worker int
	require.NoError(t, admin.QueryRow(`SELECT concurrency,worker_concurrency FROM `+pq.QuoteIdentifier(pg.SystemSchema)+`.queues WHERE name=$1`, rolloutQueue).Scan(&global, &worker))
	require.Equal(t, 4, global)
	require.Equal(t, 2, worker)
}

func TestPostgresAbortedCommitCannotMasqueradeAsSuccess(t *testing.T) {
	f, _, _ := postgresFixture(t, "reviewed")
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
	tx, err := f.engine.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO rollout_environments(environment,rollout) VALUES($1,$2)`, f.command.EnvironmentID, f.command.ID)
	require.Error(t, err)
	require.True(t, f.engine.Ready(), "expected environment conflict alone stopped the worker")
	err = tx.Commit()
	require.Error(t, err)
	require.False(t, f.engine.Ready(), "ROLLBACK command tag was treated as a successful commit")
	select {
	case reason := <-f.engine.Fatal():
		require.EqualError(t, reason, "storage_unavailable")
	case <-time.After(time.Second):
		t.Fatal("aborted commit did not signal failure")
	}
}

func TestPostgresWorkflowStorageDeadlineStopsWorker(t *testing.T) {
	f, _, _ := postgresFixture(t, "plan")
	ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), workflowOwnerKey{}, true), 100*time.Millisecond)
	defer cancel()
	_, err := f.engine.db.ExecContext(ctx, `SELECT pg_sleep(30)`)
	assertWorkerStopped(t, f, err)
}

func TestPostgresImportSerializesSavedPlanFreshnessWithPeerMutation(t *testing.T) {
	f, trace, admin := postgresFixture(t, "plan")
	f.command.PlanBase = &PlanBase{BaseDeploymentID: emptyPlanBase}
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "completed")
	peer := secondWorker(t, f, "worker-1")
	mutation := f.command
	mutation.ID, mutation.Request.ID = "newer-mutation", "newer-mutation"
	mutation.Strategy, mutation.Request.Mode, mutation.PlanBase = "apply", "deploy", nil
	require.NoError(t, peer.validate(mutation))
	tx, err := peer.beginCommand(t.Context())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, peer.lockImport(t.Context(), tx, mutation))
	child := savedCommand(t, f, f.command, "saved-child")
	signed := f.signed(t, child)
	result := make(chan error, 1)
	go func() { _, err := f.engine.Submit(t.Context(), signed); result <- err }()
	require.Eventually(t, func() bool {
		var blocked bool
		err := admin.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND wait_event='advisory')`, "stellwerk-native:"+f.config.Postgres.ApplicationSchema+":"+f.config.Postgres.WorkerID).Scan(&blocked)
		return err == nil && blocked
	}, 3*time.Second, 20*time.Millisecond, "saved Plan freshness bypassed the environment import boundary")
	_, err = peer.acceptCommand(t.Context(), tx, mutation, f.signed(t, mutation).Payload)
	require.NoError(t, err)
	requireCode(t, <-result, "saved_plan_superseded")
	waitPhase(t, f.engine, mutation.ID, "completed")
	var uses int
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_plan_uses`).Scan(&uses))
	require.Zero(t, uses)
	require.Equal(t, 1, operationCount(t, trace, "plan"))
	require.Equal(t, 1, operationCount(t, trace, "apply"))
}

func TestPostgresURIAndLeadingTimeoutRetainValidProfile(t *testing.T) {
	f, _, _ := postgresFixture(t, "plan")
	closeFixture(t, f.engine)
	raw, err := readStorageSecret(f.config.Postgres.DSNFile, 8192)
	require.NoError(t, err)
	parsed, err := pgx.ParseConfig(string(raw))
	require.NoError(t, err)
	uri := url.URL{Scheme: "postgres", Host: net.JoinHostPort(parsed.Host, strconv.Itoa(int(parsed.Port))), Path: "/" + parsed.Database, User: url.UserPassword(parsed.User, parsed.Password)}
	query := url.Values{"connect_timeout": []string{"3"}, "sslmode": []string{"disable"}}
	uri.RawQuery = query.Encode()
	path := filepath.Join(t.TempDir(), "dsn")
	f.config.Postgres.DSNFile = path
	for _, dsn := range []string{uri.String(), "connect_timeout=3 " + string(raw)} {
		require.NoError(t, os.WriteFile(path, []byte(dsn), 0600))
		f.engine, err = Open(t.Context(), f.config)
		require.NoError(t, err)
		require.True(t, f.engine.Ready())
		closeFixture(t, f.engine)
	}
}
