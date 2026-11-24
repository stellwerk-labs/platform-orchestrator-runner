//go:build integration

package runner

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"filippo.io/age"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stretchr/testify/require"
)

func TestRetainedProvidersRejectOverridesAndChangedCache(t *testing.T) {
	binary := os.Getenv("IAC_TEST_LOCAL_PROVIDER_BINARY")
	if binary == "" {
		t.Skip("set IAC_TEST_LOCAL_PROVIDER_BINARY for offline retained-provider proof")
	}
	version := os.Getenv("IAC_TEST_LOCAL_PROVIDER_VERSION")
	if version == "" {
		version = "2.5.2"
	}
	for _, profile := range []struct {
		tool       config.IaCBackendType
		runnerUUID string
	}{
		{config.BackendOpenTofu, ""}, {config.BackendTerraform, ""},
		{config.BackendOpenTofu, "3cbdc8ce-60d5-4ccc-9900-c7b6ae874897"}, {config.BackendTerraform, "3cbdc8ce-60d5-4ccc-9900-c7b6ae874897"},
	} {
		tool := profile.tool
		t.Run(string(tool)+"/"+profile.runnerUUID, func(t *testing.T) {
			mirror, cli := retainedProviderMirror(t, binary, version)
			identity, err := age.GenerateX25519Identity()
			require.NoError(t, err)
			root := t.TempDir()
			managed := filepath.Join(root, "managed.txt")
			main := fmt.Sprintf(`terraform {
 backend "local" { path = %q }
 required_providers { local = { source = "hashicorp/local", version = %q } }
}
resource "local_file" "managed" {
 filename = %q
 content = "retained-provider-isolation"
}
`, filepath.Join(root, "state"), version, managed)
			planDir := privateRetainedDirectory(t)
			require.NoError(t, os.WriteFile(filepath.Join(planDir, "main.tf"), []byte(main), 0600))
			plan, err := NewLocalRunner(LocalOptions{Directory: planDir, Backend: tool, Identity: identity})
			require.NoError(t, err)
			_, err = plan.Init(t.Context())
			require.NoError(t, err)
			_, _, _, err = plan.Plan(t.Context())
			require.NoError(t, err)
			binding := RetainedBinding{Schema: "stellwerk.retained-plan/v1", Tool: string(tool)}
			if profile.runnerUUID != "" {
				binding.Schema, binding.RunnerUUID = NativeRetainedSchema, profile.runnerUUID
			}
			encoded, err := plan.RetainPlan(t.Context(), binding)
			require.NoError(t, err)
			ciphertext, err := base64.StdEncoding.DecodeString(encoded)
			require.NoError(t, err)
			assertProviderSnapshot(t, ciphertext, identity, binding)
			foreign := binding
			if binding.RunnerUUID == "" {
				foreign.Schema, foreign.RunnerUUID = NativeRetainedSchema, "7d736b82-2c14-4b4d-811a-b19a85c62b47"
			} else {
				foreign.RunnerUUID = "7d736b82-2c14-4b4d-811a-b19a85c62b47"
			}
			refusedDir := privateRetainedDirectory(t)
			refused, err := NewLocalRunner(LocalOptions{Directory: refusedDir, Backend: tool, Identity: identity})
			require.NoError(t, err)
			require.Error(t, refused.RestoreRetainedBytes(t.Context(), foreign, ciphertext, digest(ciphertext)))
			_, err = os.Stat(filepath.Join(refusedDir, "main.tf"))
			require.True(t, os.IsNotExist(err), "foreign UUID extracted provider archive")
			restore := func() (LocalRunner, error) {
				runner, err := NewLocalRunner(LocalOptions{Directory: privateRetainedDirectory(t), Backend: tool, Identity: identity})
				require.NoError(t, err)
				return runner, runner.RestoreRetainedBytes(t.Context(), binding, ciphertext, digest(ciphertext))
			}
			override := filepath.Join(t.TempDir(), "override.tfrc")
			require.NoError(t, os.WriteFile(override, []byte(`provider_installation { dev_overrides { "hashicorp/local" = "/tmp/untrusted" } }`), 0600))
			t.Setenv("TF_CLI_CONFIG_FILE", override)
			_, err = restore()
			require.ErrorContains(t, err, "override")
			t.Setenv("TF_CLI_CONFIG_FILE", cli)
			setRetainedMirror(t, mirror, version, binary, true)
			_, err = restore()
			require.EqualError(t, err, "retained provider initialization failed")
			_, err = os.Stat(managed)
			require.True(t, os.IsNotExist(err), "refused restore executed Apply")
			setRetainedMirror(t, mirror, version, binary, false)
			apply, err := restore()
			require.NoError(t, err)
			// Mutating the shared cache after verified restoration cannot replace
			// the provider package that the saved plan will actually execute.
			setRetainedMirror(t, mirror, version, binary, true)
			_, changes, err := apply.Apply(t.Context())
			require.NoError(t, err)
			require.Equal(t, 1, changes.Added)
			content, err := os.ReadFile(managed)
			require.NoError(t, err)
			require.Equal(t, "retained-provider-isolation", string(content))
		})
	}
}

func retainedProviderMirror(t *testing.T, binary, version string) (string, string) {
	t.Helper()
	mirror := t.TempDir()
	setRetainedMirror(t, mirror, version, binary, false)
	cli := filepath.Join(t.TempDir(), "providers.tfrc")
	require.NoError(t, os.WriteFile(cli, fmt.Appendf(nil, "provider_installation { filesystem_mirror { path = %q } }\n", mirror), 0600))
	t.Setenv("TF_CLI_CONFIG_FILE", cli)
	return mirror, cli
}

func setRetainedMirror(t *testing.T, mirror, version, binary string, changed bool) {
	t.Helper()
	for _, host := range []string{"registry.terraform.io", "registry.opentofu.org"} {
		dir := filepath.Join(mirror, host, "hashicorp", "local", version, runtime.GOOS+"_"+runtime.GOARCH)
		require.NoError(t, os.MkdirAll(dir, 0700))
		path := filepath.Join(dir, "terraform-provider-local")
		err := os.Remove(path)
		require.True(t, err == nil || os.IsNotExist(err))
		if changed {
			require.NoError(t, os.WriteFile(path, []byte("changed unapproved package"), 0500))
		} else {
			require.NoError(t, os.Symlink(binary, path))
		}
	}
}

func assertProviderSnapshot(t *testing.T, ciphertext []byte, identity *age.X25519Identity, binding RetainedBinding) {
	t.Helper()
	plain, err := age.Decrypt(bytes.NewReader(ciphertext), identity)
	require.NoError(t, err)
	var snapshot retainedSnapshot
	require.NoError(t, json.NewDecoder(plain).Decode(&snapshot))
	expected := retainedProviderSchema
	if binding.RunnerUUID != "" {
		expected = NativeRetainedSchema
	}
	require.Equal(t, expected, snapshot.Binding.Schema)
	require.Equal(t, binding.RunnerUUID, snapshot.Binding.RunnerUUID)
	require.NotEmpty(t, snapshot.ProviderFiles)
	dir := t.TempDir()
	require.NoError(t, unpackRetainedFiles(dir, snapshot.Archive))
	_, err = os.Stat(filepath.Join(dir, ".terraform", "providers"))
	require.True(t, os.IsNotExist(err), "provider binaries entered the retained archive")
}

func privateRetainedDirectory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))
	return dir
}
