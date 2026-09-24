package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVerifyArtifactManifestAcceptsRepeatedGitModuleDownloads(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, ".terraform", "modules", "first")
	second := filepath.Join(root, ".terraform", "modules", "second")
	for _, directory := range []string{first, second} {
		require.NoError(t, os.MkdirAll(directory, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(directory, "main.tf"), []byte("resource {}\n"), 0o644))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(first, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(first, ".github", "workflows"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(first, ".git", "config"), []byte("worktree=/tmp/first\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(first, ".github", "workflows", "ci.yml"), []byte("name: ci\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(first, ".gitignore"), []byte("*.tfstate\n"), 0o644))

	modules := `{"Modules":[{"Key":"","Source":"","Dir":"."},{"Key":"first","Source":"git::https://example.com/module?ref=abc","Dir":".terraform/modules/first"},{"Key":"second","Source":"git::https://example.com/module?ref=abc","Dir":".terraform/modules/second"}]}`
	require.NoError(t, os.WriteFile(filepath.Join(root, ".terraform", "modules", "modules.json"), []byte(modules), 0o644))
	manifest := `{"version":1,"artifacts":[{"module_id":"workload","version":"1.0.0","source":"git::https://example.com/module?ref=abc","artifact_digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}]}`
	require.NoError(t, os.WriteFile(filepath.Join(root, artifactManifestName), []byte(manifest), 0o644))

	require.NoError(t, verifyArtifactManifest(root))
}

func TestVerifyArtifactManifestDoesNotClaimTrustedDigestVerification(t *testing.T) {
	root := t.TempDir()
	moduleDir := filepath.Join(root, ".terraform", "modules", "database")
	require.NoError(t, os.MkdirAll(moduleDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "main.tf"), []byte("resource {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".terraform", "modules", "modules.json"), []byte(`{"Modules":[{"Key":"database","Source":"git::https://example.com/database?ref=abc","Dir":".terraform/modules/database"}]}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, artifactManifestName), []byte(`{"version":1,"artifacts":[{"module_id":"database","version":"2.4.0","source":"git::https://example.com/database?ref=abc","artifact_digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}]}`), 0o644))

	require.NoError(t, verifyArtifactManifest(root))
}

func TestVerifyArtifactManifestAcceptsInlineSourceWithoutDigest(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, artifactManifestName), []byte(`{"version":1,"artifacts":[{"module_id":"database","version":"2.4.0","source":"inline","artifact_digest":""}]}`), 0o644))

	require.NoError(t, verifyArtifactManifest(root))
}

func TestVerifyArtifactManifestRejectsDigestForInlineSource(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, artifactManifestName), []byte(`{"version":1,"artifacts":[{"module_id":"database","version":"2.4.0","source":"inline","artifact_digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}]}`), 0o644))

	err := verifyArtifactManifest(root)
	var verificationError *ArtifactValidationError
	require.ErrorAs(t, err, &verificationError)
	require.Equal(t, "ARTIFACT_MANIFEST_INVALID", verificationError.Code())
	require.Contains(t, verificationError.Reason, "inline source must not declare")
}

func TestVerifyArtifactManifestRejectsMalformedDeclaredDigest(t *testing.T) {
	for _, digest := range []string{"sha256:abc", "sha256:" + strings.Repeat("A", 64), "sha512:" + strings.Repeat("0", 64), " sha256:" + strings.Repeat("0", 64)} {
		t.Run(digest, func(t *testing.T) {
			root := t.TempDir()
			encoded, err := json.Marshal(artifactManifest{Version: 1, Artifacts: []artifactRequirement{{ModuleID: "database", Version: "2.4.0", Source: "registry.example/database@2.4.0", ArtifactDigest: digest}}})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(root, artifactManifestName), encoded, 0o644))
			var verificationError *ArtifactValidationError
			require.ErrorAs(t, verifyArtifactManifest(root), &verificationError)
			require.Equal(t, "ARTIFACT_MANIFEST_INVALID", verificationError.Code())
		})
	}
}

func TestVerifyArtifactManifestRejectsMissingArtifact(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, artifactManifestName), []byte(`{"version":1,"artifacts":[{"module_id":"database","version":"2.4.0","source":"registry.example/database@2.4.0","artifact_digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}]}`), 0o644))

	err := verifyArtifactManifest(root)
	var verificationError *ArtifactValidationError
	require.ErrorAs(t, err, &verificationError)
	require.Equal(t, "ARTIFACT_UNAVAILABLE", verificationError.Code())
	require.Contains(t, verificationError.Reason, "artifact was not downloaded")
	require.Equal(t, "database", verificationError.ModuleID)
	require.Equal(t, "2.4.0", verificationError.Version)
}

