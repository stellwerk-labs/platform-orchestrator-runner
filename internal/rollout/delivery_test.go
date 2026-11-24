package rollout

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stellwerk-labs/golib/hmessaging"
	api "github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stretchr/testify/require"
)

func TestEventDeliveryFreezesPrivateResultAcrossProcessRestart(t *testing.T) {
	f, trace, _ := canonicalFixture(t, "apply", "opentofu")
	main := fmt.Sprintf(`terraform {
 backend "local" { path = %q }
}
resource "terraform_data" "marker" { input = "PRIVATE_PIPELINE_VALUE" }
output "value" { value = terraform_data.marker.output }
output "platform_orchestrator_metadata" { value = { node = { label = "PRIVATE_FROZEN_METADATA_CANARY" } } }
`, f.state)
	f.command.Request.Bundle = executionBundle(t, map[string]string{"main.tf": main})
	f.command.Request.BundleSHA256 = digest(f.command.Request.Bundle)
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	completed := waitPhase(t, f.engine, f.command.ID, "completed")
	events, err := f.engine.Events(t.Context(), 0)
	require.NoError(t, err)
	last := events[len(events)-1]
	require.Equal(t, completed.Revision, last.Progress.Revision)
	closeFixture(t, f.engine)
	request := deliveryProcessRequest{Directory: f.config.Directory, Org: f.config.OrgID, Site: f.config.RunnerID, Public: f.config.PublicKey, Event: last.ID}
	path, output := filepath.Join(t.TempDir(), "request.json"), filepath.Join(t.TempDir(), "wire.json")
	data, err := json.Marshal(request)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0600))
	executable, err := os.Executable()
	require.NoError(t, err)
	var frozen EventDelivery
	for attempt := 0; attempt < 2; attempt++ {
		child := exec.CommandContext(t.Context(), executable, "-test.run=^TestEventDeliveryProcessFixture$")
		child.Env = append(os.Environ(), "DELIVERY_PROCESS_REQUEST="+path, "DELIVERY_PROCESS_OUTPUT="+output, fmt.Sprintf("DELIVERY_PROCESS_KILL=%d", attempt))
		log, err := child.CombinedOutput()
		if attempt == 0 {
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit)
			require.Equal(t, syscall.SIGKILL, exit.Sys().(syscall.WaitStatus).Signal())
		} else {
			require.NoError(t, err, "%s", log)
		}
		data, err = os.ReadFile(output)
		require.NoError(t, err)
		var replay EventDelivery
		require.NoError(t, json.Unmarshal(data, &replay))
		if attempt == 0 {
			frozen = replay
			require.FileExists(t, filepath.Join(f.config.Directory, "rollouts.sqlite-wal"))
			assertCommandDatabasePrivacy(t, f.config.Directory, []string{"PRIVATE_FROZEN_METADATA_CANARY", "PRIVATE_PIPELINE_VALUE"})
		} else {
			require.Equal(t, frozen.Progress, replay.Progress)
			require.Equal(t, frozen.Result, replay.Result)
		}
		require.NoError(t, os.Remove(output))
	}
	var progress, result hmessaging.EventEnvelope
	require.NoError(t, json.Unmarshal(frozen.Progress, &progress))
	require.NoError(t, json.Unmarshal(frozen.Result, &result))
	require.Equal(t, f.command.ID+":deployment-result", result.EventID)
	require.Equal(t, progress.CreatedAt, result.CreatedAt)
	require.Contains(t, string(result.Payload), "PRIVATE_FROZEN_METADATA_CANARY")
	require.Equal(t, 1, operationCount(t, trace, "apply"))
}

type deliveryProcessRequest struct {
	Directory, Org, Site string
	Public               []byte
	Event                int64
}

// The parent test invokes this driver in a separate process against its owned
// terminal fixture, never an operational site directory.
func TestEventDeliveryProcessFixture(t *testing.T) {
	path := os.Getenv("DELIVERY_PROCESS_REQUEST")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var request deliveryProcessRequest
	require.NoError(t, json.Unmarshal(data, &request))
	engine, err := Open(t.Context(), Config{Directory: request.Directory, OrgID: request.Org, RunnerID: request.Site, PublicKey: request.Public, Executor: &rolloutexec.Executor{}})
	require.NoError(t, err)
	defer closeFixture(t, engine)
	delivery, err := engine.PrepareDelivery(t.Context(), request.Event)
	require.NoError(t, err)
	data, err = json.Marshal(delivery)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(os.Getenv("DELIVERY_PROCESS_OUTPUT"), data, 0600))
	if os.Getenv("DELIVERY_PROCESS_KILL") == "0" {
		require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGKILL))
	}
}

