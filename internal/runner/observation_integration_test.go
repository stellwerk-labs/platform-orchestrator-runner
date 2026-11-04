//go:build integration

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Real local providers make external filesystem changes visible during refresh.
// No orchestration service, cloud credentials, or simulated CLI is involved.
func TestObservationRealTools(t *testing.T) {
	binaries := os.Getenv("IAC_TEST_BINARIES")
	if binaries == "" {
		binaries = "tofu,terraform"
	}
	for _, binary := range strings.Split(binaries, ",") {
		t.Run(binary, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
			defer cancel()
			path, err := exec.LookPath(binary)
			require.NoError(t, err)
			dir := t.TempDir()
			file := filepath.Join(dir, "managed.txt")
			tool := "terraform"
			if filepath.Base(binary) == "tofu" {
				tool = "opentofu"
			}
			baseline := fmt.Sprintf(`terraform {
 required_providers { local = { source = "hashicorp/local", version = "2.5.2" } }
}
resource "local_file" "managed" {
 for_each = toset(["SECRET_ADDRESS_CANARY"])
 filename = %q
 content = "SECRET_CONTENT_CANARY"
}
output "sensitive" {
 value = "SECRET_OUTPUT_CANARY"
 sensitive = true
}
`, file)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(baseline), 0600))
			run := func(args ...string) {
				t.Helper()
				out, err := exec.CommandContext(ctx, path, append([]string{"-chdir=" + dir}, args...)...).CombinedOutput()
				require.NoError(t, err, "fixture setup: %s", out)
			}
			run("init", "-input=false", "-no-color")
			run("apply", "-auto-approve", "-input=false", "-no-color")
			state, err := os.ReadFile(filepath.Join(dir, "terraform.tfstate"))
			require.NoError(t, err)
			r := &baseRunner{folder: dir, binaryPath: path, tool: tool, observationOnly: true}
			assertUnchanged := func() {
				t.Helper()
				after, err := os.ReadFile(filepath.Join(dir, "terraform.tfstate"))
				require.NoError(t, err)
				require.Equal(t, state, after, "observation persisted state")
				plans, err := filepath.Glob(filepath.Join(dir, ".observation-*"))
				require.NoError(t, err)
				require.Empty(t, plans, "sensitive saved plan remained")
			}
			t.Run("clean", func(t *testing.T) {
				result, err := r.Observe(ctx)
				require.NoError(t, err)
				require.Empty(t, result.ObservedChanges)
				require.Empty(t, result.ProposedChanges)
				require.Zero(t, result.OutputChangeCount)
				assertUnchanged()
			})
			t.Run("external change and corrective actions", func(t *testing.T) {
				require.NoError(t, os.WriteFile(file, []byte("EXTERNAL_SECRET_CANARY"), 0600))
				// Inherited overrides must not disable refresh or limit the full plan.
				t.Setenv("TF_CLI_ARGS_plan", "-refresh=false -target=terraform_data.nonexistent")
				logPath := filepath.Join(dir, "provider-secret.log")
				t.Setenv("TF_LOG", "TRACE")
				t.Setenv("TF_LOG_PATH", logPath)
				result, err := r.Observe(ctx)
				require.NoError(t, err)
				require.NotEmpty(t, result.ObservedChanges)
				require.NotEmpty(t, result.ProposedChanges)
				require.Equal(t, "delete", string(result.ObservedChanges[0].Actions[0]))
				require.Equal(t, "create", string(result.ProposedChanges[0].Actions[0]))
				encoded, err := json.Marshal(result)
				require.NoError(t, err)
				for _, secret := range []string{"SECRET_ADDRESS_CANARY", "SECRET_CONTENT_CANARY", "SECRET_OUTPUT_CANARY", "EXTERNAL_SECRET_CANARY", file, "address", "before", "after"} {
					require.NotContains(t, string(encoded), secret)
				}
				actual, err := os.ReadFile(file)
				require.NoError(t, err)
				require.Equal(t, "EXTERNAL_SECRET_CANARY", string(actual), "observation applied corrective actions")
				_, err = os.Stat(logPath)
				require.True(t, os.IsNotExist(err), "raw Terraform diagnostics were written")
				assertUnchanged()
			})
			t.Run("backend lock", func(t *testing.T) {
				locked, err := os.OpenFile(filepath.Join(dir, "terraform.tfstate"), os.O_RDWR, 0600)
				require.NoError(t, err)
				defer locked.Close()
				require.NoError(t, syscall.Flock(int(locked.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
				defer syscall.Flock(int(locked.Fd()), syscall.LOCK_UN)
				result, err := r.Observe(ctx)
				require.Error(t, err)
				require.Nil(t, result)
				require.EqualError(t, err, "observation plan failed")
				assertUnchanged()
			})
			t.Run("provider diagnostic", func(t *testing.T) {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(baseline+"\noutput \"SECRET_DIAGNOSTIC_CANARY\" { value = missing.SECRET_DIAGNOSTIC_CANARY }\n"), 0600))
				result, err := r.Observe(ctx)
				require.Error(t, err)
				require.Nil(t, result)
				require.NotContains(t, err.Error(), "SECRET_DIAGNOSTIC_CANARY")
				assertUnchanged()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(baseline), 0600))
			})
			t.Run("bounded stdout and secret stderr", func(t *testing.T) {
				out, err := r.observationCommand(ctx, 8, "version", "-json")
				require.Error(t, err)
				require.Empty(t, out)
				out, err = r.observationCommand(ctx, maxObservationPlanBytes, "-chdir="+dir, "show", "-json", filepath.Join(dir, "SECRET_ERROR_CANARY"))
				require.Error(t, err)
				require.Empty(t, out)
				require.NotContains(t, err.Error(), "SECRET_ERROR_CANARY")
			})
			t.Run("cancel", func(t *testing.T) {
				canceled, stop := context.WithCancel(ctx)
				stop()
				result, err := r.Observe(canceled)
				require.Error(t, err)
				require.Nil(t, result)
				assertUnchanged()
			})
		})
	}
}
