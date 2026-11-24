//go:build integration

package rollout

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"

	"github.com/lib/pq"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
	"github.com/stretchr/testify/require"
)

func TestNativeCostUnpricedResourceRequiresHumanWithPinnedCLI(t *testing.T) {
	binary := os.Getenv("INFRACOST_TEST_BINARY")
	if binary == "" {
		binary = "/opt/runner/infracost"
	}
	require.FileExists(t, binary)
	f, _ := publishedFixture(t, "apply", "opentofu")
	f.command.ToolsDigest = f.config.Executor.CatalogDigest()
	ctx := context.Background()
	_, err := f.engine.Submit(ctx, f.signed(t, f.command))
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "completed")
	// The builtin resource has known values, but the pinned estimator must decide
	// coverage. Its unsupported resource is never assumed to have zero cost.
	pricing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		require.NoError(t, err)
		var queries []json.RawMessage
		require.NoError(t, json.Unmarshal(body, &queries))
		response := make([]json.RawMessage, len(queries))
		for i := range response {
			response[i] = json.RawMessage(`{"data":{"products":[{"prices":[{"priceHash":"fixture","USD":"0.1"}]}]}}`)
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer pricing.Close()
	f.config.Executor.Cost = &runner.CostConfiguration{APIKey: "OWNED_FIXTURE_KEY", Binary: binary, Endpoint: pricing.URL, PricingAccess: "local"}
	f.config.Executor.Capabilities = append(f.config.Executor.Capabilities, rolloutexec.CostCapability, rolloutexec.LocalPricingCapability)
	command := f.command
	command.ID, command.Request.ID, command.Strategy = "known-free-review", "known-free-review", "reviewed"
	command.Cost = &CostPolicy{Enabled: true}
	command.Approval = Approval{Mode: "cost_non_increasing", PolicyID: command.Recipe.ID, PolicyVersion: "1"}
	command.RequiredCapabilities = append(command.RequiredCapabilities, rolloutexec.CostCapability, rolloutexec.LocalPricingCapability)
	command.ToolsDigest = f.config.Executor.CatalogDigest()
	_, err = f.engine.Submit(ctx, f.signed(t, command))
	require.NoError(t, err)
	pending := waitPhase(t, f.engine, command.ID, "awaiting_approval")
	require.Equal(t, "partial", pending.Cost.Status, "%+v", pending.Cost)
	require.Greater(t, pending.Cost.AfterCoverage.Unpriced, 0)
	require.Nil(t, pending.ApprovalDecision)
	var claims int
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_effects WHERE rollout=? AND step='apply'`, command.ID).Scan(&claims))
	require.Zero(t, claims, "incomplete costs cannot claim Apply before human review")
	_, err = f.engine.Decide(ctx, pending.ID, pending.Subject, "approve", "operator")
	require.NoError(t, err)
	completed := waitPhase(t, f.engine, command.ID, "completed")
	require.Equal(t, "human", completed.ApprovalDecision.Source)
	require.Equal(t, "operator", completed.ApprovalDecision.ActorID)
	require.Equal(t, command.Recipe.ID, completed.ApprovalDecision.PolicyID)
	require.Equal(t, command.Approval.PolicyVersion, completed.ApprovalDecision.PolicyVersion)
	require.Equal(t, completed.Subject, completed.ApprovalDecision.Subject)
	require.Len(t, completed.ApprovalDecision.EvidenceDigest, 64)
	require.Equal(t, 0, completed.PlanChanges.Added)
	require.Equal(t, 0, completed.ApplyChanges.Added)
	closeFixture(t, f.engine)
	f.engine, err = Open(ctx, f.config)
	require.NoError(t, err)
	_, err = f.engine.Submit(ctx, f.signed(t, command))
	require.NoError(t, err)
	var decisions int
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_approval_audit WHERE rollout=?`, command.ID).Scan(&decisions))
	require.Equal(t, 1, decisions)
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_effects WHERE rollout=? AND step='apply' AND status='done'`, command.ID).Scan(&claims))
	require.Equal(t, 1, claims)
}

// Exercise actual pricing and PostgreSQL approval persistence without enqueuing
// the synthetic review material as an executable infrastructure workflow.
func TestPostgresActualCostComparisonPersistsApprovalWithoutWorkflow(t *testing.T) {
	_, err := exec.LookPath("tofu")
	require.NoError(t, err, "the existing Native fixture requires actual OpenTofu")
	binary := os.Getenv("INFRACOST_TEST_BINARY")
	require.FileExists(t, binary, "the actual pinned source-built CLI is required")
	pricing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var queries []json.RawMessage
		if r.URL.Path != "/graphql" || r.Header.Get("X-Api-Key") != "OWNED_FIXTURE_KEY" || json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&queries) != nil {
			http.Error(w, "invalid owned pricing request", http.StatusBadRequest)
			return
		}
		response := make([]json.RawMessage, len(queries))
		for i := range response {
			response[i] = json.RawMessage(`{"data":{"products":[{"prices":[{"priceHash":"fixture","USD":"0.1"}]}]}}`)
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer pricing.Close()
	for _, test := range []struct {
		name, delta string
		after       int
		automatic   bool
	}{
		{"increase", "10", 200, false},
		{"flat", "0", 100, true},
		{"decrease", "-5", 50, true},
		{"unknown", "", 100, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, trace := publishedFixture(t, "reviewed", "opentofu")
			configuration := runner.CostConfiguration{APIKey: "OWNED_FIXTURE_KEY", Binary: binary, Endpoint: pricing.URL, PricingAccess: "local"}
			f.config.Executor.Cost = &configuration
			f.config.Executor.Capabilities = append(f.config.Executor.Capabilities, rolloutexec.CostCapability, rolloutexec.LocalPricingCapability)
			f.command.RequiredCapabilities = append(f.command.RequiredCapabilities, rolloutexec.CostCapability, rolloutexec.LocalPricingCapability)
			f.command.Cost = &CostPolicy{Enabled: true, PolicyRevision: 1}
			f.command.Approval = Approval{Mode: "cost_non_increasing", PolicyID: f.command.Recipe.ID, PolicyVersion: "1"}
			admin := openPostgresFixture(t, f)
			f.command.ToolsDigest, f.command.NativeTarget.ToolsDigest = f.engine.ToolsDigest(), f.engine.ToolsDigest()
			command, err := f.engine.verify(f.signed(t, f.command))
			require.NoError(t, err)
			require.NoError(t, f.engine.validate(command))
			storeNativeLogCommand(t, f, command)
			input, err := os.ReadFile("testdata/cost-comparison-plan.json")
			require.NoError(t, err)
			var document map[string]any
			require.NoError(t, json.Unmarshal(input, &document))
			resource := document["planned_values"].(map[string]any)["root_module"].(map[string]any)["resources"].([]any)[0].(map[string]any)
			resource["values"].(map[string]any)["size"] = test.after
			if test.name == "unknown" {
				document["resource_changes"] = []any{map[string]any{"change": map[string]any{"after_unknown": map[string]any{"size": true}}}}
			}
			input, err = json.Marshal(document)
			require.NoError(t, err)
			cost := runner.ComparePlanCosts(t.Context(), configuration, input)
			require.Equal(t, test.delta, cost.DeltaMonthly)
			if test.name == "unknown" {
				require.Equal(t, "unknown_plan_values", cost.Reason)
			} else {
				require.Equal(t, "complete", cost.Status)
				require.Equal(t, runner.CostCoverage{Resources: 1, Priced: 1}, cost.BeforeCoverage)
				require.Equal(t, cost.BeforeCoverage, cost.AfterCoverage)
			}
			require.NoError(t, f.engine.savePlan(t.Context(), command, rolloutexec.Plan{Ciphertext: []byte("opaque-policy-test-artifact"), Review: input, Logs: []byte("policy integration review"), Cost: &cost}))
			require.NoError(t, f.engine.approvalReady(t.Context(), command.ID))
			first, err := f.engine.Get(t.Context(), command.ID)
			require.NoError(t, err)
			var ciphertext []byte
			var evidence string
			require.NoError(t, f.engine.db.QueryRow(`SELECT content,digest FROM rollout_cost_artifacts WHERE rollout=$1`, command.ID).Scan(&ciphertext, &evidence))
			plain, err := json.Marshal(cost)
			require.NoError(t, err)
			require.Equal(t, digest(plain), evidence)
			require.NotContains(t, string(ciphertext), string(plain))
			for _, reopen := range []bool{false, true} {
				if reopen {
					reopenPostgres(t, f)
				}
				require.NoError(t, f.engine.approvalReady(t.Context(), command.ID))
				current, err := f.engine.Get(t.Context(), command.ID)
				require.NoError(t, err)
				require.Equal(t, first.Subject, current.Subject)
				require.Equal(t, first.ApprovalDecision, current.ApprovalDecision)
				require.Equal(t, &cost, current.Cost)
				persisted, retainedDigest, err := f.engine.costEvidence(t.Context(), f.engine.db, command.ID)
				require.NoError(t, err)
				require.Equal(t, &cost, persisted)
				require.Equal(t, evidence, retainedDigest)
				var audits, effects, workflows int
				require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_approval_audit WHERE rollout=$1`, command.ID).Scan(&audits))
				if test.automatic {
					require.Equal(t, "approved", current.Phase)
					require.Equal(t, 1, audits)
					require.NotNil(t, current.ApprovalDecision)
					require.Equal(t, "policy", current.ApprovalDecision.Source)
					require.Empty(t, current.ApprovalDecision.ActorID)
					require.Equal(t, command.Approval.PolicyID, current.ApprovalDecision.PolicyID)
					require.Equal(t, command.Approval.PolicyVersion, current.ApprovalDecision.PolicyVersion)
					require.Equal(t, current.Subject, current.ApprovalDecision.Subject)
					require.Equal(t, evidence, current.ApprovalDecision.EvidenceDigest)
				} else {
					require.Equal(t, "awaiting_approval", current.Phase)
					require.Zero(t, audits)
					require.Nil(t, current.ApprovalDecision)
				}
				require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_effects WHERE rollout=$1`, command.ID).Scan(&effects))
				require.NoError(t, admin.QueryRow(`SELECT count(*) FROM `+pq.QuoteIdentifier(f.config.Postgres.SystemSchema)+`.workflow_status WHERE workflow_uuid=$1`, command.ID).Scan(&workflows))
				require.Zero(t, effects)
				require.Zero(t, workflows, "queued and running workflows both require a workflow_status row")
				require.NoFileExists(t, trace, "the policy persistence test must not execute IaC")
			}
		})
	}
}
