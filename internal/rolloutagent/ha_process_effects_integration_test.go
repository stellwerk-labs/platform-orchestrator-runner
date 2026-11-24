//go:build integration && (linux || darwin)

package rolloutagent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

type haApplyGate struct {
	directory string
	root      string
	killed    bool
}

func (f *haProcessFixture) gateApply(t *testing.T, boundary string) *haApplyGate {
	t.Helper()
	require.Contains(t, []string{"before", "after"}, boundary)
	binary, err := exec.LookPath("tofu")
	require.NoError(t, err)
	directory := filepath.Join(f.root, "apply-gate")
	require.NoError(t, os.Mkdir(directory, 0700))
	gate := &haApplyGate{directory: directory, root: f.root}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	probe := "if [ -e " + quote(filepath.Join(directory, "probe")) + " ] && [ ! -e " + quote(filepath.Join(directory, "probe-ok")) + " ]; then " + quote(binary) + " \"$1\" state pull >/dev/null && touch " + quote(filepath.Join(directory, "probe-ok")) + "; fi\n"
	marker := "printf '%s\\n' \"$$\" > " + quote(filepath.Join(directory, "pid")) + "\ntouch " + quote(filepath.Join(directory, "entered")) + "\nwhile [ ! -e " + quote(filepath.Join(directory, "release")) + " ]; do " + probe + "sleep 0.02; done\n"
	script := "#!/bin/sh\ncase \" $* \" in *\" apply \"*) ;; *) exec " + quote(binary) + " \"$@\" ;; esac\n"
	if boundary == "before" {
		script += marker
	}
	script += quote(binary) + " \"$@\"\nstatus=$?\n[ \"$status\" -eq 0 ] || exit \"$status\"\n"
	if boundary == "after" {
		script += marker
	}
	script += "exit \"$status\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(directory, "tofu"), []byte(script), 0700))
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() { gate.killOrphan(t) })
	return gate
}

func (gate *haApplyGate) entered(t *testing.T) {
	t.Helper()
	haWaitFile(t, filepath.Join(gate.directory, "entered"))
}

func (gate *haApplyGate) release(t *testing.T) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(gate.directory, "release"), nil, 0600))
}

func (gate *haApplyGate) probeBackend(t *testing.T) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(gate.directory, "probe"), nil, 0600))
	haWaitFile(t, filepath.Join(gate.directory, "probe-ok"))
}

func (gate *haApplyGate) process(t *testing.T) (*os.Process, bool) {
	t.Helper()
	if gate.killed {
		return nil, false
	}
	body, err := os.ReadFile(filepath.Join(gate.directory, "pid"))
	if os.IsNotExist(err) {
		return nil, false
	}
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	require.NoError(t, err)
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=", "-o", "command=").Output()
	if err != nil {
		return nil, false
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 || strings.HasPrefix(fields[0], "Z") {
		return nil, false
	}
	require.Contains(t, string(output), gate.root, "refusing to signal a PID outside this owned fixture")
	process, err := os.FindProcess(pid)
	require.NoError(t, err)
	return process, true
}

func (gate *haApplyGate) killOrphan(t *testing.T) {
	t.Helper()
	process, alive := gate.process(t)
	if alive {
		require.NoError(t, process.Kill())
		gate.killed = true
	}
}

func TestHAKilledWorkerRetainsUnknownEffectWithoutReplay(t *testing.T) {
	for _, boundary := range []string{"before", "after"} {
		t.Run(boundary+"_actual_apply", func(t *testing.T) {
			f := newHAProcessFixture(t)
			gate := f.gateApply(t, boundary)
			first := f.start(t, "worker-0", true)
			command := f.command(t, "apply")
			f.publish(t, command)
			gate.entered(t)
			f.start(t, "worker-1", true)
			require.Equal(t, 1, f.count(t, "rollout_effects", command.ID))
			if boundary == "after" {
				f.stateMarker(t, command.ID)
			}
			first.kill(t)
			_, alive := gate.process(t)
			require.True(t, alive, "fault did not leave the real effect subprocess orphaned")
			f.start(t, "worker-0", true)
			f.progress(t, command.ID, "outcome_unknown")
			f.assertHeldClaim(t, command.ID)
			want := 0
			if boundary == "after" {
				want = 1
			}
			require.Equal(t, want, f.wire.operations(t, "apply"), "replacement repeated a possibly started effect")
			gate.killOrphan(t)
		})
	}
}

func TestHADurableEffectResultReplaysBeforeWorkflowCheckpoint(t *testing.T) {
	f := newHAProcessFixture(t)
	gate := f.gateApply(t, "after")
	first := f.start(t, "worker-0", true)
	command := f.command(t, "apply")
	f.publish(t, command)
	gate.entered(t)
	f.stateMarker(t, command.ID)
	f.start(t, "worker-1", true)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	tx, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer haRollback(t, tx)
	_, err = tx.ExecContext(ctx, `LOCK TABLE `+pq.QuoteIdentifier(f.pg.SystemSchema)+`.operation_outputs IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	gate.release(t)
	require.Eventually(t, func() bool {
		var results int
		err := f.db.QueryRow(`SELECT count(*) FROM `+f.table("rollout_pipeline_results")+` WHERE rollout=$1`, command.ID).Scan(&results)
		return err == nil && results == 1
	}, 10*time.Second, 20*time.Millisecond, "real effect result did not commit before the blocked SDK checkpoint")
	f.progress(t, command.ID, "applied")
	first.kill(t)
	require.NoError(t, tx.Commit())
	f.start(t, "worker-0", true)
	f.progress(t, command.ID, "completed")
	require.Equal(t, 1, f.wire.operations(t, "apply"))
	require.Equal(t, 1, f.count(t, "rollout_pipeline_results", command.ID))
	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM `+f.table("rollout_effects")+` WHERE rollout=$1 AND step='apply'`, command.ID).Scan(&status))
	require.Equal(t, "done", status)
	f.assertNoWorkflowErrors(t, command.ID)
	f.waitResultOwner(t, command.ID)
	f.stateMarker(t, command.ID)
}

