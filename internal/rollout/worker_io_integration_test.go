//go:build integration

package rollout

import (
	"context"
	"database/sql"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func holdProtocolWrite(t *testing.T, f *fixture, admin *sql.DB, table string) func() {
	t.Helper()
	key := time.Now().UnixNano()
	control, err := admin.Conn(t.Context())
	require.NoError(t, err)
	_, err = control.ExecContext(t.Context(), `SELECT pg_advisory_lock($1)`, key)
	require.NoError(t, err)
	var once sync.Once
	release := func() {
		once.Do(func() {
			_, err := control.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
			require.NoError(t, err)
			require.NoError(t, control.Close())
		})
	}
	t.Cleanup(release)
	schema := pq.QuoteIdentifier(f.config.Postgres.SystemSchema)
	_, err = admin.Exec(`CREATE FUNCTION ` + schema + `.hold_protocol() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(` + strconv.FormatInt(key, 10) + `); RETURN NEW; END; $$;
 CREATE TRIGGER hold_protocol BEFORE INSERT ON ` + schema + `.` + pq.QuoteIdentifier(table) + ` FOR EACH ROW EXECUTE FUNCTION ` + schema + `.hold_protocol()`)
	require.NoError(t, err)
	return release
}

func awaitProtocolWrite(t *testing.T, f *fixture, admin *sql.DB) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting bool
		err := admin.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event='advisory')`, "stellwerk-native:"+f.config.Postgres.ApplicationSchema+":"+f.config.Postgres.WorkerID).Scan(&waiting)
		return err == nil && waiting
	}, 3*time.Second, 20*time.Millisecond)
}

func TestPostgresTransactionalCallerCancellationRollsBack(t *testing.T) {
	for _, operation := range []string{"enqueue", "send"} {
		t.Run(operation, func(t *testing.T) {
			f, _, admin := postgresFixture(t, "reviewed")
			var p Progress
			var err error
			table := "workflow_status"
			if operation == "send" {
				_, err = f.engine.Submit(t.Context(), f.signed(t, f.command))
				require.NoError(t, err)
				p = waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
				table = "notifications"
			}
			release := holdProtocolWrite(t, f, admin, table)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			signed := f.signed(t, f.command)
			done := make(chan error, 1)
			go func() {
				if operation == "enqueue" {
					_, err := f.engine.Submit(ctx, signed)
					done <- err
					return
				}
				_, err := f.engine.Decide(ctx, p.ID, p.Subject, "approve", "reviewer")
				done <- err
			}()
			awaitProtocolWrite(t, f, admin)
			cancel()
			select {
			case err = <-done:
				require.Error(t, err)
			case <-time.After(2 * time.Second):
				t.Fatal("transactional SQL ignored caller cancellation")
			}
			release()
			require.True(t, f.engine.Ready(), "ordinary caller cancellation stopped the actor")
			assertProtocolRollback(t, f, operation)
		})
	}
}

func assertProtocolRollback(t *testing.T, f *fixture, operation string) {
	t.Helper()
	if operation == "enqueue" {
		for _, table := range []string{"rollout_commands", "rollout_environments", "rollout_events"} {
			var count int
			require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM `+table).Scan(&count))
			require.Zero(t, count)
		}
		var workflows int
		require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM `+pq.QuoteIdentifier(f.config.Postgres.SystemSchema)+`.workflow_status`).Scan(&workflows))
		require.Zero(t, workflows)
		return
	}
	p, err := f.engine.Get(t.Context(), f.command.ID)
	require.NoError(t, err)
	require.Equal(t, "awaiting_approval", p.Phase)
	var audits, notifications int
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_approval_audit`).Scan(&audits))
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM `+pq.QuoteIdentifier(f.config.Postgres.SystemSchema)+`.notifications`).Scan(&notifications))
	require.Zero(t, audits)
	require.Zero(t, notifications)
}

func TestPostgresTransactionalStorageDeadlinePreservesPendingRecovery(t *testing.T) {
	f, trace, admin := postgresFixture(t, "reviewed")
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	p := waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
	release := holdProtocolWrite(t, f, admin, "notifications")
	done := make(chan error, 1)
	go func() { _, err := f.engine.Decide(t.Context(), p.ID, p.Subject, "approve", "reviewer"); done <- err }()
	awaitProtocolWrite(t, f, admin)
	select {
	case err = <-done:
		require.Error(t, err)
	case <-time.After(18 * time.Second):
		t.Fatal("transactional SQL exceeded its operation deadline")
	}
	release()
	require.False(t, f.engine.Ready())
	assertProtocolRollback(t, f, "send")
	reopenPostgres(t, f)
	p = waitPhase(t, f.engine, p.ID, "awaiting_approval")
	_, err = f.engine.Decide(t.Context(), p.ID, p.Subject, "approve", "reviewer")
	require.NoError(t, err)
	waitPhase(t, f.engine, p.ID, "completed")
	require.Equal(t, 1, operationCount(t, trace, "plan"))
	require.Equal(t, 1, operationCount(t, trace, "apply"))
}

func TestPostgresQueuedWorkOutlivesAdmissionCaller(t *testing.T) {
	f, trace, _ := postgresFixture(t, "reviewed")
	ctx, cancel := context.WithCancel(t.Context())
	_, err := f.engine.Submit(ctx, f.signed(t, f.command))
	require.NoError(t, err)
	cancel()
	p := waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
	var executionTimeout, executionDeadline int64
	require.NoError(t, f.engine.db.QueryRow(`SELECT coalesce(workflow_timeout_ms,0),coalesce(workflow_deadline_epoch_ms,0) FROM `+pq.QuoteIdentifier(f.config.Postgres.SystemSchema)+`.workflow_status WHERE workflow_uuid=$1`, p.ID).Scan(&executionTimeout, &executionDeadline))
	require.Zero(t, executionTimeout)
	require.Zero(t, executionDeadline)
	_, err = f.engine.Decide(t.Context(), p.ID, p.Subject, "approve", "reviewer")
	require.NoError(t, err)
	waitPhase(t, f.engine, p.ID, "completed")
	require.Equal(t, 1, operationCount(t, trace, "apply"))
}
