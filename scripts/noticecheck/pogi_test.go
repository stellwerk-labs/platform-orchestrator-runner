package main

import (
	"os"
	"path/filepath"
	"testing"
)

func copyPogiFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	source := filepath.Join(root, "internal/thirdparty/pogi")
	upstream := filepath.Join("..", "..", "internal", "thirdparty", "pogi")
	err := filepath.WalkDir(upstream, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(upstream, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(source, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, data, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "pogi"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"LICENSE", "SOURCE.json", "pogi.spdx.json"} {
		data, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bundle, "pogi", name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root, bundle
}
func TestPogiSourceAndArtifactClosure(t *testing.T) {
	for _, kind := range []string{"valid", "source", "manifest", "licence", "sbom", "extra_source", "missing_artifact", "changed_artifact", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root, bundle := copyPogiFixture(t)
			source := filepath.Join(root, "internal/thirdparty/pogi")
			mutatePogiFixture(t, kind, source, bundle)
			err := checkPogi(bundle, root)
			if kind == "valid" && err != nil {
				t.Fatal(err)
			}
			if kind != "valid" && err == nil {
				t.Fatal("tamper accepted")
			}
		})
	}
}

func mutatePogiFixture(t *testing.T, kind, source, bundle string) {
	t.Helper()
	path := map[string]string{
		"source": filepath.Join(source, "cost/aws.go"), "manifest": filepath.Join(source, "SOURCE.json"),
		"licence": filepath.Join(source, "LICENSE"), "sbom": filepath.Join(source, "pogi.spdx.json"),
		"extra_source": filepath.Join(source, "cost/unreviewed.go"), "changed_artifact": filepath.Join(bundle, "pogi/pogi.spdx.json"),
	}[kind]
	if kind == "missing_artifact" {
		if err := os.Remove(filepath.Join(bundle, "pogi/LICENSE")); err != nil {
			t.Fatal(err)
		}
	}
	if kind == "symlink" {
		if err := os.Symlink(filepath.Join(source, "LICENSE"), filepath.Join(source, "linked")); err != nil {
			t.Fatal(err)
		}
	}

	if path != "" {
		if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
