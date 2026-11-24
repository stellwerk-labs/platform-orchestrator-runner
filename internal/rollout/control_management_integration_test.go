//go:build integration

package rollout

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func holdControlRead(t *testing.T, f *fixture, admin *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := admin.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `LOCK TABLE `+pq.QuoteIdentifier(f.config.Postgres.SystemSchema)+`.workflow_status IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	t.Cleanup(func() {
		err := tx.Rollback()
		require.True(t, err == nil || errors.Is(err, sql.ErrTxDone))
	})
	return tx
}

func awaitControlRead(t *testing.T, f *fixture, admin *sql.DB) {
	t.Helper()
	// Match the pinned SDK management SELECT, not its background queue queries.
	require.Eventually(t, func() bool {
		var waiting bool
		err := admin.QueryRow(`SELECT EXISTS(
 SELECT 1 FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid
 WHERE l.relation=$1::regclass AND NOT l.granted
 AND a.query LIKE 'SELECT workflow_uuid, status, name, authenticated_user%')`, pq.QuoteIdentifier(f.config.Postgres.SystemSchema)+`.workflow_status`).Scan(&waiting)
		return err == nil && waiting
	}, 2*time.Second, 20*time.Millisecond)
}

func TestPostgresNativeControlManagementReadsHonorCaller(t *testing.T) {
	f, request, before, trace, admin := nativeControlFixture(t)
	request.Subject = strings.Repeat("f", 64)
	signed := signNativeControl(t, f, request)
	receipt, err := f.engine.AcceptControl(t.Context(), signed)
	require.NoError(t, err)
	for _, trial := range []struct {
		name                         string
		receipt, deadline, cancelled bool
	}{
		{"receipt/cancel", true, false, false},
		{"page/cancel", false, false, false},
		{"receipt/deadline", true, true, false},
		{"page/deadline", false, true, false},
		{"receipt/already_cancelled", true, false, true},
		{"page/already_cancelled", false, false, true},
	} {
		t.Run(trial.name, func(t *testing.T) {
			lock := holdControlRead(t, f, admin)
			ctx, cancel := context.WithCancel(t.Context())
			if trial.deadline {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 3*time.Second)
			}
			defer cancel()
			if trial.cancelled {
				cancel()
			}
			done := make(chan error, 1)
			go func() {
				if trial.receipt {
					_, err := f.engine.readControl(ctx, request.ID)
					done <- err
					return
				}
				_, err := f.engine.ControlDeliveries(ctx, 0, false)
				done <- err
			}()
			if !trial.cancelled {
				awaitControlRead(t, f, admin)
				if !trial.deadline {
					cancel()
				}
			}
			select {
			case err := <-done:
				require.Error(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("control management read ignored its caller")
			}
			require.NoError(t, lock.Rollback())
			require.True(t, f.engine.Ready(), "caller cancellation stopped healthy storage authority")
			after, err := f.engine.Get(t.Context(), request.RolloutID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			replayed, err := f.engine.AcceptControl(t.Context(), signed)
			require.NoError(t, err)
			require.Equal(t, receipt, replayed)
			require.Zero(t, operationCount(t, trace, "apply"))
		})
	}
}

func TestPostgresNativeControlAckCancelsInsideNotification(t *testing.T) {
	f, request, before, trace, admin := nativeControlFixture(t)
	request.Subject = strings.Repeat("f", 64)
	receipt, err := f.engine.AcceptControl(t.Context(), signNativeControl(t, f, request))
	require.NoError(t, err)
	release := holdProtocolWrite(t, f, admin, "notifications")
	for _, trial := range []struct {
		name     string
		deadline bool
	}{{"cancel", false}, {"deadline", true}} {
		t.Run(trial.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			if trial.deadline {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
			}
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- f.engine.AcknowledgeControl(ctx, request.ID, receipt.ResultDigest) }()
			// Native application_name and advisory wait prove Send reached its SQL
			// transaction after the preceding management receipt read completed.
			awaitProtocolWrite(t, f, admin)
			if !trial.deadline {
				cancel()
			}
			select {
			case err := <-done:
				require.Error(t, err)
			case <-time.After(7 * time.Second):
				t.Fatal("control acknowledgment notification ignored its caller")
			}
			require.True(t, f.engine.Ready(), "caller cancellation stopped healthy storage authority")
			after, err := f.engine.Get(t.Context(), request.RolloutID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			var notifications int
			err = admin.QueryRow(`SELECT count(*) FROM ` + pq.QuoteIdentifier(f.config.Postgres.SystemSchema) + `.notifications`).Scan(&notifications)
			require.NoError(t, err)
			require.Zero(t, notifications, "cancelled Send committed a notification")
		})
	}
	release()
	f.engine.Drain()
	require.NoError(t, f.engine.AcknowledgeControl(t.Context(), request.ID, receipt.ResultDigest))
	require.NoError(t, f.engine.AcknowledgeControl(t.Context(), request.ID, receipt.ResultDigest))
	require.Eventually(t, func() bool {
		rows, err := dbos.ListWorkflows(f.engine.durable, dbos.WithFilterWorkflowIDs(f.engine.controlWorkflowID(request.ID)))
		return err == nil && len(rows) == 1 && rows[0].Status == dbos.WorkflowStatusSuccess
	}, 5*time.Second, 20*time.Millisecond)
	retained, err := f.engine.readControl(t.Context(), request.ID)
	require.NoError(t, err)
	require.Equal(t, &receipt, retained)
	require.Zero(t, operationCount(t, trace, "apply"))
}

func TestPostgresNativeControlManagementReadHonorsRuntimeShutdown(t *testing.T) {
	f, request, _, trace, admin := nativeControlFixture(t)
	ended, err := f.engine.Decide(t.Context(), request.RolloutID, request.Subject, "deny", request.ActorID)
	require.NoError(t, err)
	require.Equal(t, "denied", ended.Phase)
	require.Eventually(t, func() bool {
		rows, err := dbos.ListWorkflows(f.engine.durable, dbos.WithFilterWorkflowIDs(request.RolloutID))
		return err == nil && len(rows) == 1 && rows[0].Status == dbos.WorkflowStatusSuccess
	}, 5*time.Second, 20*time.Millisecond)
	lock := holdControlRead(t, f, admin)
	read := make(chan error, 1)
	go func() { _, err := f.engine.ControlDeliveries(t.Context(), 0, false); read <- err }()
	awaitControlRead(t, f, admin)
	closed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		closed <- f.engine.Close(ctx)
	}()
	select {
	case err := <-read:
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("management query outlived its runtime cancellation")
	}
	require.NoError(t, t.Context().Err(), "the caller must remain live")
	require.NoError(t, lock.Rollback())
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("owned runtime did not close after its management query stopped")
	}
	require.Zero(t, operationCount(t, trace, "apply"))
}
