package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func noticeGoCommand(args ...string) (*exec.Cmd, error) {
	resolve := exec.Command("go", "env", "GOROOT")
	resolve.Env = append(os.Environ(), "GOTOOLCHAIN=go1.26.11", "GOWORK=off", "GOENV=off")
	output, err := resolve.Output()
	if err != nil {
		return nil, fmt.Errorf("pinned go toolchain unavailable")
	}
	return exec.Command(filepath.Join(strings.TrimSpace(string(output)), "bin/go"), args...), nil
}
