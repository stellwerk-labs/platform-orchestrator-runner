package rollout

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
	"github.com/stretchr/testify/require"
)

func publishedFixture(t *testing.T, strategy, tool string) (*fixture, string) {
	t.Helper()
	f, trace, _ := canonicalFixture(t, strategy, tool)
	f.command.Recipe = &RecipeReference{ID: "published-recipe", Version: 1, Digest: strings.Repeat("a", 64)}
	f.config.Executor.Capabilities = []string{RecipeCapability}
	f.command.RequiredCapabilities = []string{RecipeCapability}
	return f, trace
}

func TestPublishedManualPlanPreservesChecksApprovalAndPostTasks(t *testing.T) {
	for _, tool := range []string{"opentofu", "terraform"} {
		t.Run(tool, func(t *testing.T) {
			f, trace := publishedFixture(t, "plan", tool)
			f.command.PlanBase = &PlanBase{BaseDeploymentID: emptyPlanBase}
			f.command.Approval = Approval{Mode: "local", PolicyID: f.command.Recipe.ID, PolicyVersion: "1"}
			checkTrace, postTrace := filepath.Join(t.TempDir(), "checks"), filepath.Join(t.TempDir(), "post")
			addTask(t, f, "plan_check", "read_only", traceTask(checkTrace))
			checks, checkSteps := f.config.Executor.Tools, f.command.Steps
			addTask(t, f, "post_apply", "receipt", traceTask(postTrace))
			f.config.Executor.Tools[0].ID = "post-task"
			f.command.Steps[0].ID, f.command.Steps[0].ToolID = "post", "post-task"
			f.config.Executor.Tools = append(checks, f.config.Executor.Tools...)
			f.command.Steps = append(checkSteps, f.command.Steps...)
			f.command.ToolsDigest = f.config.Executor.CatalogDigest()
			ctx := context.Background()
			_, err := f.engine.Submit(ctx, f.signed(t, f.command))
			require.NoError(t, err)
			source := waitPhase(t, f.engine, f.command.ID, "completed")
			require.NotNil(t, source.PlanChanges)
			require.Nil(t, source.ApplyChanges)
			require.Len(t, source.Steps, 1)
			require.Equal(t, "allow", source.Steps[0].Outcome.Verdict)
			require.Equal(t, "1", source.Steps[0].Outcome.ToolVersion)
			_, err = os.Stat(postTrace)
			require.True(t, os.IsNotExist(err), "Plan must not perform a post-Apply effect")
			child := savedCommand(t, f, f.command, "recipe-child")
			_, err = f.engine.Submit(ctx, f.signed(t, child))
			require.NoError(t, err)
			pending := waitPhase(t, f.engine, child.ID, "awaiting_approval")
			require.NotEqual(t, source.Subject, pending.Subject, "new decision binds the child and inherited exact evidence")
			_, err = f.engine.Decide(ctx, child.ID, pending.Subject, "approve", "operator")
			require.NoError(t, err)
			completed := waitPhase(t, f.engine, child.ID, "completed")
			require.Equal(t, "human", completed.ApprovalDecision.Source)
			require.Equal(t, "operator", completed.ApprovalDecision.ActorID)
			require.Equal(t, pending.Subject, completed.ApprovalDecision.Subject)
			require.NotNil(t, completed.PlanChanges)
			require.NotNil(t, completed.ApplyChanges)
			require.Equal(t, 1, completed.PlanChanges.Added)
			require.Equal(t, 1, completed.ApplyChanges.Added)
			require.Equal(t, 1, operationCount(t, trace, "plan"))
			require.Equal(t, 1, operationCount(t, trace, "apply"))
			checkBody, err := os.ReadFile(checkTrace)
			require.NoError(t, err)
			require.Equal(t, "x", string(checkBody), "completed Plan checks must not repeat in child")
			postBody, err := os.ReadFile(postTrace)
			require.NoError(t, err)
			require.Equal(t, "x", string(postBody))
		})
	}
}

func traceTask(path string) string {
	return fmt.Sprintf(`import json,sys,pathlib
r=json.load(sys.stdin)
p=pathlib.Path(%q)
with p.open('a') as f: f.write('x')
print(json.dumps({'version':1,'task_id':r['task_id'],'effect_id':r['effect_id'],'decision':'allow','category':'ok'}))
`, path)
}

func TestPublishedSourceCannotDropItsApprovalOrRecipe(t *testing.T) {
	f, _ := publishedFixture(t, "plan", "opentofu")
	f.command.PlanBase = &PlanBase{BaseDeploymentID: emptyPlanBase}
	f.command.Approval = Approval{Mode: "local", PolicyID: f.command.Recipe.ID, PolicyVersion: "1"}
	f.command.ToolsDigest = f.config.Executor.CatalogDigest()
	_, err := f.engine.Submit(context.Background(), f.signed(t, f.command))
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "completed")
	for _, test := range []struct {
		name string
		edit func(*Command)
	}{
		{"approval", func(c *Command) { c.Approval = Approval{} }},
		{"recipe", func(c *Command) { c.Recipe = nil; c.Approval = Approval{} }},
		{"digest", func(c *Command) { copy := *c.Recipe; copy.Digest = strings.Repeat("b", 64); c.Recipe = &copy }},
	} {
		t.Run(test.name, func(t *testing.T) {
			child := savedCommand(t, f, f.command, "changed-"+test.name)
			test.edit(&child)
			_, err := f.engine.Submit(context.Background(), f.signed(t, child))
			requireCode(t, err, "saved_plan_material_conflict")
		})
	}
}

