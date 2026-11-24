//go:build integration

package rollout

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Terraform is a separately signed standalone Engine proof. The DP producer
// remains OpenTofu-only, and Core does not register the local state backend.
func TestPostgresStandaloneNativeTerraformSavedPlan(t *testing.T) {
	f, trace, _ := canonicalFixture(t, "plan", "terraform")
	openPostgresFixture(t, f)
	f.command.PlanBase = &PlanBase{BaseDeploymentID: emptyPlanBase}
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "completed")
	child := savedCommand(t, f, f.command, "terraform-child")
	_, err = f.engine.Submit(t.Context(), f.signed(t, child))
	require.NoError(t, err)
	waitPhase(t, f.engine, child.ID, "completed")
	again := savedCommand(t, f, f.command, "terraform-second-child")
	_, err = f.engine.Submit(t.Context(), f.signed(t, again))
	requireCode(t, err, "saved_plan_already_used")
	require.Equal(t, 1, operationCount(t, trace, "apply"))
}

func TestPostgresNativeSavedSourceKeepsOpaqueTargetDigest(t *testing.T) {
	f, trace, _ := postgresFixture(t, "plan")
	f.command.PlanBase = &PlanBase{BaseDeploymentID: emptyPlanBase}
	_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "completed")
	child := savedCommand(t, f, f.command, "changed-target-child")
	changed := *child.NativeTarget
	changed.StateConfigurationDigest = strings.Repeat("c", 64)
	child.NativeTarget = &changed
	_, err = f.engine.Submit(t.Context(), f.signed(t, child))
	requireCode(t, err, "saved_plan_material_conflict")
	var uses int
	require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_plan_uses`).Scan(&uses))
	require.Zero(t, uses)
	valid := savedCommand(t, f, f.command, "same-target-child")
	_, err = f.engine.Submit(t.Context(), f.signed(t, valid))
	require.NoError(t, err)
	waitPhase(t, f.engine, valid.ID, "completed")
	require.Equal(t, 1, operationCount(t, trace, "apply"))
}
