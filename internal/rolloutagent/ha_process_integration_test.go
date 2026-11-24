//go:build integration && (linux || darwin)

package rolloutagent

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stretchr/testify/require"
)

func TestHAWorkersSharePoolCapacityAndRejectDuplicateSlot(t *testing.T) {
	f := newHAProcessFixture(t)
	first := f.start(t, "worker-0", true)
	commands := []rollout.Command{f.command(t, "reviewed")}
	f.publish(t, commands[0])
	f.progress(t, commands[0].ID, "awaiting_approval")
	f.start(t, "worker-1", true)
	duplicate := f.start(t, "worker-0", false)
	select {
	case err := <-duplicate.done:
		duplicate.exited = true
		require.Error(t, err, "a live slot accepted a second OS process")
	case <-time.After(10 * time.Second):
		t.Fatal("duplicate worker did not refuse startup")
	}
	var generation int64
	require.NoError(t, f.db.QueryRow(`SELECT generation FROM `+f.table("rollout_workers")+` WHERE worker='worker-0'`).Scan(&generation))
	require.EqualValues(t, 1, generation)
	for range 4 {
		commands = append(commands, f.command(t, "reviewed"))
		f.publish(t, commands[len(commands)-1])
	}
	system := pq.QuoteIdentifier(f.pg.SystemSchema) + ".workflow_status"
	require.Eventually(t, func() bool {
		var pending, enqueued, workers int
		err := f.db.QueryRow(`SELECT count(*) FILTER(WHERE status='PENDING'),count(*) FILTER(WHERE status='ENQUEUED'),
 (SELECT count(DISTINCT owner_worker) FROM `+f.table("rollout_commands")+` WHERE owner_worker<>'') FROM `+system).Scan(&pending, &enqueued, &workers)
		return err == nil && pending == 4 && enqueued == 1 && workers == 2
	}, 25*time.Second, 25*time.Millisecond, "pool capacity or native ownership did not converge")
	require.Zero(t, f.wire.operations(t, "apply"))
	f.action(t, first, commands[0].ID, "cancel", "")
	f.progress(t, commands[0].ID, "cancelled")
	for _, command := range commands[1:] {
		f.progress(t, command.ID, "awaiting_approval")
		f.action(t, first, command.ID, "cancel", "")
		f.progress(t, command.ID, "cancelled")
	}
	require.Zero(t, f.wire.operations(t, "apply"))
}

func TestHAReplacementRecoversApprovalOnOtherLiveSlot(t *testing.T) {
	f := newHAProcessFixture(t)
	first := f.start(t, "worker-0", true)
	command := f.command(t, "reviewed")
	f.publish(t, command)
	before := f.progress(t, command.ID, "awaiting_approval")
	owner, generation := f.owner(t, command.ID)
	require.Equal(t, "worker-0", owner)
	require.EqualValues(t, 1, generation)
	f.start(t, "worker-1", true)
	first.kill(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	tx, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer haRollback(t, tx)
	var id string
	system := pq.QuoteIdentifier(f.pg.SystemSchema) + ".workflow_status"
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT workflow_uuid FROM `+system+` WHERE workflow_uuid=$1 FOR UPDATE`, command.ID).Scan(&id))
	replacement := f.start(t, "worker-0", false)
	f.waitBlockedRecovery(t)
	haSignal(t, replacement, syscall.SIGSTOP)
	require.NoError(t, tx.Commit())
	require.Eventually(t, func() bool {
		var worker string
		err := f.db.QueryRow(`SELECT owner_worker FROM `+f.table("rollout_commands")+` WHERE id=$1`, command.ID).Scan(&worker)
		return err == nil && worker == "worker-1"
	}, 15*time.Second, 25*time.Millisecond, "recovered workflow did not run on the surviving OS worker")
	haSignal(t, replacement, syscall.SIGCONT)
	f.ready(t, replacement)
	f.action(t, replacement, command.ID, "approve", before.Subject)
	f.progress(t, command.ID, "completed")
	require.Equal(t, 1, f.wire.operations(t, "plan"), "recovery repeated a cached real plan")
	require.Equal(t, 1, f.wire.operations(t, "apply"))
	require.Equal(t, 1, f.count(t, "rollout_effects", command.ID))
	f.stateMarker(t, command.ID)
}

func TestHAGracefulStopPreservesPendingRecovery(t *testing.T) {
	f := newHAProcessFixture(t)
	first := f.start(t, "worker-0", true)
	command := f.command(t, "reviewed")
	f.publish(t, command)
	before := f.progress(t, command.ID, "awaiting_approval")
	peer := f.start(t, "worker-1", true)
	haSignal(t, first, syscall.SIGTERM)
	select {
	case err := <-first.done:
		first.exited = true
		require.NoError(t, err, "ordinary supervised shutdown must not be reported as a worker failure")
	case <-time.After(15 * time.Second):
		t.Fatal("owned worker did not stop gracefully")
	}
	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM `+pq.QuoteIdentifier(f.pg.SystemSchema)+`.workflow_status WHERE workflow_uuid=$1`, command.ID).Scan(&status))
	require.Equal(t, "PENDING", status, "graceful replacement poisoned the waiting workflow")
	f.start(t, "worker-0", true)
	f.action(t, peer, command.ID, "approve", before.Subject)
	f.progress(t, command.ID, "completed")
	require.Equal(t, 1, f.wire.operations(t, "plan"))
	require.Equal(t, 1, f.wire.operations(t, "apply"))
	f.assertNoWorkflowErrors(t, command.ID)
	f.stateMarker(t, command.ID)
}

func (f *haProcessFixture) waitBlockedRecovery(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		var blocked bool
		err := f.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE $1 AND query LIKE '%owner_xid = NULL%')`, "%"+f.pg.SystemSchema+"%").Scan(&blocked)
		return err == nil && blocked
	}, 10*time.Second, 20*time.Millisecond, "replacement did not reach the real public SDK recovery statement")
}
