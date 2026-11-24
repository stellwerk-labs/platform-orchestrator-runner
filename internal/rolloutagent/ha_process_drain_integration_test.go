//go:build integration && (linux || darwin)

package rolloutagent

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func TestHAWorkerDrainPreservesActiveApplyAndPendingStages(t *testing.T) {
	f := newHAProcessFixture(t)
	gate := f.gateApply(t, "after")
	first := f.start(t, "worker-0", true)
	f.control(t, first, true, "rollout-ready")
	command := f.command(t, "apply")
	f.publish(t, command)
	gate.entered(t)
	f.stateMarker(t, command.ID)
	waiting := f.command(t, "reviewed")
	f.publish(t, waiting)
	approval := f.progress(t, waiting.ID, "awaiting_approval")
	f.control(t, first, false, "--wait", "--timeout=200ms", "rollout-drain")
	status := f.workerStatus(t, first)
	require.True(t, status.AuthorityAlive)
	require.False(t, status.Accepting)
	require.True(t, status.Draining)
	require.Equal(t, 1, status.ActiveStages)
	f.control(t, first, true, "rollout-status")
	f.control(t, first, false, "rollout-ready")
	_, alive := gate.process(t)
	require.True(t, alive, "drain timeout killed the healthy Apply")
	f.assertDrainMutationUnavailable(t, first, waiting.ID, approval.Subject)
	peer := f.start(t, "worker-1", true)
	f.action(t, peer, waiting.ID, "approve", approval.Subject)
	f.progress(t, waiting.ID, "approved")
	gate.release(t)
	f.control(t, first, true, "--wait", "--timeout=10s", "rollout-drain")
	f.progress(t, command.ID, "applied")
	require.Equal(t, 1, f.wire.operations(t, "apply"), "approval wakeup crossed the closed stage gate")
	require.Zero(t, f.workerStatus(t, first).ActiveStages)
	f.waitEventsAcked(t, command.ID)
	f.assertNoWorkflowErrors(t, command.ID)
	f.assertNoWorkflowErrors(t, waiting.ID)
	f.stopDrainedWorker(t, first)
	var pending int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM `+pq.QuoteIdentifier(f.pg.SystemSchema)+`.workflow_status WHERE workflow_uuid IN ($1,$2) AND status='PENDING'`, command.ID, waiting.ID).Scan(&pending))
	require.Equal(t, 2, pending)
	f.start(t, "worker-0", true)
	f.progress(t, command.ID, "completed")
	f.progress(t, waiting.ID, "completed")
	require.Equal(t, 2, f.wire.operations(t, "apply"), "replacement repeated the already committed Apply")
	f.waitResultOwner(t, command.ID)
	f.stateMarker(t, command.ID)
	f.stateMarker(t, waiting.ID)
}

func TestHAWorkerDrainPreservesActiveReceiptTaskAndFinalDelivery(t *testing.T) {
	f := newHAProcessFixture(t)
	gate, receipt := f.gateReceiptTask(t)
	first := f.start(t, "worker-0", true)
	command := f.command(t, "apply")
	f.publish(t, command)
	gate.entered(t)
	before, err := os.ReadFile(receipt)
	require.NoError(t, err)
	f.control(t, first, false, "--wait", "--timeout=200ms", "rollout-drain")
	require.Equal(t, 1, f.workerStatus(t, first).ActiveStages)
	gate.release(t)
	f.control(t, first, true, "--wait", "--timeout=10s", "rollout-drain")
	f.progress(t, command.ID, "completed")
	f.waitResultOwner(t, command.ID)
	f.waitEventsAcked(t, command.ID)
	require.True(t, f.workerStatus(t, first).AuthorityAlive)
	f.assertNoWorkflowErrors(t, command.ID)
	f.stopDrainedWorker(t, first)
	f.start(t, "worker-0", true)
	f.progress(t, command.ID, "completed")
	after, err := os.ReadFile(receipt)
	require.NoError(t, err)
	require.Equal(t, before, after)
	runs, err := os.ReadFile(filepath.Join(gate.directory, "runs"))
	require.NoError(t, err)
	require.Equal(t, 1, bytes.Count(runs, []byte("\n")), "replacement reran the completed task")
	require.Equal(t, 1, f.wire.operations(t, "apply"))
	f.stateMarker(t, command.ID)
}

func TestHAWorkerDrainDoesNotSuppressAuthorityFailure(t *testing.T) {
	f := newHAProcessFixture(t)
	first := f.start(t, "worker-0", true)
	f.control(t, first, true, "--wait", "rollout-drain")
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
	select {
	case err = <-first.done:
		first.exited = true
		require.Error(t, err, "authority loss during drain must still fail-stop")
	case <-time.After(15 * time.Second):
		t.Fatal("draining worker ignored lost storage authority")
	}
}

func (f *haProcessFixture) control(t *testing.T, worker *haWorkerProcess, success bool, args ...string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), f.binary, args...)
	command.Env = append(os.Environ(), "ROLLOUT_CONFIG_FILE="+filepath.Join(filepath.Dir(worker.log), "config.json"))
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if success {
		require.NoError(t, err, "local control: stdout=%s stderr=%s", stdout.String(), stderr.String())
		var status rollout.WorkerStatus
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &status), "local control stdout: %s", stdout.String())
		require.True(t, status.AuthorityAlive)
		require.NotEmpty(t, stderr.String(), "local control diagnostics must use stderr")
	} else {
		require.Error(t, err, "local control unexpectedly succeeded: stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
}

func (f *haProcessFixture) workerStatus(t *testing.T, worker *haWorkerProcess) rollout.WorkerStatus {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, worker.url+"/v1/worker/status", nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+f.token)
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var status rollout.WorkerStatus
	require.NoError(t, json.NewDecoder(response.Body).Decode(&status))
	return status
}

func (f *haProcessFixture) stopDrainedWorker(t *testing.T, worker *haWorkerProcess) {
	t.Helper()
	require.True(t, f.workerStatus(t, worker).Draining)
	require.Zero(t, f.workerStatus(t, worker).ActiveStages)
	haSignal(t, worker, syscall.SIGTERM)
	select {
	case err := <-worker.done:
		worker.exited = true
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("drained worker did not stop normally")
	}
}

func (f *haProcessFixture) waitEventsAcked(t *testing.T, id string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var pending int
		err := f.db.QueryRow(`SELECT count(*) FROM `+f.table("rollout_events")+` WHERE rollout=$1 AND acknowledged=0`, id).Scan(&pending)
		return err == nil && pending == 0
	}, 10*time.Second, 25*time.Millisecond, "draining worker stopped final encrypted delivery/Ack")
}

func (f *haProcessFixture) assertDrainMutationUnavailable(t *testing.T, worker *haWorkerProcess, id, subject string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"actor": "owned-ha-operator", "subject": subject})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, worker.url+"/v1/rollouts/"+id+"/approve", bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+f.token)
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
}

func (f *haProcessFixture) gateReceiptTask(t *testing.T) (*haApplyGate, string) {
	t.Helper()
	receipt := f.wire.addReceiptTask(t)
	directory := filepath.Join(f.root, "receipt-gate")
	require.NoError(t, os.Mkdir(directory, 0700))
	gate := &haApplyGate{directory: directory, root: f.root}
	body := fmt.Sprintf(`import json,sys,pathlib,os,time
r=json.load(sys.stdin)
p=pathlib.Path(%q)
if not p.exists(): p.write_text(r['effect_id'])
assert p.read_text()==r['effect_id']
g=pathlib.Path(%q)
with (g/'runs').open('a') as runs: runs.write(r['effect_id']+'\n')
(g/'pid').write_text(str(os.getpid()))
(g/'entered').touch()
while not (g/'release').exists(): time.sleep(0.02)
print(json.dumps({'version':1,'task_id':r['task_id'],'effect_id':r['effect_id'],'decision':'allow','category':'ok'}))`, receipt, directory)
	program := filepath.Join(directory, "task.py")
	require.NoError(t, os.WriteFile(program, []byte(body), 0600))
	tool := &f.wire.config.Executor.Tools[0]
	tool.Definition.Executable, tool.Definition.ExecutableSHA256 = program, transportDigest([]byte(body))
	f.config["tools"] = f.wire.config.Executor.Tools
	t.Cleanup(func() { gate.killOrphan(t) })
	return gate, receipt
}
