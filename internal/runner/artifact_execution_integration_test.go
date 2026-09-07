//go:build integration

package runner

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// This test uses the real IaC initializer and executor with a local Git artifact.
// It needs no registry, cloud credentials or provider plugin downloads.
func TestArtifactExecutionPreservesLegacyHistory(t *testing.T) {
	binaries := os.Getenv("IAC_TEST_BINARIES")
	if binaries == "" {
		binaries = "tofu"
	}
	for _, binary := range strings.Split(binaries, ",") {
		t.Run(binary, func(t *testing.T) {
			binaryPath, err := exec.LookPath(binary)
			require.NoError(t, err, "install %s before running the artifact integration suite", binary)
			artifact := t.TempDir()
			git := func(args ...string) string {
				command := exec.CommandContext(t.Context(), "git", args...)
				command.Dir = artifact
				output, err := command.CombinedOutput()
				require.NoError(t, err, "%s", output)
				return strings.TrimSpace(string(output))
			}
			git("init", "--initial-branch=fixture")
			git("config", "user.name", "Stellwerk integration fixture")
			git("config", "user.email", "integration@example.invalid")
			git("config", "commit.gpgsign", "false")
			commitArtifact := func(value string) string {
				source := fmt.Sprintf("resource \"terraform_data\" \"value\" { input = %q }\noutput \"value\" { value = terraform_data.value.output }\n", value)
				require.NoError(t, os.WriteFile(filepath.Join(artifact, "main.tf"), []byte(source), 0o600))
				git("add", "main.tf")
				git("commit", "-m", "fixture "+value)
				return git("rev-parse", "HEAD")
			}
			legacyRevision := commitArtifact("legacy")
			managedRevision := commitArtifact("managed")
			root := t.TempDir()
			base := &baseRunner{folder: root, binaryPath: binaryPath}
			artifactURL := (&url.URL{Scheme: "file", Path: artifact}).String()
			deploy := func(t *testing.T, revision, generation, semver, expected string, includeManifest bool) {
				source := "git::" + artifactURL + "?ref=" + revision
				if revision == "mounted" {
					source = artifact
				}
				configuration := fmt.Sprintf("module \"database\" { source = %q }\noutput \"value\" { value = module.database.value }\n", source)
				require.NoError(t, os.WriteFile(filepath.Join(root, "main.tf"), []byte(configuration), 0o600))
				if includeManifest {
					requirement := artifactRequirement{ModuleID: "database", Version: "opaque-" + revision, Source: source, MigrationGeneration: generation, SemanticVersion: semver}
					if generation != "v0" {
						requirement.ArtifactDigest = "sha256:" + strings.Repeat("0", 64)
					}
					encoded, err := json.Marshal(artifactManifest{Version: 1, Artifacts: []artifactRequirement{requirement}})
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(filepath.Join(root, artifactManifestName), encoded, 0o600))
				}
				_, err := base.init(t.Context())
				if err != nil {
					downloads, _ := os.ReadFile(filepath.Join(root, ".terraform/modules/modules.json"))
					require.NoError(t, err, "initializer module metadata: %s", downloads)
				}
				_, _, _, err = base.plan(t.Context())
				require.NoError(t, err)
				_, _, err = base.apply(t.Context())
				require.NoError(t, err)
				value, err := base.OutputMetadata(t.Context(), "value")
				require.NoError(t, err)
				require.JSONEq(t, fmt.Sprintf("%q", expected), value)
			}
			t.Run("pre-management bundle", func(t *testing.T) { deploy(t, legacyRevision, "", "", "legacy", false) })
			t.Run("migrated v0 carry-forward", func(t *testing.T) { deploy(t, legacyRevision, "v0", "", "legacy", true) })
			t.Run("first managed v1", func(t *testing.T) { deploy(t, managedRevision, "v1", "1.0.0", "managed", true) })
			t.Run("exact history rollback to v0", func(t *testing.T) { deploy(t, legacyRevision, "v0", "", "legacy", true) })
			t.Run("mounted external artifact", func(t *testing.T) { deploy(t, "mounted", "v0", "", "managed", true) })
		})
	}
}
