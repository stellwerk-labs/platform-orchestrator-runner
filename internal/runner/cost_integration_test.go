//go:build integration

package runner

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Real OpenTofu, its built-in provider and a local backend exercise both JSON boundaries offline.
func TestCostBothPlanBoundaries(t *testing.T) {
	binary, err := exec.LookPath("tofu")
	require.NoError(t, err)
	original := slog.Default()
	defer slog.SetDefault(original)
	cases := []struct {
		name                  string
		observation, disabled bool
		entries               int
	}{
		{"manual", false, false, 1}, {"manual_disabled", false, true, 0},
		{"observation", true, false, 1}, {"observation_disabled", true, true, 0},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {

			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`resource "terraform_data" "private" { input = "SECRET_COST_HOOK_CANARY" }
output "private" {
 value = terraform_data.private.output
 sensitive = true
}
`), 0600))
			r := baseRunner{folder: dir, binaryPath: binary, tool: "opentofu", observationOnly: test.observation, costEstimationDisabled: test.disabled}
			var logs bytes.Buffer
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			if test.observation {
				result, err := r.Observe(t.Context())
				require.NoError(t, err)
				require.True(t, bool(result.Complete))
			} else {
				_, err := r.init(t.Context())
				require.NoError(t, err)
				_, _, changes, err := r.plan(t.Context())
				require.NoError(t, err)
				require.Equal(t, 1, changes.Added)
			}
			require.Equal(t, test.entries, strings.Count(logs.String(), `"cost_estimation"`))
			require.NotContains(t, logs.String(), "SECRET_COST_HOOK_CANARY")
			matches, err := filepath.Glob(filepath.Join(dir, ".observation-*"))
			require.NoError(t, err)
			require.Empty(t, matches)
			_, err = os.Stat(filepath.Join(dir, "terraform.tfstate"))
			require.True(t, os.IsNotExist(err), "a plan must not write state")
		})
	}
}

func TestCostFailurePreservesPlanAndObservation(t *testing.T) {
	binary, err := exec.LookPath("tofu")
	require.NoError(t, err)
	original := slog.Default()
	defer slog.SetDefault(original)
	for _, mode := range []string{"manual", "observation"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			// The valid plan exceeds only the estimator's stricter 8 MiB boundary.
			require.NoError(t, os.WriteFile(filepath.Join(dir, "payload"), bytes.Repeat([]byte("S"), maxCostPlanBytes), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`resource "terraform_data" "private" { input = file("payload") }`), 0600))
			r := baseRunner{folder: dir, binaryPath: binary, tool: "opentofu", observationOnly: mode == "observation"}
			var logs bytes.Buffer
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			if mode == "observation" {
				result, err := r.Observe(t.Context())
				require.NoError(t, err)
				require.True(t, bool(result.Complete))
			} else {
				_, err := r.init(t.Context())
				require.NoError(t, err)
				_, _, changes, err := r.plan(t.Context())
				require.NoError(t, err)
				require.Equal(t, 1, changes.Added)
			}
			require.Equal(t, 1, strings.Count(logs.String(), `"cost_estimation"`))
			require.Contains(t, logs.String(), `"level":"WARN"`)
			require.Contains(t, logs.String(), `"status":"unknown"`)
			require.NotContains(t, logs.String(), "known_monthly_subtotal")
			require.Less(t, logs.Len(), 1500)
			plans, err := filepath.Glob(filepath.Join(dir, ".observation-*"))
			require.NoError(t, err)
			require.Empty(t, plans)
			_, err = os.Stat(filepath.Join(dir, "terraform.tfstate"))
			require.True(t, os.IsNotExist(err))
		})
	}
}
