package runner

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const artifactManifestName = ".stellwerk-artifacts.json"

var artifactDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type ArtifactValidationError struct {
	ModuleID  string
	Version   string
	CodeValue string
	Reason    string
}

func (e *ArtifactValidationError) Error() string {
	return fmt.Sprintf("artifact validation failed for %s@%s: %s", e.ModuleID, e.Version, e.Reason)
}

func (e *ArtifactValidationError) Code() string { return e.CodeValue }

type artifactRequirement struct {
	ModuleID            string `json:"module_id"`
	Version             string `json:"version"`
	SemanticVersion     string `json:"semantic_version,omitempty"`
	MigrationGeneration string `json:"migration_generation,omitempty"`
	Source              string `json:"source"`
	ArtifactDigest      string `json:"artifact_digest,omitempty"`
}

type artifactManifest struct {
	Version   int                   `json:"version"`
	Artifacts []artifactRequirement `json:"artifacts"`
}

type downloadedModuleManifest struct {
	Modules []struct {
		Key     string `json:"Key"`
		Source  string `json:"Source"`
		Version string `json:"Version"`
		Dir     string `json:"Dir"`
	} `json:"Modules"`
}

func verifyArtifactManifest(root string) error {
	encoded, err := os.ReadFile(filepath.Join(root, artifactManifestName))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var manifest artifactManifest
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		return fmt.Errorf("decode module artifact manifest: %w", err)
	}
	if manifest.Version != 1 {
		return fmt.Errorf("unsupported module artifact manifest version %d", manifest.Version)
	}

	var downloaded downloadedModuleManifest
	modulesJSON, err := os.ReadFile(filepath.Join(root, ".terraform", "modules", "modules.json"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(modulesJSON, &downloaded); err != nil {
			return fmt.Errorf("decode downloaded module manifest: %w", err)
		}
	}

	for _, requirement := range manifest.Artifacts {
		// Preserve the Control Plane's historical identity without fabricating
		// SemVer or digest metadata. New managed claims are optional as well.
		legacy := requirement.MigrationGeneration == "v0"
		if legacy && (requirement.SemanticVersion != "" || requirement.ArtifactDigest != "") {
			return &ArtifactValidationError{
				ModuleID: requirement.ModuleID, Version: requirement.Version,
				CodeValue: "ARTIFACT_MANIFEST_INVALID", Reason: "migrated v0 must not declare invented SemVer or artifact digest metadata",
			}
		}
		if requirement.Source == "inline" {
			if requirement.ArtifactDigest != "" {
				return &ArtifactValidationError{
					ModuleID:  requirement.ModuleID,
					Version:   requirement.Version,
					CodeValue: "ARTIFACT_MANIFEST_INVALID",
					Reason:    "inline source must not declare an external artifact digest",
				}
			}
			continue
		}
		// Version-1 bundles historically encoded an absent claim as "". Keep
		// those retained bundles readable; publication rejects explicit empty
		// claims and current producers omit the field. This is format validation,
		// not the deferred trusted verification of downloaded artifact content.
		if requirement.ArtifactDigest != "" && !artifactDigestPattern.MatchString(requirement.ArtifactDigest) {
			return &ArtifactValidationError{
				ModuleID:  requirement.ModuleID,
				Version:   requirement.Version,
				CodeValue: "ARTIFACT_MANIFEST_INVALID",
				Reason:    "external artifact digest must be sha256 followed by 64 lowercase hexadecimal characters",
			}
		}
		directories := artifactDirectories(root, requirement, downloaded)
		if len(directories) == 0 {
			return &ArtifactValidationError{
				ModuleID: requirement.ModuleID, Version: requirement.Version,
				CodeValue: "ARTIFACT_UNAVAILABLE", Reason: "artifact was not downloaded by the IaC initializer",
			}
		}
	}
	return nil
}

func artifactDirectories(root string, requirement artifactRequirement, downloaded downloadedModuleManifest) []string {
	source, version := splitRegistrySource(requirement.Source)
	// IaC initializers record absolute, mounted module paths as file URLs.
	if filepath.IsAbs(source) {
		source = (&url.URL{Scheme: "file", Path: filepath.ToSlash(source)}).String()
	}
	result := make([]string, 0)
	for _, module := range downloaded.Modules {
		if module.Key == "" || module.Source != source || (version != "" && module.Version != version) {
			continue
		}
		directory := filepath.FromSlash(module.Dir)
		if !filepath.IsAbs(directory) {
			directory = filepath.Join(root, directory)
		}
		if info, err := os.Stat(directory); err == nil && info.IsDir() {
			result = append(result, directory)
		}
	}
	slices.Sort(result)
	return result
}

func splitRegistrySource(source string) (string, string) {
	index := strings.LastIndex(source, "@")
	if index < 0 || strings.HasPrefix(source, "/") || strings.HasPrefix(source, "git::") {
		return source, ""
	}
	return source[:index], source[index+1:]
}
