package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const pogiSourceHash = "1a7de1e55c216535392b227276c71fc94ca39268959c7619e9848bb05d669176"
const pogiSBOMHash = "bb288432ad8370fd53c08bdf2bedde10121f07edd774c6778b628df2c2f632cf"
const pogiLicenseHash = "3243ac7affae1bc6a42a7c6806e51a361fa77f91859a03425a48710ae514f34e"

// The manifest digest pins the full selected upstream closure, including original and adapted bytes.
// It must change deliberately when the source pin or the sole import rewrite changes.
func checkPogi(bundle, root string) error {
	sourceRoot := filepath.Join(root, "internal/thirdparty/pogi")
	data, err := regular(filepath.Join(sourceRoot, "SOURCE.json"))
	if err != nil || hash(data) != pogiSourceHash {
		return fmt.Errorf("Pogi source manifest differs from reviewed pin")
	}
	var manifest struct {
		Files []struct {
			Upstream string `json:"upstream"`
			Path     string `json:"path"`
			Original string `json:"originalSHA256"`
			Adapted  string `json:"adaptedSHA256"`
		} `json:"files"`
	}
	if json.Unmarshal(data, &manifest) != nil || len(manifest.Files) != 11 {
		return fmt.Errorf("invalid Pogi source closure")
	}
	expected := map[string]bool{"SOURCE.json": true, "pogi.spdx.json": true}
	for _, file := range manifest.Files {
		actual, err := regular(filepath.Join(sourceRoot, file.Path))
		if err != nil || hash(actual) != file.Adapted {
			return fmt.Errorf("Pogi adapted source differs")
		}
		original := bytes.ReplaceAll(actual, []byte("github.com/stellwerk-labs/platform-orchestrator-runner/internal/thirdparty/pogi/terraform"), []byte("github.com/nrndev/pogi/internal/terraform"))
		if hash(original) != file.Original {
			return fmt.Errorf("Pogi upstream source differs")
		}
		expected[file.Path] = true
	}
	if err := checkPogiFileClosure(sourceRoot, expected); err != nil {
		return err
	}
	return checkPogiNotices(bundle, sourceRoot)
}

func checkPogiFileClosure(sourceRoot string, expected map[string]bool) error {
	return filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlinked Pogi source")
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(sourceRoot, path)
		if err != nil || !expected[filepath.ToSlash(relative)] {
			return fmt.Errorf("unexpected Pogi source")
		}
		return nil
	})
}

func checkPogiNotices(bundle, sourceRoot string) error {
	for name, want := range map[string]string{"SOURCE.json": pogiSourceHash, "LICENSE": pogiLicenseHash, "pogi.spdx.json": pogiSBOMHash} {
		input, err := regular(filepath.Join(sourceRoot, name))
		if err != nil || hash(input) != want {
			return fmt.Errorf("Pogi source notice differs")
		}
		if err := same(filepath.Join(bundle, "pogi", name), filepath.Join(sourceRoot, name)); err != nil {
			return err
		}
	}
	return nil
}