func TestNativeCostUncertaintyRequiresHumanAndPersistsEvidence(t *testing.T) {
	f, trace := publishedFixture(t, "reviewed", "opentofu")
	f.config.Executor.Cost = &runner.CostConfiguration{}
	f.config.Executor.Capabilities = append(f.config.Executor.Capabilities, rolloutexec.CostCapability, rolloutexec.NetworkPricingCapability)
	f.command.RequiredCapabilities = append(f.command.RequiredCapabilities, rolloutexec.CostCapability, rolloutexec.NetworkPricingCapability)
	f.command.Cost = &CostPolicy{Enabled: true}
	f.command.Approval = Approval{Mode: "cost_non_increasing", PolicyID: f.command.Recipe.ID, PolicyVersion: "1"}
	f.command.ToolsDigest = f.config.Executor.CatalogDigest()
	ctx := context.Background()
	_, err := f.engine.Submit(ctx, f.signed(t, f.command))
	require.NoError(t, err)
	p := waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
	require.NotNil(t, p.Cost)
	require.Equal(t, "unknown", p.Cost.Status)
	require.False(t, p.Cost.NonIncreasing())
	require.Nil(t, p.ApprovalDecision)
	require.Equal(t, 0, operationCount(t, trace, "apply"))
	var ciphertext []byte
	var evidence string
	require.NoError(t, f.engine.db.QueryRow(`SELECT content,digest FROM rollout_cost_artifacts WHERE rollout=?`, p.ID).Scan(&ciphertext, &evidence))
	require.NotContains(t, string(ciphertext), "infracost")
	require.Len(t, evidence, 64)
	// Replacing a valid encrypted cost result cannot change an existing subject.
	changed := *p.Cost
	changed.Reason = "changed_after_review"
	body, err := json.Marshal(changed)
	require.NoError(t, err)
	replacement, err := f.engine.encrypt(body)
	require.NoError(t, err)
	_, err = f.engine.db.Exec(`UPDATE rollout_cost_artifacts SET content=?,digest=? WHERE rollout=?`, replacement, digest(body), p.ID)
	require.NoError(t, err)
	_, err = f.engine.Decide(ctx, p.ID, p.Subject, "approve", "operator")
	requireCode(t, err, "invalid_cost_evidence")
	_, err = f.engine.db.Exec(`UPDATE rollout_cost_artifacts SET content=?,digest=? WHERE rollout=?`, ciphertext, evidence, p.ID)
	require.NoError(t, err)
	closeFixture(t, f.engine)
	f.engine, err = Open(ctx, f.config)
	require.NoError(t, err)
	_, err = f.engine.Decide(ctx, p.ID, p.Subject, "approve", "operator")
	require.NoError(t, err)
	completed := waitPhase(t, f.engine, p.ID, "completed")
	require.Equal(t, "human", completed.ApprovalDecision.Source)
	require.Equal(t, evidence, completed.ApprovalDecision.EvidenceDigest)
	require.Equal(t, 1, operationCount(t, trace, "plan"))
	require.Equal(t, 1, operationCount(t, trace, "apply"))
}

// This is a SQLite approval-authority test with explicit cost evidence, not a
// pricing or IaC integration test. Real CLI comparisons are tested independently.
func TestAutomaticCostDecisionIsAtomicIdempotentAndSubjectBound(t *testing.T) {
	f, _ := publishedFixture(t, "reviewed", "opentofu")
	f.config.Executor.Cost = &runner.CostConfiguration{}
	f.config.Executor.Capabilities = append(f.config.Executor.Capabilities, rolloutexec.CostCapability, rolloutexec.NetworkPricingCapability)
	f.command.RequiredCapabilities = append(f.command.RequiredCapabilities, rolloutexec.CostCapability, rolloutexec.NetworkPricingCapability)
	f.command.Cost = &CostPolicy{Enabled: true}
	f.command.Approval = Approval{Mode: "cost_non_increasing", PolicyID: f.command.Recipe.ID, PolicyVersion: "1"}
	f.command.ToolsDigest = f.config.Executor.CatalogDigest()
	ctx := context.Background()
	signed := f.signed(t, f.command)
	_, err := f.engine.importCommand(ctx, f.command, signed.Payload)
	require.NoError(t, err)
	cost := &runner.CostComparison{Version: "stellwerk.cost/v1", Estimator: "infracost", Currency: "USD", Status: "complete", BeforeMonthly: "10", AfterMonthly: "9", DeltaMonthly: "-1", BeforeCoverage: runner.CostCoverage{Resources: 1, Priced: 1}, AfterCoverage: runner.CostCoverage{Resources: 1, Priced: 1}}
	plan := rolloutexec.Plan{Ciphertext: []byte("opaque-approval-unit-artifact"), Review: []byte(`{}`), Changes: runner.IaCChanges{}, Cost: cost}
	require.NoError(t, f.engine.savePlan(ctx, f.command, plan))
	require.NoError(t, f.engine.approvalReady(ctx, f.command.ID))
	approved, err := f.engine.Get(ctx, f.command.ID)
	require.NoError(t, err)
	require.Equal(t, "approved", approved.Phase)
	require.Equal(t, "policy", approved.ApprovalDecision.Source)
	require.Equal(t, f.command.Recipe.ID, approved.ApprovalDecision.PolicyID)
	require.Empty(t, approved.ApprovalDecision.ActorID)
	require.Equal(t, approved.Subject, approved.ApprovalDecision.Subject)
	require.Len(t, approved.ApprovalDecision.EvidenceDigest, 64)
	require.NoError(t, f.engine.approvalReady(ctx, f.command.ID))
	replayed, err := f.engine.Get(ctx, f.command.ID)
	require.NoError(t, err)
	require.Equal(t, approved.ApprovalDecision, replayed.ApprovalDecision)
	var count int
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_approval_audit WHERE rollout=?`, approved.ID).Scan(&count))
	require.Equal(t, 1, count)
}
