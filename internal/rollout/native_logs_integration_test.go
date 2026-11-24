//go:build integration

package rollout

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stretchr/testify/require"
)

// Register retained command authority without starting DBOS. The tests call the
// real executor/persistence boundaries explicitly to control the crash window.
func storeNativeLogCommand(t *testing.T, f *fixture, command Command) {
	t.Helper()
	payload := f.signed(t, command).Payload
	encrypted, err := f.engine.encrypt(payload)
	require.NoError(t, err)
	tx, err := f.engine.beginCommand(t.Context(), command.ID)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(t.Context(), `INSERT INTO rollout_commands(id,digest,payload,progress,version,environment,strategy) VALUES($1,$2,$3,$4,$5,$6,$7)`, command.ID, digest(payload), encrypted, []byte(`{}`), command.Version, command.EnvironmentID, command.Strategy)
	require.NoError(t, err)
	p := Progress{ID: command.ID, EnvironmentID: command.EnvironmentID, Phase: "preparing", CreatedAt: time.Now().UTC(), Steps: []StepResult{}}
	require.NoError(t, consumeSourcePlan(t.Context(), tx, command))
	require.NoError(t, saveProgress(t.Context(), tx, &p))
	require.NoError(t, tx.Commit())
}

func rejectNativeLogJournal(t *testing.T, f *fixture) func() {
	t.Helper()
	_, err := f.engine.db.Exec(`CREATE FUNCTION reject_native_log_journal() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'owned capture journal fault'; END; $$;
 CREATE TRIGGER reject_native_log_journal BEFORE INSERT ON rollout_deliveries FOR EACH ROW EXECUTE FUNCTION reject_native_log_journal()`)
	require.NoError(t, err)
	return func() {
		_, err := f.engine.db.Exec(`DROP TRIGGER reject_native_log_journal ON rollout_deliveries; DROP FUNCTION reject_native_log_journal()`)
		require.NoError(t, err)
	}
}

func nativeCaptureDelivery(t *testing.T, f *fixture, id string, revision int64) EventDelivery {
	t.Helper()
	var eventID int64
	require.NoError(t, f.engine.db.QueryRow(`SELECT id FROM rollout_events WHERE rollout=$1 AND revision=$2`, id, revision).Scan(&eventID))
	delivery, err := f.engine.PrepareDelivery(t.Context(), eventID)
	require.NoError(t, err)
	return delivery
}

func TestPostgresNativeLogCaptureRollbackRecaptureAndRestart(t *testing.T) {
	f, trace, _ := postgresFixture(t, "plan")
	managed, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	f.command.Request.EncryptedLogsRecipient = managed.Recipient().String()
	storeNativeLogCommand(t, f, f.command)
	require.NoError(t, f.engine.saveFailedPlanLogs(t.Context(), f.command.ID, rolloutexec.Plan{Logs: []byte("first failed diagnostic")}))
	first, err := f.engine.Get(t.Context(), f.command.ID)
	require.NoError(t, err)
	require.NotNil(t, first.LogCapture)
	frozen := nativeCaptureDelivery(t, f, first.ID, first.LogCapture.Authority.CaptureRevision)
	require.NotNil(t, frozen.Log)
	// The frozen row must exist before any later publisher asks for this event.
	var before []byte
	require.NoError(t, f.engine.db.QueryRow(`SELECT content FROM rollout_deliveries WHERE event_id=$1`, frozen.EventID).Scan(&before))
	restore := rejectNativeLogJournal(t, f)
	require.Error(t, f.engine.saveFailedPlanLogs(t.Context(), f.command.ID, rolloutexec.Plan{Logs: []byte("rolled back diagnostic")}))
	restore()
	current, err := f.engine.Get(t.Context(), first.ID)
	require.NoError(t, err)
	require.Equal(t, first.Revision, current.Revision)
	captured, err := f.engine.GetPlanLogs(t.Context(), first.ID, "")
	require.NoError(t, err)
	require.Equal(t, "first failed diagnostic", string(captured.Content))
	reopenPostgres(t, f)
	// Recover by actually executing Plan; its new subject/revision cannot retarget
	// the previously frozen failed capture.
	require.NoError(t, f.engine.plan(t.Context(), first.ID))
	recovered, err := f.engine.Get(t.Context(), first.ID)
	require.NoError(t, err)
	require.Greater(t, recovered.LogCapture.Authority.CaptureRevision, first.LogCapture.Authority.CaptureRevision)
	require.Equal(t, first.LogCapture.Authority.ExecutionID, recovered.LogCapture.Authority.ExecutionID)
	require.Equal(t, 1, operationCount(t, trace, "plan"))
	reopenPostgres(t, f)
	replay := nativeCaptureDelivery(t, f, first.ID, first.LogCapture.Authority.CaptureRevision)
	require.Equal(t, frozen, replay)
	decoded, err := nativelogs.Decode(replay.Log.Ciphertext, managed, replay.Log.Descriptor.Authority)
	require.NoError(t, err)
	require.Equal(t, "first failed diagnostic", string(decoded.Logs))
	var after []byte
	require.NoError(t, f.engine.db.QueryRow(`SELECT content FROM rollout_deliveries WHERE event_id=$1`, frozen.EventID).Scan(&after))
	require.Equal(t, before, after)
}

