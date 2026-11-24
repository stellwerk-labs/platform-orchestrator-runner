package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProviderConfigSelectionUsesActualEnvironmentPrecedence(t *testing.T) {
	t.Setenv("TF_CLI_CONFIG_FILE", "")
	t.Setenv("TERRAFORM_CONFIG", "")
	t.Setenv("TOFU_CLI_CONFIG_FILE", filepath.Join(t.TempDir(), "ignored.tfrc"))
	for _, tool := range []string{"terraform", "opentofu"} {
		paths, err := providerCLIConfigPaths(tool)
		require.NoError(t, err)
		require.NotContains(t, paths, os.Getenv("TOFU_CLI_CONFIG_FILE"))
	}
	alias := filepath.Join(t.TempDir(), "alias.tfrc")
	require.NoError(t, os.WriteFile(alias, []byte(`provider_installation { "dev_\u006fverrides" {} }`), 0600))
	t.Setenv("TERRAFORM_CONFIG", alias)
	for _, tool := range []string{"terraform", "opentofu"} {
		paths, err := providerCLIConfigPaths(tool)
		require.NoError(t, err)
		require.Equal(t, []string{alias}, paths)
		require.Error(t, validateRetainedProviderConfig(tool))
	}
	t.Setenv("TF_CLI_CONFIG_FILE", filepath.Join(t.TempDir(), "primary.tfrc"))
	paths, err := providerCLIConfigPaths("terraform")
	require.NoError(t, err)
	require.Equal(t, []string{os.Getenv("TF_CLI_CONFIG_FILE")}, paths)
}

func TestProviderDefaultConfigIncludesFragmentsAndTofuFallbacks(t *testing.T) {
	directory, xdg := t.TempDir(), t.TempDir()
	legacy := filepath.Join(directory, ".terraformrc")
	tofu := filepath.Join(directory, ".tofurc")
	fragments := filepath.Join(directory, ".terraform.d")
	require.NoError(t, os.Mkdir(fragments, 0700))
	bad := filepath.Join(fragments, "unsafe.tfrc.json")
	require.NoError(t, os.WriteFile(bad, []byte(`{"PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE":true}`), 0600))
	require.NoError(t, os.WriteFile(legacy, []byte("disable_checkpoint = true"), 0600))
	for _, tool := range []string{"terraform", "opentofu"} {
		paths, err := defaultProviderCLIConfigPaths(tool, directory, xdg)
		require.NoError(t, err)
		require.Equal(t, []string{legacy, bad}, paths)
		require.Error(t, validateProviderCLIConfigFiles(paths))
	}
	require.NoError(t, os.WriteFile(tofu, []byte("disable_checkpoint = true"), 0600))
	paths, err := defaultProviderCLIConfigPaths("opentofu", directory, xdg)
	require.NoError(t, err)
	require.Equal(t, []string{tofu, bad}, paths)
	require.NoError(t, os.RemoveAll(fragments))
	require.NoError(t, os.Remove(legacy))
	require.NoError(t, os.Remove(tofu))
	xdgDir := filepath.Join(xdg, "opentofu")
	require.NoError(t, os.Mkdir(xdgDir, 0700))
	xdgBad := filepath.Join(xdgDir, "unsafe.tfrc")
	require.NoError(t, os.WriteFile(xdgBad, []byte(`provider_installation { "dev_\u006fverrides" {} }`), 0600))
	paths, err = defaultProviderCLIConfigPaths("opentofu", directory, xdg)
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(xdgDir, "tofurc"), xdgBad}, paths)
	require.Error(t, validateProviderCLIConfigFiles(paths))
}
