package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpentofuAcquisitionDirectoryDoesNotInheritParentModule(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parentMod := filepath.Join(parent, "go.mod")
	before := []byte("module example.invalid/parent\n\ngo 1.26.11\n")
	if err := os.WriteFile(parentMod, before, 0600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(parent, "scratch-parent")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", nested)
	gomod := func(dir string) string {
		command := exec.CommandContext(t.Context(), "go", "env", "GOMOD")
		command.Dir = dir
		command.Env = append(os.Environ(), "GOWORK=off", "GOENV=off", "GOTOOLCHAIN=local", "GOROOT=", "GOPROXY=off", "GOFLAGS=", "GOEXPERIMENT=")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("real Go GOMOD query failed: %v: %s", err, output)
		}
		return strings.TrimSpace(string(output))
	}
	if actual := gomod(nested); actual != parentMod {
		t.Fatalf("nested directory should inherit the parent module, got %q", actual)
	}
	acquisition, err := opentofuAcquisitionDirectory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(acquisition) })
	if filepath.Dir(acquisition) != nested {
		t.Fatalf("acquisition did not exercise nested TMPDIR: %q", acquisition)
	}
	if actual := gomod(acquisition); actual != filepath.Join(acquisition, "go.mod") {
		t.Fatalf("acquisition inherited another module: %q", actual)
	}
	after, err := os.ReadFile(parentMod)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("parent go.mod changed")
	}
	if _, err := os.Stat(filepath.Join(parent, "go.sum")); !os.IsNotExist(err) {
		t.Fatal("parent go.sum was created")
	}
}
