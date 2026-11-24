package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalPlanProviderPreparationDiagnosticIsSafe(t *testing.T) {
	const privateValue = "PRIVATE_PROVIDER_VALUE_CANARY"
	const privatePath = "PRIVATE_PROVIDER_PATH_CANARY"
	for _, tool := range []string{"terraform", "opentofu"} {
		for _, test := range []struct {
			name, variable, configuration, reason string
		}{
			{name: "missing_home", reason: "is not defined"},
			{name: "environment_override", variable: "TF_REATTACH_PROVIDERS", reason: "retained provider override unsupported"},
			{name: "configuration_override", configuration: `provider_installation { dev_overrides { "hashicorp/local" = "` + privateValue + `" } }`, reason: "retained provider override unsupported"},
		} {
			t.Run(tool+"/"+test.name, func(t *testing.T) {
				for _, name := range []string{"HOME", "USERPROFILE", "home", "TF_CLI_CONFIG_FILE", "TERRAFORM_CONFIG", "TF_REATTACH_PROVIDERS", "TOFU_REATTACH_PROVIDERS", "TF_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE", "TOFU_PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE"} {
					t.Setenv(name, "")
				}
				directory := filepath.Join(t.TempDir(), privatePath)
				require.NoError(t, os.Mkdir(directory, 0700))
				if test.variable != "" {
					t.Setenv(test.variable, privateValue)
				}
				if test.configuration != "" {
					cli := filepath.Join(directory, privatePath+".tfrc")
					require.NoError(t, os.WriteFile(cli, []byte(test.configuration), 0600))
					t.Setenv("TF_CLI_CONFIG_FILE", cli)
				}
				runner := &baseRunner{folder: directory, tool: tool, localProviders: true}
				logs, planned, changes, err := runner.plan(t.Context())
				require.ErrorContains(t, err, test.reason)
				require.Contains(t, logs, "Retained provider preparation failed: "+err.Error())
				require.NotContains(t, logs, privateValue)
				require.NotContains(t, logs, privatePath)
				require.Nil(t, planned)
				require.Nil(t, changes)
				_, err = os.Stat(runner.planFile())
				require.True(t, os.IsNotExist(err))
			})
		}
	}
}
