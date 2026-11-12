package runner

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

func (r *baseRunner) logDiskEstimate(ctx context.Context, data []byte) {
	if r.costEstimationDisabled {
		return
	}
	estimate, err := estimateDisks(data)
	status := "known"
	if estimate.Unpriced+estimate.Unknown > 0 {
		status = "partial"
	}
	if estimate.Priced+estimate.Free == 0 {
		status = "unknown"
	}
	attrs := []any{"framework", "pogi", "framework_version", "0.2.0", "currency", "USD", "estimate_for", "planned_resources", "catalogue_date", costCatalogueDate, "catalogue_source", "AWS Price List bulk files; Azure Retail Prices API", "catalogue_age_days", int(time.Since(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)).Hours() / 24), "resources", estimate.Resources, "priced_resources", estimate.Priced, "free_resources", estimate.Free, "unpriced_resources", estimate.Unpriced, "unknown_resources", estimate.Unknown}
	if err != nil {
		attrs = append(attrs, "status", "unknown", "reason", "invalid_plan_or_unavailable_catalogue")
		slog.WarnContext(ctx, "Monthly base disk estimate unavailable", slog.Group("cost_estimation", attrs...))
		return
	}
	message := "Monthly base disk estimate: unknown"
	if estimate.Priced > 0 || estimate.Resources > 0 && estimate.Free == estimate.Resources {
		amount := fmt.Sprintf("%.2f", estimate.Monthly)
		attrs = append(attrs, "known_monthly_subtotal", amount)
		message = "Monthly base disk estimate: USD " + amount + " (" + status + ")"
	}
	attrs = append(attrs, "status", status)
	slog.InfoContext(ctx, message, slog.Group("cost_estimation", attrs...))
}
