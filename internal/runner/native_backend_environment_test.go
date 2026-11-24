package runner

import (
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stretchr/testify/require"
)

func TestNativeBackendEnvironmentRefusesAmbientSelection(t *testing.T) {
	for _, name := range []string{"KUBE_HOST", "KUBE_CTX", "KUBE_CTX_CLUSTER", "HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "PRIVATE_NATIVE_ENVIRONMENT_CANARY")
			err := ValidateNativeKubernetesEnvironment("")
			require.EqualError(t, err, "native Kubernetes backend environment refused")
			if len(name) > 5 && name[:5] == "KUBE_" {
				require.Error(t, ValidateNativeKubernetesEnvironment("http://proxy.test:8080"))
			} else {
				require.NoError(t, ValidateNativeKubernetesEnvironment("http://proxy.test:8080"))
			}
		})
	}
}

func TestNativeBackendEnvironmentIsCheckedAtSubprocessBoundaries(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	proxy := ""
	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0700))
	local, err := NewLocalRunner(LocalOptions{Directory: directory, Backend: config.BackendOpenTofu, Identity: identity, NativeKubernetesProxy: &proxy})
	require.NoError(t, err)
	native := local.(*OpenTofuRunner).baseRunner
	native.binaryPath = "/bin/sh"
	proxy = "http://caller-mutated.test"
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	t.Setenv("HTTP_PROXY", "http://PRIVATE_NATIVE_ENVIRONMENT_CANARY")
	_, err = native.runCommand(t.Context(), "-c", "touch \"$1\"", "sh", marker)
	require.EqualError(t, err, "native Kubernetes backend environment refused")
	_, err = native.observationCommand(t.Context(), 0, "-c", "touch \"$1\"", "sh", marker)
	require.EqualError(t, err, "native Kubernetes backend environment refused")
	require.EqualError(t, native.restoreProviders(t.Context(), nil), "native Kubernetes backend environment refused")
	_, err = os.Stat(marker)
	require.True(t, os.IsNotExist(err), "guarded subprocesses must never start")
	legacy, err := NewLocalRunner(LocalOptions{Directory: directory, Backend: config.BackendOpenTofu, Identity: identity})
	require.NoError(t, err)
	legacyRunner := legacy.(*OpenTofuRunner).baseRunner
	legacyRunner.binaryPath = "/bin/sh"
	output, err := legacyRunner.runCommand(t.Context(), "-c", "printf legacy")
	require.NoError(t, err)
	require.Equal(t, "legacy", output)
}
