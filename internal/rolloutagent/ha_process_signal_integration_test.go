//go:build integration && (linux || darwin)

package rolloutagent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stretchr/testify/require"
)

func TestHASignalDrainsReplacementWithActiveApply(t *testing.T) {
	f := newHAProcessFixture(t)
	gate := f.gateApply(t, "after")
	first := f.start(t, "worker-0", true)
	f.control(t, first, true, "--wait", "rollout-drain")
	f.stopDrainedWorker(t, first)
	config := filepath.Join(filepath.Dir(first.log), "config.json")
	replacement := &haWorkerProcess{command: exec.Command(f.binary, "rollout"), done: make(chan error, 1), url: first.url, log: filepath.Join(filepath.Dir(first.log), "replacement.log")}
	replacement.command.Env = append(os.Environ(), "ROLLOUT_CONFIG_FILE="+config)
	log, err := os.OpenFile(replacement.log, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	require.NoError(t, err)
	replacement.command.Stdout, replacement.command.Stderr = log, log
	require.NoError(t, replacement.command.Start())
	go func() { replacement.done <- replacement.command.Wait(); _ = log.Close() }()
	f.workers = append(f.workers, replacement)
	t.Cleanup(func() { replacement.kill(t) })
	f.ready(t, replacement)
	require.True(t, f.workerStatus(t, replacement).Accepting)
	command := f.command(t, "apply")
	f.publish(t, command)
	gate.entered(t)
	f.stateMarker(t, command.ID)
	haSignal(t, replacement, syscall.SIGTERM)
	f.waitSignalDrain(t, replacement)
	haSignal(t, replacement, syscall.SIGTERM)
	_, alive := gate.process(t)
	require.True(t, alive, "a repeated graceful signal killed the admitted Apply")
	gate.release(t)
	f.waitSignalExit(t, replacement, true)
	f.start(t, "worker-0", true)
	f.progress(t, command.ID, "completed")
	f.assertNoWorkflowErrors(t, command.ID)
	f.waitResultOwner(t, command.ID)
	require.Equal(t, 1, f.wire.operations(t, "apply"))
	f.stateMarker(t, command.ID)
}

func TestHASignalDrainsActiveReceiptTask(t *testing.T) {
	f := newHAProcessFixture(t)
	gate, receipt := f.gateReceiptTask(t)
	first := f.start(t, "worker-0", true)
	command := f.command(t, "apply")
	f.publish(t, command)
	gate.entered(t)
	before, err := os.ReadFile(receipt)
	require.NoError(t, err)
	haSignal(t, first, syscall.SIGINT)
	f.waitSignalDrain(t, first)
	_, alive := gate.process(t)
	require.True(t, alive, "SIGINT killed the admitted receipt task")
	gate.release(t)
	f.waitSignalExit(t, first, true)
	f.start(t, "worker-0", true)
	f.progress(t, command.ID, "completed")
	after, err := os.ReadFile(receipt)
	require.NoError(t, err)
	require.Equal(t, before, after)
	runs, err := os.ReadFile(filepath.Join(gate.directory, "runs"))
	require.NoError(t, err)
	require.Equal(t, 1, bytes.Count(runs, []byte("\n")))
	require.Equal(t, 1, f.wire.operations(t, "apply"))
	f.assertNoWorkflowErrors(t, command.ID)
	f.waitResultOwner(t, command.ID)
}

func TestHASignalDrainRetainsFatalAuthorityLoss(t *testing.T) {
	f := newHAProcessFixture(t)
	gate := f.gateApply(t, "after")
	first := f.start(t, "worker-0", true)
	command := f.command(t, "apply")
	f.publish(t, command)
	gate.entered(t)
	haSignal(t, first, syscall.SIGTERM)
	f.waitSignalDrain(t, first)
	var terminated bool
	require.NoError(t, f.db.QueryRow(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name=$1`, "stellwerk-native:"+f.pg.ApplicationSchema+":worker-0").Scan(&terminated))
	require.True(t, terminated)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, first.url+"/v1/rollouts", nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+f.token)
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
	if err == nil {
		require.NoError(t, response.Body.Close())
	}
	f.waitSignalExit(t, first, false)
	f.start(t, "worker-0", true)
	f.progress(t, command.ID, "outcome_unknown")
	f.assertHeldClaim(t, command.ID)
	require.Equal(t, 1, f.wire.operations(t, "apply"))
	f.stateMarker(t, command.ID)
}

func TestHASignalPreservesFirstShutdownFailure(t *testing.T) {
	f := newHAProcessFixture(t)
	gate := f.gateApply(t, "after")
	first := f.start(t, "worker-0", true)
	command := f.command(t, "apply")
	f.publish(t, command)
	gate.entered(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	tx, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer haRollback(t, tx)
	_, err = tx.ExecContext(ctx, `LOCK TABLE `+pq.QuoteIdentifier(f.pg.SystemSchema)+`.operation_outputs IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	gate.release(t)
	f.progress(t, command.ID, "applied")
	require.Eventually(t, func() bool { return f.workerStatus(t, first).ActiveStages == 0 }, 5*time.Second, 25*time.Millisecond)
	haSignal(t, first, syscall.SIGTERM)
	f.waitSignalExit(t, first, false)
	require.NoError(t, tx.Commit())
	f.start(t, "worker-0", true)
	f.progress(t, command.ID, "completed")
	f.assertNoWorkflowErrors(t, command.ID)
	require.Equal(t, 1, f.wire.operations(t, "apply"))
}

func (f *haProcessFixture) waitSignalDrain(t *testing.T, worker *haWorkerProcess) {
	t.Helper()
	require.Eventually(t, func() bool {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, worker.url+"/v1/worker/status", nil)
		if err != nil {
			return false
		}
		request.Header.Set("Authorization", "Bearer "+f.token)
		response, err := (&http.Client{Timeout: time.Second}).Do(request)
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		var status rollout.WorkerStatus
		return json.NewDecoder(response.Body).Decode(&status) == nil && status.AuthorityAlive && status.Draining && !status.Accepting && status.ActiveStages == 1
	}, 5*time.Second, 25*time.Millisecond, "stop request did not preserve the active native operation")
}

func (f *haProcessFixture) waitSignalExit(t *testing.T, worker *haWorkerProcess, success bool) {
	t.Helper()
	select {
	case err := <-worker.done:
		worker.exited = true
		if success {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("signaled owned worker did not finish its shutdown")
	}
}