func TestHAOldWorkerCannotWriteAfterBackendLossAndReplacement(t *testing.T) {
	f := newHAProcessFixture(t)
	gate := f.gateApply(t, "after")
	first := f.start(t, "worker-0", true)
	command := f.command(t, "apply")
	f.publish(t, command)
	gate.entered(t)
	f.stateMarker(t, command.ID)
	f.start(t, "worker-1", true)
	haSignal(t, first, syscall.SIGSTOP)
	var pid int
	require.NoError(t, f.db.QueryRow(`SELECT pid FROM pg_stat_activity WHERE application_name=$1`, "stellwerk-native:"+f.pg.ApplicationSchema+":worker-0").Scan(&pid))
	var terminated bool
	require.NoError(t, f.db.QueryRow(`SELECT pg_terminate_backend($1,5000)`, pid).Scan(&terminated))
	require.True(t, terminated)
	_, alive := gate.process(t)
	require.True(t, alive, "the old effect child must retain independent backend access")
	f.start(t, "worker-0", true)
	before := f.progress(t, command.ID, "outcome_unknown")
	haSignal(t, first, syscall.SIGCONT)
	select {
	case err := <-first.done:
		first.exited = true
		require.Error(t, err, "superseded OS worker exited successfully")
	case <-time.After(20 * time.Second):
		t.Fatal("superseded OS worker did not fail-stop")
	}
	after := f.progress(t, command.ID, "outcome_unknown")
	require.Equal(t, before.Revision, after.Revision, "old worker wrote late progress after replacement")
	f.assertHeldClaim(t, command.ID)
	require.Equal(t, 1, f.wire.operations(t, "apply"))
	f.stateMarker(t, command.ID)
}

