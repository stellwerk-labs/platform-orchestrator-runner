package runner

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCostComparisonAuthorityRejectsUncertainOrChangedEvidence(t *testing.T) {
	valid := CostComparison{Version: "stellwerk.cost/v1", Currency: "USD", Status: "complete", BeforeMonthly: "10.000000000000000001", AfterMonthly: "10", DeltaMonthly: "-0.000000000000000001"}
	require.True(t, valid.NonIncreasing(), "comparison must retain precision beyond floating point")
	for _, change := range []struct {
		name string
		edit func(*CostComparison)
	}{
		{"increase", func(c *CostComparison) { c.BeforeMonthly, c.AfterMonthly, c.DeltaMonthly = "1", "2", "1" }},
		{"partial", func(c *CostComparison) { c.Status = "partial" }},
		{"unknown", func(c *CostComparison) { c.BeforeCoverage.Unknown = 1 }},
		{"usage", func(c *CostComparison) { c.AfterCoverage.MissingUsage = 1 }},
		{"currency", func(c *CostComparison) { c.Currency = "EUR" }},
		{"missing", func(c *CostComparison) { c.BeforeMonthly = "" }},
		{"negative", func(c *CostComparison) { c.BeforeMonthly = "-1" }},
		{"forged delta", func(c *CostComparison) { c.DeltaMonthly = "0" }},
		{"reason", func(c *CostComparison) { c.Reason = "estimator_unavailable" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			candidate := valid
			change.edit(&candidate)
			require.False(t, candidate.NonIncreasing())
		})
	}
}

func TestCostComparisonRequiresBothSnapshotsAndKnownInputs(t *testing.T) {
	plan := []byte(`{"planned_values":{"root_module":{"resources":[]}},"prior_state":{"values":{"root_module":{"resources":[]}}},"resource_changes":[]}`)
	before, after, reason := comparisonViews(plan)
	require.Empty(t, reason)
	require.JSONEq(t, string(before), string(after))
	var document map[string]any
	require.NoError(t, json.Unmarshal(plan, &document))
	delete(document, "prior_state")
	data, err := json.Marshal(document)
	require.NoError(t, err)
	_, _, reason = comparisonViews(data)
	require.Equal(t, "cost_baseline_unavailable", reason)
	require.NoError(t, json.Unmarshal(plan, &document))
	document["resource_changes"] = []any{map[string]any{"change": map[string]any{"after_unknown": map[string]any{"size": true}}}}
	data, err = json.Marshal(document)
	require.NoError(t, err)
	_, _, reason = comparisonViews(data)
	require.Equal(t, "unknown_plan_values", reason)
}

func TestCostComparisonUsageDefaultsCannotAuthorize(t *testing.T) {
	output := []byte(`{"version":"0.2","currency":"USD","totalMonthlyCost":"10","summary":{"totalSupportedResources":1},"projects":[{"breakdown":{"resources":[{"costComponents":[{"usageBased":true,"monthlyQuantity":"100","monthlyCost":"10"}]}]}}]}`)
	estimate, reason := parseCostOutput(output, costEstimate{Resources: 1})
	require.Empty(t, reason)
	require.False(t, coverage(estimate).complete(), "a CLI default quantity is not explicit usage evidence")
}
