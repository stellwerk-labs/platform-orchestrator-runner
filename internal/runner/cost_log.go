package runner

import (
	"context"
	"log/slog"
)

func (r *baseRunner) logCostEstimate(ctx context.Context, data []byte) {
	if !r.costEstimationEnabled {
		return
	}
	estimate, reason := r.estimateCosts(ctx, data)
	status := "estimated"
	if estimate.Unknown+estimate.Unpriced+estimate.MissingUsage > 0 {
		status = "partial"
	}
	if estimate.Monthly == "" {
		status = "unknown"
	}
	attrs := []any{"framework", "infracost", "framework_version", infracostVersion, "framework_build", infracostBuild, "currency", "USD", "estimate_for", "planned_resources", "pricing_source", "infracost_cloud_pricing_api", "catalogue_updated_at", "unknown", "upstream_defaults_possible", true, "resources", estimate.Resources, "priced_resources", estimate.Priced, "free_resources", estimate.Free, "unpriced_resources", estimate.Unpriced, "unknown_resources", estimate.Unknown, "missing_usage_resources", estimate.MissingUsage}
	if reason != "" {
		attrs = append(attrs, "status", "unknown", "reason", reason)
		slog.WarnContext(ctx, "Monthly infrastructure estimate unavailable", slog.Group("cost_estimation", attrs...))
		return
	}
	if estimate.Monthly != "" {
		attrs = append(attrs, "estimated_monthly_subtotal", estimate.Monthly)
	}
	attrs = append(attrs, "status", status)
	slog.InfoContext(ctx, "Monthly infrastructure estimate", slog.Group("cost_estimation", attrs...))
}
