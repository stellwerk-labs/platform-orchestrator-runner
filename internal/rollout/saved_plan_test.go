package rollout

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"testing"

	"filippo.io/age"
	api "github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stretchr/testify/require"
)

const emptyPlanBase = "00000000-0000-0000-0000-000000000000"

func completedManualPlan(t *testing.T, tool string) (*fixture, string) {
	t.Helper()
	f, trace, _ := canonicalFixture(t, "plan", tool)
	f.command.PlanBase = &PlanBase{BaseDeploymentID: emptyPlanBase}
	_, err := f.engine.Submit(context.Background(), f.signed(t, f.command))
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "completed")
	return f, trace
}

func savedCommand(t *testing.T, f *fixture, source Command, id string) Command {
	t.Helper()
	p, err := f.engine.Get(context.Background(), source.ID)
	require.NoError(t, err)
	child := source
	child.ID, child.Request.ID, child.Request.Mode = id, id, "deploy"
	child.Strategy, child.PlanBase = "apply_saved", nil
	child.SourcePlan = &SourcePlan{SourcePlanID: source.ID, Subject: p.Subject, BaseDeploymentID: source.PlanBase.BaseDeploymentID}
	return child
}

func TestSavedPlanChildUsesExactArtifactOnce(t *testing.T) {
	for _, tool := range []string{"opentofu", "terraform"} {
		t.Run(tool, func(t *testing.T) { savedChild(t, tool) })
	}
}

func savedChild(t *testing.T, tool string) {
	t.Helper()
	f, trace := completedManualPlan(t, tool)
	ctx := context.Background()
	child := savedCommand(t, f, f.command, "saved-child")
	recipient, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	child.Request.EncryptedOutputsRecipient = recipient.Recipient().String()
	child.Request.DeploymentToken = "child-result-token"
	signed := f.signed(t, child)
	_, err = f.engine.Submit(ctx, signed)
	require.NoError(t, err)
	p := waitPhase(t, f.engine, child.ID, "completed")
	require.True(t, p.InfrastructureApplied)
	token, body, ready, err := f.engine.Completion(ctx, p)
	require.NoError(t, err)
	require.True(t, ready)
	require.Equal(t, "child-result-token", token)
	var result api.DeploymentResultsUpdateBody
	require.NoError(t, json.Unmarshal(body, &result))
	require.Len(t, result.Metadata, 1)
	require.NotNil(t, result.Outputs)
	cipher, err := base64.StdEncoding.DecodeString(*result.Outputs)
	require.NoError(t, err)
	reader, err := age.Decrypt(bytes.NewReader(cipher), recipient)
	require.NoError(t, err)
	plain, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Contains(t, string(plain), "PRIVATE_PIPELINE_VALUE")
	closeFixture(t, f.engine)
	f.engine, err = Open(ctx, f.config)
	require.NoError(t, err)
	// Recovery precedes expiry/catalog checks and source consumption checks.
	f.config.Executor.Capabilities = []string{"changed-after-completion"}
	_, err = f.engine.Submit(ctx, signed)
	require.NoError(t, err)
	f.config.Executor.Capabilities = nil
	_, err = f.engine.Submit(ctx, f.signed(t, savedCommand(t, f, f.command, "different-child")))
	requireCode(t, err, "saved_plan_already_used")
	require.Equal(t, 1, operationCount(t, trace, "plan"))
	require.Equal(t, 1, operationCount(t, trace, "apply"))
	logs, err := f.engine.GetExecutionLogs(ctx, child.ID, "")
	require.NoError(t, err)
	require.Contains(t, string(logs.Content), "apply_complete")
}

