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
			if binary == "tofu" {
				output, err := exec.CommandContext(t.Context(), binaryPath, "version", "-json").CombinedOutput()
				require.NoError(t, err, "%s", output)
				var version struct {
					TerraformVersion string `json:"terraform_version"`
				}
				require.NoError(t, json.Unmarshal(output, &version))
				require.Equal(t, "1.13.9-stellwerk.remediation.20261123.1", version.TerraformVersion)
			}
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
			type stateSnapshot struct {
				TerraformVersion string `json:"terraform_version"`
				Lineage          string `json:"lineage"`
				Serial           int64  `json:"serial"`
			}
			readState := func(t *testing.T) stateSnapshot {
				t.Helper()
				data, err := os.ReadFile(filepath.Join(root, "terraform.tfstate"))
				require.NoError(t, err)
				var state stateSnapshot
				require.NoError(t, json.Unmarshal(data, &state))
				require.NotEmpty(t, state.Lineage)
				return state
			}
			var previousState stateSnapshot
			deploy := func(t *testing.T, revision, generation, semver, expected string, includeManifest bool) {
				t.Helper()
				source := "git::" + artifactURL + "?ref=" + revision
				if revision == "mounted" {
					source = artifact
				}
				configuration := fmt.Sprintf("module \"database\" { source = %q }\noutput \"value\" { value = module.database.value }\n", source)
				require.NoError(t, os.WriteFile(filepath.Join(root, "main.tf"), []byte(configuration), 0o600))
				if includeManifest {
					requirement := artifactRequirement{ModuleID: "database", Version: "opaque-" + revision, Source: source, MigrationGeneration: generation, SemanticVersion: semver}
					if generation == "v1" {
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
				state := readState(t)
				if previousState.Lineage != "" {
					require.Equal(t, previousState.Lineage, state.Lineage, "carry-forward and history rollback must retain state lineage")
					require.GreaterOrEqual(t, state.Serial, previousState.Serial, "history rollback must not rewind IaC state")
				}
				previousState = state
			}
			legacyBinary := os.Getenv("IAC_LEGACY_BINARY")
			useLegacyEngine := binary == "tofu" && legacyBinary != ""
			t.Run("pre-management bundle", func(t *testing.T) {
				if useLegacyEngine {
					legacyPath, err := exec.LookPath(legacyBinary)
					require.NoError(t, err)
					base.binaryPath = legacyPath
					defer func() { base.binaryPath = binaryPath }()
				}
				deploy(t, legacyRevision, "", "", "legacy", false)
				if useLegacyEngine {
					require.Equal(t, "1.13.2", readState(t).TerraformVersion, "the upgrade must start from state written by the previously shipped engine")
				}
			})
			if useLegacyEngine {
				t.Run("previous engine saved plan requires replan without effects", func(t *testing.T) {
					work := t.TempDir()
					legacyPath, err := exec.LookPath(legacyBinary)
					require.NoError(t, err)
					run := func(path string, args ...string) ([]byte, error) {
						command := exec.CommandContext(t.Context(), path, args...)
						command.Dir = work
						return command.CombinedOutput()
					}
					configuration := filepath.Join(work, "main.tf")
					source := "resource \"terraform_data\" \"value\" { input = \"before\" }\noutput \"value\" { value = terraform_data.value.output }\n"
					require.NoError(t, os.WriteFile(configuration, []byte(source), 0o600))
					output, err := run(legacyPath, "init", "-input=false", "-no-color")
					require.NoError(t, err, "%s", output)
					output, err = run(legacyPath, "apply", "-auto-approve", "-input=false", "-no-color")
					require.NoError(t, err, "%s", output)
					statePath := filepath.Join(work, "terraform.tfstate")
					before, err := os.ReadFile(statePath)
					require.NoError(t, err)
					var previous stateSnapshot
					require.NoError(t, json.Unmarshal(before, &previous))
					require.Equal(t, "1.13.2", previous.TerraformVersion)
					require.NotEmpty(t, previous.Lineage)
					require.NoError(t, os.WriteFile(configuration, []byte(strings.ReplaceAll(source, "before", "after")), 0o600))
					output, err = run(legacyPath, "plan", "-input=false", "-no-color", "-out=previous.tfplan")
					require.NoError(t, err, "%s", output)
					output, err = run(binaryPath, "apply", "-input=false", "-no-color", "previous.tfplan")
					require.Error(t, err)
					require.Contains(t, strings.Join(strings.Fields(string(output)), " "), "plan file was created by OpenTofu or Terraform 1.13.2")
					require.Contains(t, string(output), "1.13.9-stellwerk.remediation.20261123.1")
					after, err := os.ReadFile(statePath)
					require.NoError(t, err)
					require.Equal(t, before, after, "refusing the old plan must leave state bytes unchanged")
					output, err = run(binaryPath, "output", "-json", "value")
					require.NoError(t, err, "%s", output)
					require.JSONEq(t, `"before"`, string(output))
					output, err = run(binaryPath, "plan", "-input=false", "-no-color", "-out=current.tfplan")
					require.NoError(t, err, "%s", output)
					output, err = run(binaryPath, "apply", "-input=false", "-no-color", "current.tfplan")
					require.NoError(t, err, "%s", output)
					after, err = os.ReadFile(statePath)
					require.NoError(t, err)
					var current stateSnapshot
					require.NoError(t, json.Unmarshal(after, &current))
					require.Equal(t, "1.13.9", current.TerraformVersion)
					require.Equal(t, previous.Lineage, current.Lineage)
					require.Greater(t, current.Serial, previous.Serial)
					output, err = run(binaryPath, "output", "-json", "value")
					require.NoError(t, err, "%s", output)
					require.JSONEq(t, `"after"`, string(output))
				})
			}
			t.Run("migrated v0 carry-forward", func(t *testing.T) { deploy(t, legacyRevision, "v0", "", "legacy", true) })
			t.Run("first managed v1", func(t *testing.T) {
				deploy(t, managedRevision, "v1", "1.0.0", "managed", true)
				if useLegacyEngine {
					require.Equal(t, "1.13.9", readState(t).TerraformVersion, "the patched engine must write the updated state")
				}
			})
			t.Run("managed external artifact without digest", func(t *testing.T) { deploy(t, managedRevision, "managed", "1.1.0", "managed", true) })
			t.Run("exact history rollback to v0", func(t *testing.T) { deploy(t, legacyRevision, "v0", "", "legacy", true) })
			t.Run("mounted external artifact", func(t *testing.T) { deploy(t, "mounted", "v0", "", "managed", true) })
			for _, invalid := range []struct {
				name, source, digest, code string
			}{
				{"malformed declared digest", artifact, "sha256:invalid", "ARTIFACT_MANIFEST_INVALID"},
				{"inline external digest", "inline", "sha256:" + strings.Repeat("0", 64), "ARTIFACT_MANIFEST_INVALID"},
				{"unavailable external artifact without digest", "git::" + artifactURL + "?ref=missing", "", "ARTIFACT_UNAVAILABLE"},
			} {
				t.Run(invalid.name, func(t *testing.T) {
					encoded, err := json.Marshal(artifactManifest{Version: 1, Artifacts: []artifactRequirement{{
						ModuleID: "database", Version: "1.2.0", SemanticVersion: "1.2.0", MigrationGeneration: "managed",
						Source: invalid.source, ArtifactDigest: invalid.digest,
					}}})
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(filepath.Join(root, artifactManifestName), encoded, 0o600))
					_, err = base.init(t.Context())
					var validationError *ArtifactValidationError
					require.ErrorAs(t, err, &validationError)
					require.Equal(t, invalid.code, validationError.Code())
				})
			}
		})
	}
}