func TestPostgresNativeSavedChildRewrapsInheritedPlan(t *testing.T) {
	f, trace := publishedFixture(t, "plan", "opentofu")
	f.command.PlanBase = &PlanBase{BaseDeploymentID: emptyPlanBase}
	f.command.Approval = Approval{Mode: "local", PolicyID: f.command.Recipe.ID, PolicyVersion: "1"}
	f.command.ToolsDigest = f.config.Executor.CatalogDigest()
	openPostgresFixture(t, f)
	sourceKey, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	childKey, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	f.command.Request.EncryptedLogsRecipient = sourceKey.Recipient().String()
	_, err = f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	source := waitPhase(t, f.engine, f.command.ID, "completed")
	raw, err := f.engine.GetPlanLogs(t.Context(), source.ID, source.Subject)
	require.NoError(t, err)
	child := savedCommand(t, f, f.command, "native-log-child")
	child.Request.EncryptedLogsRecipient = childKey.Recipient().String()
	_, err = f.engine.Submit(t.Context(), f.signed(t, child))
	require.NoError(t, err)
	pending := waitPhase(t, f.engine, child.ID, "awaiting_approval")
	inherited := nativeCaptureDelivery(t, f, child.ID, pending.LogCapture.Authority.CaptureRevision)
	require.NotNil(t, inherited.Log)
	authority := inherited.Log.Descriptor.Authority
	require.True(t, authority.Inherited)
	require.Equal(t, source.ID, authority.ExecutionID)
	require.Equal(t, source.ID, authority.SourcePlanID)
	require.Equal(t, source.Subject, authority.SourceSubject)
	require.Equal(t, pending.Subject, authority.Subject)
	require.NotEqual(t, source.Subject, authority.Subject)
	envelope, err := nativelogs.Decode(inherited.Log.Ciphertext, childKey, authority)
	require.NoError(t, err)
	require.Equal(t, raw.Content, envelope.Logs)
	_, err = nativelogs.Decode(inherited.Log.Ciphertext, sourceKey, authority)
	require.Error(t, err)
	_, err = f.engine.Decide(t.Context(), child.ID, pending.Subject, "approve", "log-reviewer")
	require.NoError(t, err)
	waitPhase(t, f.engine, child.ID, "completed")
	require.Equal(t, 1, operationCount(t, trace, "plan"))
	require.Equal(t, 1, operationCount(t, trace, "apply"))
}

