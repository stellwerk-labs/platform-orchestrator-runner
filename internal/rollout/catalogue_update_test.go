package rollout

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stretchr/testify/require"
)

func TestCatalogueMaintenanceRequiresExplicitSafeProgress(t *testing.T) {
	good := Progress{ID: "execution", EnvironmentID: "environment", Phase: "completed", Revision: 1, CreatedAt: time.Now(), Steps: []StepResult{}}
	for _, test := range []struct {
		name   string
		mutate func(*Progress)
		safe   bool
	}{
		{"completed_plan", func(*Progress) {}, true},
		{"completed_apply", func(p *Progress) { p.InfrastructureApplied = true }, true},
		{"failed_before_apply", func(p *Progress) { p.Phase = "failed"; p.ErrorCode = "plan_failed" }, true},
		{"denied", func(p *Progress) { p.Phase = "denied" }, true},
		{"cancelled", func(p *Progress) { p.Phase = "cancelled" }, true},
		{"active_plan_without_owner", func(p *Progress) { p.Phase = "planning" }, false},
		{"pending_approval", func(p *Progress) { p.Phase = "awaiting_approval" }, false},
		{"unknown_is_not_safe_terminal", func(p *Progress) { p.Phase = "outcome_unknown" }, false},
		{"pending_post_task", func(p *Progress) { p.Phase = "post_failed"; p.InfrastructureApplied = true }, false},
		{"failed_after_apply", func(p *Progress) { p.Phase = "failed"; p.InfrastructureApplied = true }, false},
		{"inconsistent_id", func(p *Progress) { p.ID = "other" }, false},
		{"inconsistent_environment", func(p *Progress) { p.EnvironmentID = "other" }, false},
		{"missing_revision", func(p *Progress) { p.Revision = 0 }, false},
		{"missing_creation", func(p *Progress) { p.CreatedAt = time.Time{} }, false},
		{"completed_with_failure", func(p *Progress) { p.FailedStep = "task" }, false},
		{"incomplete_post_task", func(p *Progress) { p.Steps = []StepResult{{ID: "task", Phase: "post_apply", Status: "failed"}} }, false},
		{"inconsistent_completion", func(p *Progress) { p.ErrorCode = "task_failed" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := good
			test.mutate(&p)
			encoded, err := json.Marshal(p)
			require.NoError(t, err)
			strategy := "plan"
			if p.InfrastructureApplied {
				strategy = "apply"
			}
			require.Equal(t, test.safe, catalogueSafeProgress(good.ID, good.EnvironmentID, strategy, encoded, p.InfrastructureApplied))
		})
	}
	for _, malformed := range []string{"", "null", "{}", "[]", `{"phase":"completed"}`, `{} {}`, `{"future_field":true}`} {
		require.False(t, catalogueSafeProgress(good.ID, good.EnvironmentID, "plan", []byte(malformed), false))
	}
}

func TestCatalogueMaintenanceRejectsInconsistentApplyEvidence(t *testing.T) {
	p := Progress{ID: "execution", EnvironmentID: "environment", Phase: "completed", Revision: 1, CreatedAt: time.Now()}
	data, err := json.Marshal(p)
	require.NoError(t, err)
	require.False(t, catalogueSafeProgress(p.ID, p.EnvironmentID, "apply", data, true))
	require.False(t, catalogueSafeProgress(p.ID, p.EnvironmentID, "apply", data, false), "a completed Apply needs settled Apply evidence")
	p.InfrastructureApplied = true
	data, err = json.Marshal(p)
	require.NoError(t, err)
	require.False(t, catalogueSafeProgress(p.ID, p.EnvironmentID, "apply", data, false))
}

func TestCatalogueMaintenanceRequiresExplicitNativeConfigurationBeforeStorageAccess(t *testing.T) {
	cfg := Config{OrgID: "org", RunnerID: "site", PublicKey: make([]byte, 32), Executor: &rolloutexec.Executor{}}
	requireCode(t, UpdatePostgresCatalogue(context.Background(), cfg, strings.Repeat("a", 64), "missing-admin"), "catalogue_update_configuration_refused")
}
