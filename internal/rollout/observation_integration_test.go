//go:build integration

package rollout

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	api "github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stretchr/testify/require"
)

func TestNativeObservationRunsRealToolsWithoutApplyingOrRetaining(t *testing.T) {
	provider := os.Getenv("IAC_TEST_LOCAL_PROVIDER_BINARY")
	if provider == "" {
		t.Skip("set IAC_TEST_LOCAL_PROVIDER_BINARY to an installed hashicorp/local binary for offline execution")
	}
	version := os.Getenv("IAC_TEST_LOCAL_PROVIDER_VERSION")
	if version == "" {
		version = "2.5.2"
	}
	for _, tool := range []string{"opentofu", "terraform"} {
		t.Run(tool, func(t *testing.T) {
			installObservationProvider(t, provider, version)
			f, trace := observationFixture(t, tool)
			managed := filepath.Join(t.TempDir(), "managed.txt")
			main := fmt.Sprintf(`terraform {
 backend "local" { path = %q }
 required_providers { local = { source = "hashicorp/local", version = %q } }
}
resource "local_file" "managed" {
 filename = %q
 content = "SECRET_NATIVE_CONTENT"
}
output "secret" { value = "SECRET_NATIVE_OUTPUT" }
`, f.state, version, managed)
			prepareObservedState(t, tool, main)
			state, err := os.ReadFile(f.state)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(managed, []byte("EXTERNAL_NATIVE_SECRET"), 0600))
			f.command.Request.Bundle = executionBundle(t, map[string]string{"main.tf": main})
			f.command.Request.BundleSHA256 = digest(f.command.Request.Bundle)
			t.Setenv("TF_CLI_ARGS_plan", "-refresh=false -target=terraform_data.missing")
			t.Setenv("TF_DATA_DIR", filepath.Join(t.TempDir(), "wrong-data"))
			t.Setenv("TF_WORKSPACE", "wrong-workspace")
			_, err = f.engine.Submit(t.Context(), f.signed(t, f.command))
			require.NoError(t, err)
			completed := waitPhase(t, f.engine, f.command.ID, "completed")
			token, body, ready, err := f.engine.Completion(t.Context(), completed)
			require.NoError(t, err)
			require.True(t, ready)
			require.Equal(t, f.command.Request.DeploymentToken, token)
			var result api.DeploymentResultsUpdateBody
			require.NoError(t, json.Unmarshal(body, &result))
			require.Equal(t, api.Success, result.Status)
			require.NotNil(t, result.ObservationResult)
			require.NotEmpty(t, result.ObservationResult.ObservedChanges)
			require.NotEmpty(t, result.ObservationResult.ProposedChanges)
			require.Nil(t, result.Outputs)
			require.Empty(t, result.Metadata)
			require.False(t, completed.InfrastructureApplied)
			for _, private := range []string{"SECRET_NATIVE_CONTENT", "SECRET_NATIVE_OUTPUT", "EXTERNAL_NATIVE_SECRET", managed, "local_file.managed"} {
				require.NotContains(t, string(body), private)
			}
			after, err := os.ReadFile(f.state)
			require.NoError(t, err)
			require.Equal(t, state, after, "observation persisted refreshed state")
			actual, err := os.ReadFile(managed)
			require.NoError(t, err)
			require.Equal(t, "EXTERNAL_NATIVE_SECRET", string(actual), "observation applied correction")
			assertObservationArtifacts(t, f)
			closeFixture(t, f.engine)
			f.engine, err = Open(t.Context(), f.config)
			require.NoError(t, err)
			_, err = f.engine.Submit(t.Context(), f.signed(t, f.command))
			require.NoError(t, err)
			recovered, err := f.engine.Get(t.Context(), f.command.ID)
			require.NoError(t, err)
			_, replay, _, err := f.engine.Completion(t.Context(), recovered)
			require.NoError(t, err)
			require.JSONEq(t, string(body), string(replay))
			require.Equal(t, 1, operationCount(t, trace, "plan"))
			require.Equal(t, 1, operationCount(t, trace, "show"))
			require.Equal(t, 1, operationCount(t, trace, "apply"), "only baseline fixture setup may apply")
		})
	}
}

func installObservationProvider(t *testing.T, binary, version string) string {
	t.Helper()
	mirror := t.TempDir()
	for _, host := range []string{"registry.terraform.io", "registry.opentofu.org"} {
		dir := filepath.Join(mirror, host, "hashicorp", "local", version, runtime.GOOS+"_"+runtime.GOARCH)
		require.NoError(t, os.MkdirAll(dir, 0700))
		require.NoError(t, os.Symlink(binary, filepath.Join(dir, "terraform-provider-local")))
	}
	config := filepath.Join(t.TempDir(), "providers.tfrc")
	require.NoError(t, os.WriteFile(config, fmt.Appendf(nil, "provider_installation { filesystem_mirror { path = %q } }\n", mirror), 0600))
	t.Setenv("TF_CLI_CONFIG_FILE", config)
	return mirror
}

func prepareObservedState(t *testing.T, tool, main string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(main), 0600))
	program := "terraform"
	if tool == "opentofu" {
		program = "tofu"
	}
	for _, operation := range [][]string{{"init", "-input=false", "-no-color"}, {"apply", "-auto-approve", "-input=false", "-no-color"}} {
		cmd := exec.CommandContext(t.Context(), program, append([]string{"-chdir=" + dir}, operation...)...)
		for _, variable := range os.Environ() {
			if !strings.HasPrefix(variable, "TF_CLI_ARGS") {
				cmd.Env = append(cmd.Env, variable)
			}
		}
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
}

func assertObservationArtifacts(t *testing.T, f *fixture) {
	t.Helper()
	for _, table := range []string{"rollout_artifacts", "rollout_plan_logs", "rollout_execution_logs", "rollout_effects"} {
		var count int
		require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM `+table).Scan(&count))
		require.Zero(t, count, "observation created %s", table)
	}
	entries, err := os.ReadDir(f.config.Executor.Directory)
	require.NoError(t, err)
	require.Empty(t, entries, "private observation work directory retained")
}