func TestHAWholeWorkerPartitionPreservesIndependentEffect(t *testing.T) {
	f := newHAProcessFixture(t)
	gate := f.gateApply(t, "after")
	proxy, dsn := f.postgresProxy(t)
	first := f.startWithDSN(t, "worker-0", dsn, true)
	command := f.command(t, "apply")
	f.publish(t, command)
	gate.entered(t)
	f.stateMarker(t, command.ID)
	f.start(t, "worker-1", true)
	haSignal(t, first, syscall.SIGSTOP)
	require.GreaterOrEqual(t, proxy.connectionCount(), 2, "native and DBOS pools must both use this worker's proxy")
	proxy.partition()
	gate.probeBackend(t)
	_, alive := gate.process(t)
	require.True(t, alive, "the old effect subprocess did not retain backend access across the database partition")
	require.Eventually(t, func() bool {
		var sessions int
		err := f.db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE application_name=$1`, "stellwerk-native:"+f.pg.ApplicationSchema+":worker-0").Scan(&sessions)
		return err == nil && sessions == 0
	}, 5*time.Second, 20*time.Millisecond)
	f.start(t, "worker-0", true)
	before := f.progress(t, command.ID, "outcome_unknown")
	haSignal(t, first, syscall.SIGCONT)
	select {
	case err := <-first.done:
		first.exited = true
		require.Error(t, err)
	case <-time.After(25 * time.Second):
		t.Fatal("partitioned OS worker did not fail-stop")
	}
	after := f.progress(t, command.ID, "outcome_unknown")
	require.Equal(t, before.Revision, after.Revision)
	f.assertHeldClaim(t, command.ID)
	require.Equal(t, 1, f.wire.operations(t, "apply"))
	f.stateMarker(t, command.ID)
}

func TestHAReceiptTaskRepairSurvivesWorkerReplacement(t *testing.T) {
	f := newHAProcessFixture(t)
	receipt := f.wire.addReceiptTask(t)
	f.config["tools"] = f.wire.config.Executor.Tools
	first := f.start(t, "worker-0", true)
	command := f.command(t, "apply")
	f.publish(t, command)
	failed := f.progress(t, command.ID, "post_failed")
	require.True(t, failed.InfrastructureApplied)
	require.Equal(t, "post", failed.FailedStep)
	before, err := os.ReadFile(receipt)
	require.NoError(t, err)
	var resultOwners int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM `+f.table("rollout_deliveries")+` WHERE result_owner=$1`, command.ID).Scan(&resultOwners))
	require.Zero(t, resultOwners, "post-task failure prematurely owned the final infrastructure result")
	peer := f.start(t, "worker-1", true)
	first.kill(t)
	f.start(t, "worker-0", true)
	f.operatorAction(t, peer, command.ID, "repair", map[string]string{"step_id": "post"})
	f.progress(t, command.ID, "completed")
	after, err := os.ReadFile(receipt)
	require.NoError(t, err)
	require.Equal(t, before, after, "receipt-safe repair changed the original effect identity")
	require.Equal(t, 1, f.wire.operations(t, "apply"))
	require.Equal(t, 1, f.count(t, "rollout_pipeline_results", command.ID))
	f.waitResultOwner(t, command.ID)
	f.assertNoWorkflowErrors(t, command.ID)
	f.stateMarker(t, command.ID)
}

func (f *haProcessFixture) assertHeldClaim(t *testing.T, id string) {
	t.Helper()
	var status, worker string
	var generation int64
	require.NoError(t, f.db.QueryRow(`SELECT status,owner_worker,owner_generation FROM `+f.table("rollout_effects")+` WHERE rollout=$1 AND step='apply'`, id).Scan(&status, &worker, &generation))
	require.Equal(t, "started", status)
	require.Equal(t, "worker-0", worker)
	require.EqualValues(t, 1, generation)
	require.Equal(t, 1, f.count(t, "rollout_environments", id), "unknown mutation lost its environment fence")
	require.Equal(t, 1, f.count(t, "rollout_no_effect_blocks", id), "unknown mutation became eligible for no-effect proof")
	require.Zero(t, f.count(t, "rollout_pipeline_results", id))
	f.assertNoWorkflowErrors(t, id)
}

func (f *haProcessFixture) assertNoWorkflowErrors(t *testing.T, id string) {
	t.Helper()
	var failures int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM `+pq.QuoteIdentifier(f.pg.SystemSchema)+`.operation_outputs WHERE workflow_uuid=$1 AND error IS NOT NULL`, id).Scan(&failures))
	require.Zero(t, failures, "an interrupted worker poisoned durable workflow recovery with an error checkpoint")
}

func (f *haProcessFixture) waitResultOwner(t *testing.T, id string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var owners int
		err := f.db.QueryRow(`SELECT count(*) FROM `+f.table("rollout_deliveries")+` WHERE result_owner=$1`, id).Scan(&owners)
		return err == nil && owners == 1
	}, 5*time.Second, 20*time.Millisecond, "terminal delivery did not retain exactly one result owner")
}
