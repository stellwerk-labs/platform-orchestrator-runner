package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const infracostPatchHash = "c75b4b058e2820aa2e90c40285280b7c0f6a48c994546b68fba7c00b53ff08ed"

type infracostPatch struct {
	Module, Version, Path string
	BeforeSHA256          string `json:"beforeSHA256"`
	AfterSHA256           string `json:"afterSHA256"`
	Replacements          int
	Edits                 []struct{ Before, After string } `json:"edits"`
}
type infracostPatches struct {
	SourceCommit string           `json:"sourceCommit"`
	ModSHA256    string           `json:"modSHA256"`
	SumSHA256    string           `json:"sumSHA256"`
	Files        []infracostPatch `json:"files"`
}

func infracostGo(source, cache, proxy string, args ...string) ([]byte, error) {
	cmd, err := noticeGoCommand(args...)
	if err != nil {
		return nil, err
	}
	cmd.Dir = source
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOWORK=off", "GOENV=off", "GOMAXPROCS=2", "GOFLAGS=-p=2")
	if cache != "" {
		cmd.Env = append(cmd.Env, "GOMODCACHE="+cache, "GOPROXY="+proxy)
	}
	data, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("infracost build input command failed: %s", data)
	}
	return data, nil
}

func readInfracostPatches(root string) (infracostPatches, error) {
	var receipt infracostPatches
	data, err := regular(filepath.Join(root, "scripts/infracost-patches/PATCHES.json"))
	if err != nil || hash(data) != infracostPatchHash || json.Unmarshal(data, &receipt) != nil || receipt.SourceCommit != infracostCommit || len(receipt.Files) != 4 {
		return receipt, fmt.Errorf("infracost compatibility receipt differs")
	}
	return receipt, nil
}

func installInfracostModuleInputs(root, source string, receipt infracostPatches) error {
	for name, want := range map[string]string{"go.mod": receipt.ModSHA256, "go.sum": receipt.SumSHA256} {
		blob, err := regular(filepath.Join(root, "scripts/infracost-patches", name))
		if err != nil || hash(blob) != want {
			return fmt.Errorf("patched module input differs")
		}
		if err := os.WriteFile(filepath.Join(source, name), blob, 0644); err != nil {
			return err
		}
	}
	return nil
}

func applyInfracostPatch(source, cache, proxy string, patch infracostPatch) error {
	metadata, err := infracostGo(source, cache, proxy, "list", "-mod=readonly", "-m", "-json", patch.Module)
	if err != nil {
		return err
	}
	var module struct{ Path, Version, Dir string }
	if json.Unmarshal(metadata, &module) != nil || module.Path != patch.Module || module.Version != patch.Version {
		return fmt.Errorf("patch module identity differs")
	}
	path := filepath.Join(module.Dir, patch.Path)
	relative, err := filepath.Rel(cache, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return fmt.Errorf("patch outside private module cache")
	}
	before, err := regular(path)
	if err != nil || hash(before) != patch.BeforeSHA256 {
		return fmt.Errorf("patch original bytes differ")
	}
	after, err := patchedInfracostBytes(before, patch)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0644); err != nil {
		return err
	}
	return os.WriteFile(path, after, 0644)
}

func prepareInfracostPatches(output, source, root string) error {
	receipt, err := readInfracostPatches(root)
	if err != nil {
		return err
	}
	if err := os.Mkdir(output, 0755); err != nil {
		return err
	}
	output, err = filepath.EvalSymlinks(output)
	if err != nil {
		return err
	}
	privateSource := filepath.Join(output, "source")
	// The archive is authenticated before copying; preserve upstream fixture links.
	if err := exec.Command("cp", "-R", source, privateSource).Run(); err != nil {
		return err
	}
	if err := installInfracostModuleInputs(root, privateSource, receipt); err != nil {
		return err
	}
	shared, err := infracostGo(source, "", "", "env", "GOMODCACHE")
	if err != nil {
		return err
	}
	proxyURL := url.URL{Scheme: "file", Path: filepath.Join(strings.TrimSpace(string(shared)), "cache/download")}
	proxy := proxyURL.String() + ",https://proxy.golang.org"
	cache := filepath.Join(output, "modules")
	// Authenticate immutable module archives before the explicit private patches.
	if _, err := infracostGo(privateSource, cache, proxy, "mod", "download"); err != nil {
		return err
	}
	if _, err := infracostGo(privateSource, cache, proxy, "mod", "verify"); err != nil {
		return err
	}
	for _, patch := range receipt.Files {
		if err := applyInfracostPatch(privateSource, cache, proxy, patch); err != nil {
			return err
		}
	}
	// After patching, exact after-hashes replace mod verify for those four files.
	_, err = infracostGo(privateSource, cache, proxy, "list", "-mod=readonly", "-deps", "./cmd/infracost")
	return err
}

func patchedInfracostBytes(before []byte, patch infracostPatch) ([]byte, error) {
	old := []byte("golang.org/x/crypto/openpgp")
	if bytes.Count(before, old) != patch.Replacements {
		return nil, fmt.Errorf("patch import count differs")
	}
	after := bytes.ReplaceAll(before, old, []byte("github.com/ProtonMail/go-crypto/openpgp"))
	for _, edit := range patch.Edits {
		if bytes.Count(after, []byte(edit.Before)) != 1 {
			return nil, fmt.Errorf("signature compatibility precondition differs")
		}
		after = bytes.ReplaceAll(after, []byte(edit.Before), []byte(edit.After))
	}
	if hash(after) != patch.AfterSHA256 {
		return nil, fmt.Errorf("patched bytes differ")
	}
	return after, nil
}