func TestSavedPlanFreshnessIgnoresPlansButRejectsLaterMutation(t *testing.T) {
	f, trace := completedManualPlan(t, "opentofu")
	ctx := context.Background()
	laterPlan := f.command
	laterPlan.ID, laterPlan.Request.ID = "later-plan", "later-plan"
	_, err := f.engine.Submit(ctx, f.signed(t, laterPlan))
	require.NoError(t, err)
	waitPhase(t, f.engine, laterPlan.ID, "completed")
	_, err = f.engine.db.ExecContext(ctx, `VACUUM`)
	require.NoError(t, err)
	child := savedCommand(t, f, f.command, "saved-child")
	_, err = f.engine.Submit(ctx, f.signed(t, child))
	require.NoError(t, err)
	waitPhase(t, f.engine, child.ID, "completed")
	_, err = f.engine.Submit(ctx, f.signed(t, savedCommand(t, f, laterPlan, "stale-child")))
	requireCode(t, err, "saved_plan_superseded")
	// A newer no-op direct deployment also supersedes a fresh Plan.
	laterPlan.ID, laterPlan.Request.ID = "current-plan", "current-plan"
	_, err = f.engine.Submit(ctx, f.signed(t, laterPlan))
	require.NoError(t, err)
	waitPhase(t, f.engine, laterPlan.ID, "completed")
	noOp := child
	noOp.ID, noOp.Request.ID, noOp.Strategy, noOp.SourcePlan = "no-op", "no-op", "apply", nil
	_, err = f.engine.Submit(ctx, f.signed(t, noOp))
	require.NoError(t, err)
	waitPhase(t, f.engine, noOp.ID, "completed")
	_, err = f.engine.Submit(ctx, f.signed(t, savedCommand(t, f, laterPlan, "stale-after-no-op")))
	requireCode(t, err, "saved_plan_superseded")
	require.Equal(t, 3, operationCount(t, trace, "plan"))
	require.Equal(t, 2, operationCount(t, trace, "apply"))
}

func TestSavedPlanAuthorityAndMaterialRefuseBeforeEffect(t *testing.T) {
	f, trace := completedManualPlan(t, "opentofu")
	ctx := context.Background()
	for _, test := range []struct {
		name string
		edit func(*Command)
		code string
	}{
		{"subject", func(c *Command) { c.SourcePlan.Subject = "wrong" }, "saved_plan_subject_conflict"},
		{"source", func(c *Command) { c.SourcePlan.SourcePlanID = "absent" }, "saved_plan_unavailable"},
		{"base", func(c *Command) { c.SourcePlan.BaseDeploymentID = "changed" }, "saved_plan_scope_conflict"},
		{"environment", func(c *Command) { c.EnvironmentID, c.Request.EnvironmentID = "other", "other" }, "saved_plan_scope_conflict"},
		{"backend", func(c *Command) { c.Request.Backend = "other" }, "saved_plan_material_conflict"},
		{"manifest", func(c *Command) { c.Request.Manifest = json.RawMessage(`{"different":true}`) }, "saved_plan_material_conflict"},
		{"closure", func(c *Command) {
			c.Request.Bundle = bytes.Clone(c.Request.Bundle)
			c.Request.Bundle[4] = 1
			c.Request.BundleSHA256 = digest(c.Request.Bundle)
		}, "saved_plan_material_conflict"},
		{"mode", func(c *Command) { c.Request.Mode = "plan_only" }, "strategy_refused"},
		{"legacy", func(c *Command) { c.Version = Version }, "approval_policy_refused"},
	} {
		t.Run(test.name, func(t *testing.T) {
			child := savedCommand(t, f, f.command, "refused-child")
			test.edit(&child)
			_, err := f.engine.Submit(ctx, f.signed(t, child))
			requireCode(t, err, test.code)
		})
	}
	_, err := f.engine.db.ExecContext(ctx, `UPDATE rollout_plan_logs SET content=? WHERE rollout=?`, []byte("corrupt"), f.command.ID)
	require.NoError(t, err)
	_, err = f.engine.Submit(ctx, f.signed(t, savedCommand(t, f, f.command, "corrupt-child")))
	requireCode(t, err, "saved_plan_unavailable")
	require.Equal(t, 0, operationCount(t, trace, "apply"))
}

func TestSavedPlanConcurrentChildrenConsumeOneSource(t *testing.T) {
	f, trace := completedManualPlan(t, "opentofu")
	ctx := context.Background()
	type imported struct {
		id  string
		err error
	}
	results := make(chan imported, 2)
	for _, id := range []string{"child-a", "child-b"} {
		child := savedCommand(t, f, f.command, id)
		signed := f.signed(t, child)
		go func() {
			_, err := f.engine.importCommand(ctx, child, signed.Payload)
			results <- imported{id: child.ID, err: err}
		}()
	}
	var winner string
	for range 2 {
		result := <-results
		if result.err == nil {
			require.Empty(t, winner)
			winner = result.id
		} else {
			requireCode(t, result.err, "saved_plan_already_used")
		}
	}
	require.NotEmpty(t, winner)
	require.NoError(t, f.engine.start(winner))
	waitPhase(t, f.engine, winner, "completed")
	var consumed int
	require.NoError(t, f.engine.db.QueryRowContext(ctx, `SELECT count(*) FROM rollout_plan_uses WHERE source_plan=?`, f.command.ID).Scan(&consumed))
	require.Equal(t, 1, consumed)
	require.Equal(t, 1, operationCount(t, trace, "apply"))
}

