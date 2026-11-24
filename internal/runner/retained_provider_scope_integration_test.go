//go:build integration

package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stretchr/testify/require"
)

func TestProviderGuardIsLocalRunnerOnly(t *testing.T) {
	for _, tool := range []config.IaCBackendType{config.BackendOpenTofu, config.BackendTerraform} {
		t.Run(string(tool), func(t *testing.T) {
			cli := filepath.Join(t.TempDir(), "override.tfrc")
			require.NoError(t, os.WriteFile(cli, []byte(`provider_installation { dev_overrides { "hashicorp/local" = "/tmp/unused-provider" } }`), 0600))
			t.Setenv("TF_CLI_CONFIG_FILE", cli)
			legacyDir, localDir := privateRetainedDirectory(t), privateRetainedDirectory(t)
			for _, dir := range []string{legacyDir, localDir} {
				main := fmt.Sprintf("terraform {\n backend \"local\" { path = %q }\n}\nresource \"terraform_data\" \"marker\" { input = \"legacy-compatible\" }\n", filepath.Join(dir, "state"))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(main), 0600))
			}
			legacy, err := CreateRunner(t.Context(), &config.StandardModeConfiguration{
				Mode: config.PlanOnly, IaCBackend: tool, IaCCodeDir: legacyDir,
				OrgID: "org", RunnerID: "site", Gateway: config.GatewayClientConfiguration{URL: "https://localhost:1"},
			})
			require.NoError(t, err)
			_, err = legacy.Init(t.Context())
			require.NoError(t, err)
			_, _, _, err = legacy.Plan(t.Context())
			require.NoError(t, err, "legacy ordinary Plan behavior changed")
			identity, err := age.GenerateX25519Identity()
			require.NoError(t, err)
			local, err := NewLocalRunner(LocalOptions{Directory: localDir, Backend: tool, Identity: identity})
			require.NoError(t, err)
			_, err = local.Init(t.Context())
			require.NoError(t, err)
			_, _, _, err = local.Plan(t.Context())
			require.ErrorContains(t, err, "override", "native retained flow did not enforce its guard")
		})
	}
}