func TestFrozenResultSurvivesNewerRevisionWithoutAnotherResultIdentity(t *testing.T) {
	f, _, _ := canonicalFixture(t, "reviewed", "opentofu")
	signed := f.signed(t, f.command)
	_, err := f.engine.importCommand(t.Context(), f.command, signed.Payload)
	require.NoError(t, err)
	_, err = f.engine.Cancel(t.Context(), f.command.ID, "operator")
	require.NoError(t, err)
	events, err := f.engine.Events(t.Context(), 0)
	require.NoError(t, err)
	last := events[len(events)-1]
	frozen, err := f.engine.PrepareDelivery(t.Context(), last.ID)
	require.NoError(t, err)
	require.NotEmpty(t, frozen.Result)
	_, err = f.engine.update(t.Context(), f.command.ID, func(p *Progress) error {
		p.ErrorCode = "later-diagnostic"
		return nil
	})
	require.NoError(t, err)
	replay, err := f.engine.PrepareDelivery(t.Context(), last.ID)
	require.NoError(t, err)
	require.Equal(t, frozen, replay)
	events, err = f.engine.Events(t.Context(), last.ID)
	require.NoError(t, err)
	newer, err := f.engine.PrepareDelivery(t.Context(), events[0].ID)
	require.NoError(t, err)
	require.NotEmpty(t, newer.Progress)
	require.Empty(t, newer.Result, "later diagnostics reused a result ID with different bytes")
}

func TestEventAcknowledgementDoesNotCoverEarlierRows(t *testing.T) {
	f, _, _ := canonicalFixture(t, "reviewed", "opentofu")
	signed := f.signed(t, f.command)
	_, err := f.engine.importCommand(t.Context(), f.command, signed.Payload)
	require.NoError(t, err)
	_, err = f.engine.Cancel(t.Context(), f.command.ID, "operator")
	require.NoError(t, err)
	events, err := f.engine.Events(t.Context(), 0)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.NoError(t, f.engine.AckEvent(t.Context(), events[1].ID))
	pending, err := f.engine.Events(t.Context(), 0)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, events[0].ID, pending[0].ID)
	requireCode(t, f.engine.AckEvent(t.Context(), 999999), "event_unavailable")
}

func TestPostTaskRepairFreezesItsFirstSuccessfulResult(t *testing.T) {
	f, trace, _ := canonicalFixture(t, "apply", "opentofu")
	receipt := filepath.Join(t.TempDir(), "post-task-receipt")
	addTask(t, f, "post_apply", "receipt", fmt.Sprintf(`import json,sys,pathlib
r=json.load(sys.stdin)
p=pathlib.Path(%q)
if not p.exists():
 p.write_text(r['effect_id'])
 sys.exit(1)
assert p.read_text()==r['effect_id']
print(json.dumps({'version':1,'task_id':r['task_id'],'effect_id':r['effect_id'],'decision':'allow','category':'ok'}))`, receipt))
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	failed := waitPhase(t, f.engine, f.command.ID, "post_failed")
	events, err := f.engine.Events(t.Context(), 0)
	require.NoError(t, err)
	for _, event := range events {
		delivery, err := f.engine.PrepareDelivery(t.Context(), event.ID)
		require.NoError(t, err)
		require.Empty(t, delivery.Result, "failed post tasks prematurely finalized the ordinary Deployment")
	}
	closeFixture(t, f.engine)
	f.engine, err = Open(t.Context(), f.config)
	require.NoError(t, err)
	_, err = f.engine.Repair(t.Context(), failed.ID, failed.FailedStep, "operator")
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "completed")
	events, err = f.engine.Events(t.Context(), events[len(events)-1].ID)
	require.NoError(t, err)
	var results int
	for _, event := range events {
		delivery, err := f.engine.PrepareDelivery(t.Context(), event.ID)
		require.NoError(t, err)
		if len(delivery.Result) == 0 {
			continue
		}
		results++
		var envelope hmessaging.EventEnvelope
		require.NoError(t, json.Unmarshal(delivery.Result, &envelope))
		var result api.DeploymentResultsUpdateBody
		require.NoError(t, json.Unmarshal(envelope.Payload, &result))
		require.Equal(t, api.Success, result.Status)
		require.Nil(t, result.NoEffectReceipt)
	}
	require.Equal(t, 1, results)
	require.Equal(t, 1, operationCount(t, trace, "apply"))
	data, err := os.ReadFile(receipt)
	require.NoError(t, err)
	require.NotEmpty(t, bytes.TrimSpace(data))
}

func TestEventDeliveryRefusesMisassociatedOrCorruptCiphertext(t *testing.T) {
	f, _, _ := canonicalFixture(t, "reviewed", "opentofu")
	signed := f.signed(t, f.command)
	_, err := f.engine.importCommand(t.Context(), f.command, signed.Payload)
	require.NoError(t, err)
	_, err = f.engine.Cancel(t.Context(), f.command.ID, "operator")
	require.NoError(t, err)
	events, err := f.engine.Events(t.Context(), 0)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(events), 2)
	for _, event := range events {
		_, err = f.engine.PrepareDelivery(t.Context(), event.ID)
		require.NoError(t, err)
	}
	first, last := events[0].ID, events[len(events)-1].ID
	_, err = f.engine.db.Exec(`UPDATE rollout_deliveries SET content=(SELECT content FROM rollout_deliveries WHERE event_id=?) WHERE event_id=?`, first, last)
	require.NoError(t, err)
	_, err = f.engine.PrepareDelivery(t.Context(), last)
	requireCode(t, err, "delivery_unavailable")
	_, err = f.engine.db.Exec(`UPDATE rollout_deliveries SET content=? WHERE event_id=?`, []byte("corrupt-private-content"), first)
	require.NoError(t, err)
	_, err = f.engine.PrepareDelivery(t.Context(), first)
	requireCode(t, err, "delivery_unavailable")
	pending, err := f.engine.Events(t.Context(), 0)
	require.NoError(t, err)
	require.Len(t, pending, len(events))
}
