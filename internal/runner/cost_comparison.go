package runner

import (
	"context"
	"encoding/json"
	"math/big"
	"strings"
	"time"
)

// CostConfiguration is operator-local execution configuration, never command data.
type CostConfiguration struct {
	PricingAccess string
	APIKey        string
	Binary        string
	Endpoint      string
}

type CostCoverage struct {
	Resources    int `json:"resources"`
	Priced       int `json:"priced"`
	Free         int `json:"free"`
	Unknown      int `json:"unknown"`
	Unpriced     int `json:"unpriced"`
	MissingUsage int `json:"missing_usage"`
}

// CostComparison contains only bounded aggregates suitable for the review UI.
type CostComparison struct {
	Version        string       `json:"version"`
	Estimator      string       `json:"estimator"`
	Build          string       `json:"build"`
	Currency       string       `json:"currency"`
	Status         string       `json:"status"`
	BeforeMonthly  string       `json:"before_monthly,omitempty"`
	AfterMonthly   string       `json:"after_monthly,omitempty"`
	DeltaMonthly   string       `json:"delta_monthly,omitempty"`
	BeforeCoverage CostCoverage `json:"before_coverage"`
	AfterCoverage  CostCoverage `json:"after_coverage"`
	Reason         string       `json:"reason,omitempty"`
	GeneratedAt    time.Time    `json:"generated_at"`
}

// ComparePlanCosts prices two sanitized views of one immutable Plan snapshot.
// It deliberately does not compare to a historical estimate or parse tool logs.
func ComparePlanCosts(ctx context.Context, configuration CostConfiguration, plan []byte) CostComparison {
	result := CostComparison{Version: "stellwerk.cost/v1", Estimator: "infracost", Build: infracostBuild,
		Currency: "USD", Status: "unknown", GeneratedAt: time.Now().UTC()}
	before, after, reason := comparisonViews(plan)
	if reason != "" {
		result.Reason = reason
		return result
	}
	estimator := &baseRunner{infracostAPIKey: configuration.APIKey, costBinary: configuration.Binary, costEndpoint: configuration.Endpoint}
	prior, priorReason := estimator.estimateCosts(ctx, before)
	planned, plannedReason := estimator.estimateCosts(ctx, after)
	result.BeforeCoverage, result.AfterCoverage = coverage(prior), coverage(planned)
	result.BeforeMonthly, result.AfterMonthly = prior.Monthly, planned.Monthly
	if priorReason != "" || plannedReason != "" {
		result.Reason = priorReason
		if result.Reason == "" {
			result.Reason = plannedReason
		}
		return result
	}
	// A validated empty resource view is an exact zero, unlike an absent view.
	if prior.Resources == 0 {
		result.BeforeMonthly = "0"
	}
	if planned.Resources == 0 {
		result.AfterMonthly = "0"
	}
	if !result.BeforeCoverage.complete() || !result.AfterCoverage.complete() || result.BeforeMonthly == "" || result.AfterMonthly == "" {
		result.Status, result.Reason = "partial", "incomplete_cost_coverage"
		return result
	}
	result.DeltaMonthly = costDifference(result.BeforeMonthly, result.AfterMonthly)
	result.Status = "complete"
	return result
}

// NonIncreasing authorizes only complete comparable evidence using exact decimals.
func (c CostComparison) NonIncreasing() bool {
	if c.Version != "stellwerk.cost/v1" || c.Status != "complete" || c.Currency != "USD" || c.Reason != "" || !c.BeforeCoverage.complete() || !c.AfterCoverage.complete() {
		return false
	}
	if _, ok := costMonthlyAmount(&c.BeforeMonthly); !ok {
		return false
	}
	if _, ok := costMonthlyAmount(&c.AfterMonthly); !ok {
		return false
	}
	delta := costDifference(c.BeforeMonthly, c.AfterMonthly)
	amount, ok := new(big.Rat).SetString(delta)
	return ok && delta == c.DeltaMonthly && amount.Sign() <= 0
}

func costDifference(before, after string) string {
	left, lok := new(big.Rat).SetString(before)
	right, rok := new(big.Rat).SetString(after)
	if !lok || !rok {
		return ""
	}
	value := new(big.Rat).Sub(right, left).FloatString(64)
	return strings.TrimRight(strings.TrimRight(value, "0"), ".")
}

func coverage(estimate costEstimate) CostCoverage {
	// No usage file is accepted by this contract. Upstream usage defaults are
	// estimates, not explicit comparable usage evidence for automatic authority.
	estimate.MissingUsage = max(estimate.MissingUsage, estimate.UsageBased)
	return CostCoverage{Resources: estimate.Resources, Priced: estimate.Priced, Free: estimate.Free,
		Unknown: estimate.Unknown, Unpriced: estimate.Unpriced, MissingUsage: estimate.MissingUsage}
}

func (c CostCoverage) complete() bool {
	return c.Resources >= 0 && c.Priced >= 0 && c.Free >= 0 && c.Priced+c.Free == c.Resources && c.Unknown == 0 && c.Unpriced == 0 && c.MissingUsage == 0
}

func comparisonViews(data []byte) ([]byte, []byte, string) {
	if len(data) == 0 || len(data) > maxCostPlanBytes {
		return nil, nil, "plan_limit"
	}
	var plan map[string]any
	if decodeCostJSON(data, &plan) != nil {
		return nil, nil, "invalid_plan"
	}
	// This first iteration is deliberately conservative: unknown computed fields
	// cannot silently become zero/default pricing inputs.
	changes, _ := plan["resource_changes"].([]any)
	for _, item := range changes {
		resource, ok := item.(map[string]any)
		if !ok {
			return nil, nil, "invalid_plan"
		}
		change, ok := resource["change"].(map[string]any)
		if !ok || costMaskPresent(change["after_unknown"]) {
			return nil, nil, "unknown_plan_values"
		}
	}
	prior, ok := plan["prior_state"].(map[string]any)
	if !ok {
		return nil, nil, "cost_baseline_unavailable"
	}
	values, ok := prior["values"].(map[string]any)
	if !ok {
		return nil, nil, "cost_baseline_unavailable"
	}
	if _, ok := values["root_module"].(map[string]any); !ok {
		return nil, nil, "cost_baseline_unavailable"
	}
	after, _ := json.Marshal(plan)
	plan["planned_values"] = values
	before, _ := json.Marshal(plan)
	return before, after, ""
}