func nativeApplyLogFixture(t *testing.T, strategy, tool, status string) (*fixture, string, Command) {
	t.Helper()
	program := tool
	if tool == "opentofu" {
		program = "tofu"
	}
	_, err := exec.LookPath(program)
	require.NoError(t, err, "real installed IaC tool is required")
	f, trace, _ := canonicalFixture(t, strategy, tool)
	if strategy == "saved" {
		f.command.Strategy, f.command.Request.Mode = "plan", "plan_only"
		f.command.PlanBase = &PlanBase{BaseDeploymentID: emptyPlanBase}
	}
	if status == nativelogs.TooLarge {
		main := fmt.Sprintf(`terraform {
 backend "local" {
  path = %q
 }
}
resource "terraform_data" "marker" {
 provisioner "local-exec" { command = "python3 -c 'import sys; sys.stdout.write((\"L\"*1024+\"\\n\")*5000)'" }
}
output "platform_orchestrator_metadata" { value = { node = { installed = true } } }
`, f.state)
		f.command.Request.Bundle = executionBundle(t, map[string]string{"main.tf": main})
		f.command.Request.BundleSHA256 = digest(f.command.Request.Bundle)
	}
	openPostgresFixture(t, f)
	f.command.Request.EncryptedLogsRecipient = "invalid-export-recipient"
	if status == nativelogs.TooLarge {
		key, err := age.GenerateX25519Identity()
		require.NoError(t, err)
		f.command.Request.EncryptedLogsRecipient = key.Recipient().String()
	}
	storeNativeLogCommand(t, f, f.command)
	command := f.command
	if strategy == "reviewed" {
		require.NoError(t, f.engine.plan(t.Context(), command.ID))
		require.NoError(t, f.engine.phase(t.Context(), command.ID, "approved"))
	}
	if strategy == "saved" {
		require.NoError(t, f.engine.plan(t.Context(), command.ID))
		require.NoError(t, f.engine.completePipeline(t.Context(), command.ID))
		command = savedCommand(t, f, f.command, "native-log-apply-child")
		storeNativeLogCommand(t, f, command)
	}
	return f, trace, command
}

func TestPostgresNativeActualApplyExportFailureKeepsOutcome(t *testing.T) {
	for _, tool := range []string{"opentofu", "terraform"} {
		for _, strategy := range []string{"apply", "reviewed", "saved"} {
			for _, status := range []string{nativelogs.Unavailable, nativelogs.TooLarge} {
				t.Run(strings.Join([]string{tool, strategy, status}, "/"), func(t *testing.T) {
					f, trace, command := nativeApplyLogFixture(t, strategy, tool, status)
					var err error
					switch strategy {
					case "apply":
						err = f.engine.directApply(t.Context(), command.ID)
					case "reviewed":
						err = f.engine.reviewedApply(t.Context(), command.ID, command)
					case "saved":
						err = f.engine.savedApply(t.Context(), command.ID)
					}
					require.NoError(t, err)
					p, err := f.engine.Get(t.Context(), command.ID)
					require.NoError(t, err)
					require.True(t, p.InfrastructureApplied)
					require.Equal(t, "applied", p.Phase)
					require.Equal(t, status, p.LogCapture.Status)
					frozen := nativeCaptureDelivery(t, f, command.ID, p.LogCapture.Authority.CaptureRevision)
					require.Nil(t, frozen.Log)
					var effect string
					require.NoError(t, f.engine.db.QueryRow(`SELECT status FROM rollout_effects WHERE rollout=$1 AND step='apply'`, command.ID).Scan(&effect))
					require.Equal(t, "done", effect)
					_, err = f.engine.GetExecutionLogs(t.Context(), command.ID, "")
					if status == nativelogs.TooLarge {
						requireCode(t, err, "plan_logs_too_large")
					} else {
						require.NoError(t, err)
					}
					reopenPostgres(t, f)
					switch strategy {
					case "apply":
						err = f.engine.directApply(t.Context(), command.ID)
					case "reviewed":
						err = f.engine.reviewedApply(t.Context(), command.ID, command)
					case "saved":
						err = f.engine.savedApply(t.Context(), command.ID)
					}
					require.NoError(t, err)
					require.Equal(t, 1, operationCount(t, trace, "apply"))
				})
			}
		}
	}
}