func TestVerifyArtifactManifestIsOptionalForNormalDeployments(t *testing.T) {
	require.NoError(t, verifyArtifactManifest(t.TempDir()))
}

func TestVerifyArtifactManifestLegacyMigrationContract(t *testing.T) {
	const source = "git::https://example.com/module?ref=immutable-history"
	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	for _, test := range []struct {
		name       string
		generation string
		semver     string
		digest     string
		downloaded bool
		code       string
	}{
		{name: "v0 carry-forward", generation: "v0", downloaded: true},
		{name: "v0 missing artifact", generation: "v0", code: "ARTIFACT_UNAVAILABLE"},
		{name: "v0 cannot claim SemVer", generation: "v0", semver: "1.0.0", downloaded: true, code: "ARTIFACT_MANIFEST_INVALID"},
		{name: "v0 cannot claim digest", generation: "v0", digest: digest, downloaded: true, code: "ARTIFACT_MANIFEST_INVALID"},
		{name: "v1 without optional digest", generation: "v1", semver: "1.0.0", downloaded: true},
		{name: "managed without optional digest", generation: "managed", semver: "2.0.0", downloaded: true},
		{name: "managed missing artifact without digest", generation: "managed", semver: "2.0.0", code: "ARTIFACT_UNAVAILABLE"},
		{name: "managed missing artifact with digest", generation: "managed", semver: "2.0.0", digest: digest, code: "ARTIFACT_UNAVAILABLE"},
		{name: "absent generation does not require digest", downloaded: true},
		{name: "unknown generation does not require digest", generation: "v0-compatible", downloaded: true},
		{name: "v1 declared claim remains unverified", generation: "v1", semver: "1.0.0", digest: digest, downloaded: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if test.downloaded {
				require.NoError(t, os.MkdirAll(filepath.Join(root, ".terraform/modules/legacy"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(root, ".terraform/modules/modules.json"), []byte(`{"Modules":[{"Key":"legacy","Source":"`+source+`","Dir":".terraform/modules/legacy"}]}`), 0o644))
			}
			encoded, err := json.Marshal(artifactManifest{Version: 1, Artifacts: []artifactRequirement{{
				ModuleID: "database", Version: "opaque-legacy-identity", Source: source,
				MigrationGeneration: test.generation, SemanticVersion: test.semver, ArtifactDigest: test.digest,
			}}})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(root, artifactManifestName), encoded, 0o644))
			err = verifyArtifactManifest(root)
			if test.code == "" {
				require.NoError(t, err)
				return
			}
			var validationError *ArtifactValidationError
			require.ErrorAs(t, err, &validationError)
			require.Equal(t, test.code, validationError.Code())
		})
	}
}

func TestVerifyArtifactManifestRetainsEmptyClaimReadCompatibility(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".terraform/modules/retained"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".terraform/modules/modules.json"), []byte(`{"Modules":[{"Key":"database","Source":"git::https://example.com/retained","Dir":".terraform/modules/retained"}]}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, artifactManifestName), []byte(`{"version":1,"artifacts":[{"module_id":"database","version":"2.4.0","migration_generation":"managed","semantic_version":"2.4.0","source":"git::https://example.com/retained","artifact_digest":""}]}`), 0o644))
	require.NoError(t, verifyArtifactManifest(root))
}

func TestVerifyArtifactManifestRejectsMissingDownloadedDirectory(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".terraform/modules"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".terraform/modules/modules.json"), []byte(`{"Modules":[{"Key":"legacy","Source":"git::https://example.com/module?ref=immutable-history","Dir":".terraform/modules/deleted"}]}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, artifactManifestName), []byte(`{"version":1,"artifacts":[{"module_id":"database","version":"opaque-v0","migration_generation":"v0","source":"git::https://example.com/module?ref=immutable-history"}]}`), 0o644))
	var validationError *ArtifactValidationError
	require.ErrorAs(t, verifyArtifactManifest(root), &validationError)
	require.Equal(t, "ARTIFACT_UNAVAILABLE", validationError.Code())
}

func TestVerifyArtifactManifestAcceptsMountedExternalModule(t *testing.T) {
	root, mountedModule := t.TempDir(), t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".terraform/modules"), 0o755))
	downloads, err := json.Marshal(map[string]any{"Modules": []map[string]string{{"Key": "mounted", "Source": "file://" + filepath.ToSlash(mountedModule), "Dir": mountedModule}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, ".terraform/modules/modules.json"), downloads, 0o644))
	manifest, err := json.Marshal(artifactManifest{Version: 1, Artifacts: []artifactRequirement{{ModuleID: "mounted", Version: "opaque-v0", MigrationGeneration: "v0", Source: mountedModule}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, artifactManifestName), manifest, 0o644))
	require.NoError(t, verifyArtifactManifest(root))
}
