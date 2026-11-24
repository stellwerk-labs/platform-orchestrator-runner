package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnpackRetainedFilesRejectsUnsafeEntries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []*tar.Header
	}{
		{"traversal", []*tar.Header{{Name: "../escape", Mode: 0600}}},
		{"absolute", []*tar.Header{{Name: "/escape", Mode: 0600}}},
		{"link", []*tar.Header{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../escape"}}},
		{"duplicate", []*tar.Header{{Name: "main.tf"}, {Name: "main.tf"}}},
		{"oversize", []*tar.Header{{Name: "large", Size: retainedExpandedLimit + 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			gz := gzip.NewWriter(&b)
			tw := tar.NewWriter(gz)
			for _, h := range tc.headers {
				require.NoError(t, tw.WriteHeader(h))
			}
			_ = tw.Close()
			require.NoError(t, gz.Close())
			dir := t.TempDir()
			require.Error(t, unpackRetainedFiles(dir, b.Bytes()))
			_, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape"))
			require.True(t, os.IsNotExist(err))
		})
	}
}

func TestRetainedProviderFormatCannotSilentlyDowngrade(t *testing.T) {
	binding := RetainedBinding{Schema: "stellwerk.retained-plan/v1", OrgID: "org"}
	snapshot := retainedSnapshot{Binding: binding, OS: runtime.GOOS, Arch: runtime.GOARCH,
		ProviderFiles: map[string]retainedProviderFile{"provider": {SHA256: "bound"}}}
	snapshot.Binding.Schema = retainedProviderSchema
	require.NotEqual(t, binding, snapshot.Binding, "legacy Runner exact binding comparison must refuse before Apply")
	require.True(t, retainedSnapshotMatches(snapshot, binding))
	snapshot.Binding.Schema = binding.Schema
	require.False(t, retainedSnapshotMatches(snapshot, binding), "provider manifest cannot hide in a v1 snapshot")
	snapshot.ProviderFiles = nil
	require.True(t, retainedSnapshotMatches(snapshot, binding), "ordinary legacy snapshots remain supported")
}

func TestRetainedProviderOverridesAreRejected(t *testing.T) {
	for _, config := range []string{
		`provider_installation { dev_overrides { "hashicorp/local" = "/tmp/private-provider" } }`,
		`{"provider_installation":{"dev_\u006fverrides":{"hashicorp/local":"/tmp/private-provider"}}}`,
		`provider_installation { "dev_\u006fverrides" { "hashicorp/local" = "/tmp/private-provider" } }`,
		`plugin_cache_may_break_dependency_lock_file = true`,
		`PLUGIN_CACHE_MAY_BREAK_DEPENDENCY_LOCK_FILE = true`,
	} {
		t.Run(config[:10], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.tfrc")
			require.NoError(t, os.WriteFile(path, []byte(config), 0600))
			t.Setenv("TF_CLI_CONFIG_FILE", path)
			require.Error(t, validateRetainedProviderConfig("terraform"))
		})
	}
	t.Run("reattachment", func(t *testing.T) {
		t.Setenv("TF_REATTACH_PROVIDERS", "{}")
		require.Error(t, validateRetainedProviderConfig("opentofu"))
	})
}

func TestPackRetainedFilesRejectsSymlink(t *testing.T) {
	for _, name := range []string{"link", ".terraform/providers"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, name)
			require.NoError(t, os.MkdirAll(filepath.Dir(target), 0700))
			require.NoError(t, os.Symlink("/etc/passwd", target))
			_, err := packRetainedFiles(dir, false)
			require.Error(t, err)
		})
	}
}

func TestRetainedSchemaRequiresItsExactUUIDProfile(t *testing.T) {
	native := RetainedBinding{Schema: NativeRetainedSchema, RunnerUUID: "3cbdc8ce-60d5-4ccc-9900-c7b6ae874897"}
	for _, providers := range []bool{false, true} {
		snap := retainedSnapshot{Binding: native, OS: runtime.GOOS, Arch: runtime.GOARCH}
		if providers {
			snap.ProviderFiles = map[string]retainedProviderFile{"provider": {SHA256: "bound"}}
		}
		require.True(t, retainedSnapshotMatches(snap, native))
		foreign := native
		foreign.RunnerUUID = "7d736b82-2c14-4b4d-811a-b19a85c62b47"
		require.False(t, retainedSnapshotMatches(snap, foreign))
		for _, invalid := range []RetainedBinding{
			{Schema: NativeRetainedSchema}, {Schema: NativeRetainedSchema, RunnerUUID: "00000000-0000-0000-0000-000000000000"},
			{Schema: NativeRetainedSchema, RunnerUUID: "3CBDC8CE-60D5-4CCC-9900-C7B6AE874897"},
			{Schema: "stellwerk.retained-plan/v1", RunnerUUID: native.RunnerUUID}, {Schema: retainedProviderSchema, RunnerUUID: native.RunnerUUID},
		} {
			snap.Binding = invalid
			require.False(t, retainedSnapshotMatches(snap, invalid), "equal invalid bindings must not authorize restore")
		}
	}
}