func TestPostgresNativeActualApplyJournalFailurePreservesFence(t *testing.T) {
	for _, strategy := range []string{"apply", "reviewed", "saved"} {
		t.Run(strategy, func(t *testing.T) {
			f, trace, command := nativeApplyLogFixture(t, strategy, "opentofu", nativelogs.Unavailable)
			restore := rejectNativeLogJournal(t, f)
			var err error
			switch strategy {
			case "apply":
				err = f.engine.directApply(t.Context(), command.ID)
			case "reviewed":
				err = f.engine.reviewedApply(t.Context(), command.ID, command)
			case "saved":
				err = f.engine.savedApply(t.Context(), command.ID)
			}
			requireCode(t, err, "apply_outcome_unknown")
			restore()
			var results, logs, blocks int
			require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_pipeline_results WHERE rollout=$1`, command.ID).Scan(&results))
			require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_execution_logs WHERE rollout=$1`, command.ID).Scan(&logs))
			require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_no_effect_blocks WHERE rollout=$1`, command.ID).Scan(&blocks))
			require.Zero(t, results)
			require.Zero(t, logs)
			require.Equal(t, 1, blocks)
			p, err := f.engine.Get(t.Context(), command.ID)
			require.NoError(t, err)
			require.False(t, p.InfrastructureApplied)
			require.Equal(t, "applying", p.Phase)
			reopenPostgres(t, f)
			switch strategy {
			case "apply":
				err = f.engine.directApply(t.Context(), command.ID)
			case "reviewed":
				err = f.engine.reviewedApply(t.Context(), command.ID, command)
			case "saved":
				err = f.engine.savedApply(t.Context(), command.ID)
			}
			requireCode(t, err, "apply_outcome_unknown")
			require.Equal(t, 1, operationCount(t, trace, "apply"))
		})
	}
}

func TestPostgresNativeAbsentRecipientKeepsOldRawLogsAndSignedBytes(t *testing.T) {
	f, trace, _ := postgresFixture(t, "plan")
	f.command.PlanBase = &PlanBase{BaseDeploymentID: emptyPlanBase}
	original := f.signed(t, f.command).Payload
	require.NotContains(t, string(original), "encrypted_logs_recipient")
	storeNativeLogCommand(t, f, f.command)
	require.NoError(t, f.engine.plan(t.Context(), f.command.ID))
	require.NoError(t, f.engine.completePipeline(t.Context(), f.command.ID))
	p, err := f.engine.Get(t.Context(), f.command.ID)
	require.NoError(t, err)
	require.Equal(t, nativelogs.NotManaged, p.LogCapture.Status)
	raw, err := f.engine.GetPlanLogs(t.Context(), p.ID, p.Subject)
	require.NoError(t, err)
	require.NotEmpty(t, raw.Content)
	frozen := nativeCaptureDelivery(t, f, p.ID, p.LogCapture.Authority.CaptureRevision)
	require.Nil(t, frozen.Log)
	// Restore exactly the historical format-7 raw ciphertext representation. New
	// readers recover its original approval bytes without modifying its command.
	historical, err := f.engine.encrypt(raw.Content)
	require.NoError(t, err)
	_, err = f.engine.db.Exec(`UPDATE rollout_plan_logs SET content=$1 WHERE rollout=$2`, historical, p.ID)
	require.NoError(t, err)
	reopenPostgres(t, f)
	replay, err := f.engine.GetPlanLogs(t.Context(), p.ID, p.Subject)
	require.NoError(t, err)
	require.Equal(t, raw.Content, replay.Content)
	child := savedCommand(t, f, f.command, "old-source-child")
	storeNativeLogCommand(t, f, child)
	require.NoError(t, f.engine.savedApply(t.Context(), child.ID))
	require.Equal(t, 1, operationCount(t, trace, "plan"))
	require.Equal(t, 1, operationCount(t, trace, "apply"))
	var stored []byte
	require.NoError(t, f.engine.db.QueryRow(`SELECT payload FROM rollout_commands WHERE id=$1`, p.ID).Scan(&stored))
	plain, err := f.engine.decrypt(stored)
	require.NoError(t, err)
	require.Equal(t, []byte(original), plain)
}

func TestPostgresNativeStartedApplyAndJournalFailureRemainUnknown(t *testing.T) {
	f, trace, _ := postgresFixture(t, "apply")
	main := fmt.Sprintf(`terraform {
 backend "local" { path = %q }
}
resource "terraform_data" "partial" {
 provisioner "local-exec" { command = "printf 'actual started failure'; exit 1" }
}
`, f.state)
	f.command.Request.Bundle = executionBundle(t, map[string]string{"main.tf": main})
	f.command.Request.BundleSHA256 = digest(f.command.Request.Bundle)
	f.command.Request.EncryptedLogsRecipient = "invalid-export-recipient"
	storeNativeLogCommand(t, f, f.command)
	restore := rejectNativeLogJournal(t, f)
	err := f.engine.directApply(t.Context(), f.command.ID)
	requireCode(t, err, "apply_outcome_unknown")
	require.ErrorContains(t, err, "storage_unavailable")
	restore()
	reopenPostgres(t, f)
	err = f.engine.directApply(t.Context(), f.command.ID)
	requireCode(t, err, "apply_outcome_unknown")
	require.Equal(t, 1, operationCount(t, trace, "apply"))
}
