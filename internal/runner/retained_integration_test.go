//go:build integration

package runner

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"filippo.io/age"
	runnerconfig "github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stretchr/testify/require"
)

func TestRetainedPlanNativeApply(t *testing.T) {
	_, err := exec.LookPath("tofu")
	if err != nil {
		t.Skip("tofu unavailable")
	}
	ctx := context.Background()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	require.NoError(t, os.Mkdir(work, 0700))
	state := filepath.Join(root, "state.tfstate")
	config := `terraform {
 backend "local" {
 path = "` + state + `"
 }
}
resource "terraform_data" "example" { input = "approved" }
output "platform_orchestrator_metadata" { value = {} }
`
	require.NoError(t, os.WriteFile(filepath.Join(work, "main.tf"), []byte(config), 0600))
	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	binding := RetainedBinding{Schema: "stellwerk.retained-plan/v1", OrgID: "org", EnvironmentID: "env", RunnerID: "runner", SourceDeploymentID: "source", Backend: "fixture", Tool: "opentofu"}
	plan, err := NewLocalRunner(LocalOptions{Directory: work, Backend: runnerconfig.BackendOpenTofu, Identity: id})
	require.NoError(t, err)
	_, err = plan.Init(ctx)
	require.NoError(t, err)
	_, _, _, err = plan.Plan(ctx)
	require.NoError(t, err)
	encoded, err := plan.RetainPlan(ctx, binding)
	require.NoError(t, err)
	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(work))
	require.NoError(t, os.Mkdir(work, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(work, retainedFile), ciphertext, 0600))
	local, err := NewLocalRunner(LocalOptions{Directory: work, Backend: runnerconfig.BackendOpenTofu, Identity: id})
	require.NoError(t, err)
	apply := local.(*OpenTofuRunner)
	require.Error(t, apply.RestoreRetainedPlan(ctx, binding, "bad"))
	changedBinding := binding
	changedBinding.EnvironmentID = "other"
	require.Error(t, apply.RestoreRetainedPlan(ctx, changedBinding, digest(ciphertext)))
	require.NoError(t, apply.RestoreRetainedPlan(ctx, binding, digest(ciphertext)))
	_, changes, err := apply.Apply(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, changes.Added)
	stateBytes, err := os.ReadFile(state)
	require.NoError(t, err)
	require.Contains(t, string(stateBytes), "approved")
}
