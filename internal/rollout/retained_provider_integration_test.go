//go:build integration

package rollout

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCanonicalReviewedProviderClosure(t *testing.T) {
	for _, tool := range []string{"opentofu", "terraform"} {
		t.Run(tool, func(t *testing.T) {
			f, trace, managed, _ := reviewedProviderFixture(t, tool)
			_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
			require.NoError(t, err)
			waiting := waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
			_, err = os.Stat(managed)
			require.True(t, os.IsNotExist(err), "Plan created the external resource")
			require.Equal(t, 1, operationCount(t, trace, "plan"))
			require.Zero(t, operationCount(t, trace, "apply"))
			closeFixture(t, f.engine)
			f.engine, err = Open(t.Context(), f.config)
			require.NoError(t, err)
			_, err = f.engine.Decide(t.Context(), f.command.ID, waiting.Subject, "approve", "operator")
			require.NoError(t, err)
			waitPhase(t, f.engine, f.command.ID, "completed")
			content, err := os.ReadFile(managed)
			require.NoError(t, err)
			require.Equal(t, "reviewed-provider-value", string(content))
			require.Equal(t, 1, operationCount(t, trace, "plan"), "Apply regenerated the reviewed plan")
			require.Equal(t, 1, operationCount(t, trace, "apply"))
		})
	}
}

func TestCanonicalReviewedProviderRefusalHasNoEffectClaim(t *testing.T) {
	for _, tool := range []string{"opentofu", "terraform"} {
		for _, reason := range []string{"override", "checksum"} {
			t.Run(tool+"/"+reason, func(t *testing.T) {
				f, trace, managed, mirror := reviewedProviderFixture(t, tool)
				_, err := f.engine.Submit(t.Context(), f.signed(t, f.command))
				require.NoError(t, err)
				waiting := waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
				breakReviewedProvider(t, reason, mirror)
				_, err = f.engine.Decide(t.Context(), f.command.ID, waiting.Subject, "approve", "operator")
				require.NoError(t, err)
				failed := waitPhase(t, f.engine, f.command.ID, "failed")
				require.Equal(t, "saved_plan_unavailable", failed.ErrorCode)
				require.False(t, failed.InfrastructureApplied)
				var claims int
				require.NoError(t, f.engine.db.QueryRow(`SELECT count(*) FROM rollout_effects WHERE rollout=? AND step='apply'`, f.command.ID).Scan(&claims))
				require.Zero(t, claims, "provider refusal created an ambiguous Apply claim")
				require.Zero(t, operationCount(t, trace, "apply"))
				_, err = os.Stat(managed)
				require.True(t, os.IsNotExist(err))
			})
		}
	}
}

func breakReviewedProvider(t *testing.T, reason, mirror string) {
	t.Helper()
	if reason == "override" {
		safe := os.Getenv("TF_CLI_CONFIG_FILE")
		path := filepath.Join(t.TempDir(), "override.tfrc")
		require.NoError(t, os.WriteFile(path, []byte(`provider_installation { "dev_\u006fverrides" {} }`), 0600))
		t.Setenv("TF_CLI_CONFIG_FILE", "")
		t.Setenv("TOFU_CLI_CONFIG_FILE", safe)
		t.Setenv("TERRAFORM_CONFIG", path)
		return
	}
	paths, err := filepath.Glob(filepath.Join(mirror, "*", "hashicorp", "local", "*", runtime.GOOS+"_"+runtime.GOARCH, "terraform-provider-local"))
	require.NoError(t, err)
	require.NotEmpty(t, paths)
	for _, path := range paths {
		require.NoError(t, os.Remove(path))
		require.NoError(t, os.WriteFile(path, []byte("changed unapproved provider"), 0500))
	}
}

func reviewedProviderFixture(t *testing.T, tool string) (*fixture, string, string, string) {
	t.Helper()
	provider := os.Getenv("IAC_TEST_LOCAL_PROVIDER_BINARY")
	if provider == "" {
		t.Skip("set IAC_TEST_LOCAL_PROVIDER_BINARY for the offline retained-provider proof")
	}
	version := os.Getenv("IAC_TEST_LOCAL_PROVIDER_VERSION")
	if version == "" {
		version = "2.5.2"
	}
	mirror := installObservationProvider(t, provider, version)
	f, trace, _ := canonicalFixture(t, "reviewed", tool)
	managed := filepath.Join(t.TempDir(), "managed.txt")
	main := fmt.Sprintf(`terraform {
 backend "local" { path = %q }
 required_providers { local = { source = "hashicorp/local", version = %q } }
}
resource "local_file" "managed" {
 filename = %q
 content = "reviewed-provider-value"
}
output "platform_orchestrator_metadata" { value = {} }
`, f.state, version, managed)
	f.command.Request.Bundle = executionBundle(t, map[string]string{"main.tf": main})
	f.command.Request.BundleSHA256 = digest(f.command.Request.Bundle)
	return f, trace, managed, mirror
}
