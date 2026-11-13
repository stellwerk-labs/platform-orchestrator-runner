// Command noticecheck validates the production notice closure against real Go inputs.
package main

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

func main() {
	if len(os.Args) == 4 && os.Args[1] == "infracost-archive" {
		if err := archiveInfracostSources(os.Args[2], os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if len(os.Args) == 5 && os.Args[1] == "prepare-infracost" {
		if err := prepareInfracostPatches(os.Args[2], os.Args[3], os.Args[4]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 7 && os.Args[1] == "infracost" {
		if err := collectInfracost(os.Args[2], os.Args[3], os.Args[4], os.Args[5], os.Args[6]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: noticecheck BUNDLE SOURCE_ROOT amd64|arm64")
		os.Exit(2)
	}
	if err := check(os.Args[1], os.Args[2], os.Args[3]); err != nil {
		fmt.Fprintln(os.Stderr, "notice validation rejected:", err)
		os.Exit(1)
	}
}
func hash(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }
func gitBlob(data []byte) string {
	h := sha1.New()
	_, _ = fmt.Fprintf(h, "blob %d%c", len(data), 0)
	h.Write(data)
	return fmt.Sprintf("%x", h.Sum(nil))
}
func regular(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("required regular notice missing")
	}
	return os.ReadFile(path)
}
func same(first, second string) error {
	a, err := regular(first)
	if err != nil {
		return err
	}
	b, err := regular(second)
	if err != nil {
		return err
	}
	if !bytes.Equal(a, b) {
		return fmt.Errorf("notice differs from authoritative input")
	}
	return nil
}
func command(root, arch string, args ...string) ([]byte, error) {
	cmd, err := noticeGoCommand(args...)
	if err != nil {
		return nil, err
	}
	cmd.Dir = root
	env := os.Environ()
	for _, item := range []string{"GOOS=linux", "GOARCH=" + arch, "CGO_ENABLED=0", "GOWORK=off", "GOENV=off", "GOTOOLCHAIN=local", "GOFLAGS=", "GOEXPERIMENT="} {
		key := strings.SplitN(item, "=", 2)[0] + "="
		filtered := env[:0]
		for _, old := range env {
			if !strings.HasPrefix(old, key) {
				filtered = append(filtered, old)
			}
		}
		env = append(filtered, item)
	}
	cmd.Env = env
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("authoritative Go metadata failed")
	}
	return data, nil
}

// Provenance has exactly seven string fields. Reject duplicate/unknown keys and trailing JSON.
func receipt(data []byte) (map[string]string, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("invalid provenance JSON")
	}
	result := map[string]string{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, fmt.Errorf("invalid provenance key")
		}
		if _, ok = result[name]; ok {
			return nil, fmt.Errorf("duplicate provenance key")
		}
		var value string
		if err := d.Decode(&value); err != nil {
			return nil, err
		}
		result[name] = value
	}
	if _, err = d.Token(); err != nil {
		return nil, err
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing provenance JSON")
	}
	return result, nil
}
func check(bundle, root, arch string) error {
	if err := checkInfracost(bundle, root, arch); err != nil {
		return err
	}
	if arch != "amd64" && arch != "arm64" {
		return fmt.Errorf("unsupported target")
	}
	if runtime.Version() != "go1.26.11" {
		return fmt.Errorf("unexpected notice verifier compiler")
	}
	goCommand, err := noticeGoCommand()
	if err != nil {
		return err
	}
	goroot := filepath.Dir(filepath.Dir(goCommand.Path))
	for target, source := range map[string]string{"application/LICENSE": filepath.Join(root, "LICENSE"), "go/LICENSE": filepath.Join(goroot, "LICENSE"), "go/PATENTS": filepath.Join(goroot, "PATENTS"), "opentofu/SOURCE.json": filepath.Join(root, "scripts/release-notices/opentofu-source.json")} {
		if err := same(filepath.Join(bundle, target), source); err != nil {
			return err
		}
	}
	source, err := regular(filepath.Join(bundle, "opentofu/SOURCE.json"))
	if err != nil {
		return err
	}
	var vendor struct {
		Version      string            `json:"version"`
		SourceCommit string            `json:"sourceCommit"`
		SourceURL    string            `json:"sourceURL"`
		NoticeSHA256 map[string]string `json:"noticeSHA256"`
	}
	if json.Unmarshal(source, &vendor) != nil || vendor.Version != "1.13.2" || vendor.SourceCommit != "765062032c46cacab116b36d714db098b806dede" || vendor.SourceURL != "https://github.com/opentofu/opentofu/tree/"+vendor.SourceCommit || len(vendor.NoticeSHA256) != 3 {
		return fmt.Errorf("invalid vendor source binding")
	}
	for _, name := range []string{"LICENSE", "README.md", "CHANGELOG.md"} {
		data, err := regular(filepath.Join(bundle, "opentofu", name))
		if err != nil || hash(data) != vendor.NoticeSHA256[name] {
			return fmt.Errorf("vendor notice differs from authenticated archive")
		}
	}
	target, err := regular(filepath.Join(bundle, "target.txt"))
	if err != nil || string(target) != "linux/"+arch+"\n" {
		return fmt.Errorf("wrong notice target")
	}
	graph, err := command(root, arch, "list", "-mod=readonly", "-deps", "-f", "{{with .Module}}{{.Path}}|{{.Version}}|{{.Dir}}{{end}}", "./cmd/runner")
	if err != nil {
		return err
	}
	modules := map[string]string{}
	dirs := map[string]string{}
	for _, line := range strings.Split(string(graph), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) != 3 {
			return fmt.Errorf("invalid Go graph")
		}
		modules[fields[0]] = fields[1]
		dirs[fields[0]] = fields[2]
	}
	var expected []string
	for name, version := range modules {
		expected = append(expected, name+"|"+version)
	}
	sort.Strings(expected)
	listed, err := regular(filepath.Join(bundle, "modules.txt"))
	if err != nil || string(listed) != strings.Join(expected, "\n")+"\n" {
		return fmt.Errorf("production module closure differs")
	}
	declarations, err := os.ReadFile(filepath.Join(root, "scripts/declared-licenses.tsv"))
	if err != nil {
		return err
	}
	declared := map[string][]string{}
	for _, line := range strings.Split(string(declarations), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) != 4 || declared[fields[0]] != nil || fields[1] != "EUPL-1.2" || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(fields[2]) || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(fields[3]) {
			return fmt.Errorf("invalid license declaration")
		}
		declared[fields[0]] = fields[1:]
	}
	sums, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		return err
	}
	for module, version := range modules {
		if module == "github.com/stellwerk-labs/platform-orchestrator-runner" {
			continue
		}
		found := false
		err := filepath.WalkDir(filepath.Join(bundle, module), func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlinked module notice")
			}
			name := strings.ToUpper(entry.Name())
			if entry.Type().IsRegular() && (strings.HasPrefix(name, "LICENSE") || strings.HasPrefix(name, "LICENCE") || strings.HasPrefix(name, "COPYING")) {
				found = true
			}
			return nil
		})
		if err != nil || !found {
			return fmt.Errorf("production dependency notice missing")
		}
		declaration, ok := declared[module]
		if !ok {
			continue
		}
		license, err := regular(filepath.Join(bundle, module, "LICENSE"))
		if err != nil || (hash(license) != declaration[1] || gitBlob(license) != declaration[2]) {
			return fmt.Errorf("declared license bytes differ")
		}
		if err := same(filepath.Join(bundle, module, "LICENSE"), filepath.Join(dirs[module], "LICENSE")); err != nil {
			return err
		}
		metadata, err := command(root, arch, "mod", "download", "-json", module+"@"+version)
		if err != nil {
			return err
		}
		var origin struct {
			Path, Version, Sum string
			Replace            any
			Origin             *struct{ Hash string }
		}
		if json.Unmarshal(metadata, &origin) != nil || origin.Path != module || origin.Version != version || origin.Replace != nil || origin.Origin == nil || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(origin.Origin.Hash) || !regexp.MustCompile(`^h1:[A-Za-z0-9+/]{43}=$`).MatchString(origin.Sum) || !strings.Contains("\n"+string(sums), "\n"+module+" "+version+" "+origin.Sum+"\n") {
			return fmt.Errorf("unverified declared module provenance")
		}
		data, err := regular(filepath.Join(bundle, module, "LICENSE.provenance"))
		if err != nil {
			return err
		}
		actual, err := receipt(data)
		if err != nil {
			return err
		}
		want := map[string]string{"module": module, "version": version, "sourceCommit": origin.Origin.Hash, "moduleSum": origin.Sum, "licenseBlob": declaration[2], "licenseSHA256": declaration[1], "spdx": declaration[0]}
		if len(actual) != len(want) {
			return fmt.Errorf("provenance field set differs")
		}
		for key, value := range want {
			if actual[key] != value {
				return fmt.Errorf("declared module provenance differs")
			}
		}
	}
	return nil
}