func TestSavedPlanActualApplyWithoutCheckpointRemainsUnknownOnRestart(t *testing.T) {
	f, trace := completedManualPlan(t, "opentofu")
	ctx := context.Background()
	child := savedCommand(t, f, f.command, "interrupted-child")
	signed := f.signed(t, child)
	_, err := f.engine.importCommand(ctx, child, signed.Payload)
	require.NoError(t, err)
	require.NoError(t, f.engine.phase(ctx, child.ID, "preparing"))
	material, err := f.engine.savedSource(ctx, f.engine.db, child)
	require.NoError(t, err)
	out, err := f.config.Executor.ApplySavedFrom(ctx, child.Request, material.Request, material.Ciphertext, material.Digest, func() error {
		_, claimErr := f.engine.claim(ctx, child.ID, "apply", true)
		return claimErr
	})
	require.NoError(t, err)
	require.True(t, out.Applied)
	_, err = os.Stat(f.state)
	require.NoError(t, err)
	// The real external effect finished, but its application checkpoint did not.
	closeFixture(t, f.engine)
	f.engine, err = Open(ctx, f.config)
	require.NoError(t, err)
	p := waitPhase(t, f.engine, child.ID, "outcome_unknown")
	require.Equal(t, "apply_outcome_unknown", p.ErrorCode)
	_, err = f.engine.Submit(ctx, signed)
	require.NoError(t, err)
	require.Equal(t, 1, operationCount(t, trace, "plan"))
	require.Equal(t, 1, operationCount(t, trace, "apply"))
	_, err = f.engine.Cancel(ctx, child.ID, "operator")
	requireCode(t, err, "effects_require_reconciliation")
	var holder string
	require.NoError(t, f.engine.db.QueryRowContext(ctx, `SELECT rollout FROM rollout_environments WHERE environment=?`, child.EnvironmentID).Scan(&holder))
	require.Equal(t, child.ID, holder)
}

func TestExecutionLogsDoNotMasqueradePlanAsApply(t *testing.T) {
	f, _, _ := canonicalFixture(t, "reviewed", "opentofu")
	ctx := context.Background()
	_, err := f.engine.Submit(ctx, f.signed(t, f.command))
	require.NoError(t, err)
	p := waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
	_, err = f.engine.GetExecutionLogs(ctx, p.ID, "")
	requireCode(t, err, "plan_logs_unavailable")
	_, err = f.engine.Decide(ctx, p.ID, p.Subject, "approve", "operator")
	require.NoError(t, err)
	waitPhase(t, f.engine, p.ID, "completed")
	logs, err := f.engine.GetExecutionLogs(ctx, p.ID, "")
	require.NoError(t, err)
	require.Contains(t, string(logs.Content), "apply_complete")
	require.NotContains(t, string(logs.Content), "=== Saved plan ===")
}

func TestUncapturedManualPlanCannotBePromoted(t *testing.T) {
	f, _, _ := canonicalFixture(t, "plan", "opentofu")
	ctx := context.Background()
	_, err := f.engine.Submit(ctx, f.signed(t, f.command))
	require.NoError(t, err)
	p := waitPhase(t, f.engine, f.command.ID, "completed")
	child := f.command
	child.ID, child.Request.ID, child.Request.Mode, child.Strategy = "legacy-source-child", "legacy-source-child", "deploy", "apply_saved"
	child.SourcePlan = &SourcePlan{SourcePlanID: f.command.ID, Subject: p.Subject, BaseDeploymentID: emptyPlanBase}
	_, err = f.engine.Submit(ctx, f.signed(t, child))
	requireCode(t, err, "saved_plan_unavailable")
}
