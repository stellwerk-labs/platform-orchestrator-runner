package main

import (
	"bytes"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
)

const infracostCommit = "40ad0ac9f247d883884cd0f60057367b4af3df08"
const infracostLicenseHash = "de34de041a08b4396b411d3c173132bbcafe902f90307cca5cc00aae00ae6bad"
const infracostSourceHash = "e03529cfa4a1803cef8748fff0a15d65785dfe12917b3f0096042174afe5f81f"
const infracostBuildVersion = "v0.10.49+stellwerk.1"
const infracostModulePath = "github.com/infracost/infracost"

type infracostModule struct {
	Path, Version, Dir, Sum string
	Replace                 *infracostModule
}
type infracostPackage struct {
	Dir    string
	Module *infracostModule
}

func infracostTargetGraph(buildDir, arch string) ([]infracostPackage, error) {
	cmd, err := noticeGoCommand("list", "-mod=readonly", "-deps", "-json", "./cmd/infracost")
	if err != nil {
		return nil, err
	}
	cmd.Dir = filepath.Join(buildDir, "source")
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOWORK=off", "GOENV=off", "GOFLAGS=-p=2", "GOEXPERIMENT=", "GOMAXPROCS=2", "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch, "GOMODCACHE="+filepath.Join(buildDir, "modules"))
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("infracost target graph unavailable")
	}
	sums, err := regular(filepath.Join(buildDir, "source/go.sum"))
	if err != nil {
		return nil, err
	}
	checksums := map[string]string{}
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 {
			checksums[fields[0]+" "+fields[1]] = fields[2]
		}
	}
	packages := []infracostPackage{}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	for decoder.More() {
		var pkg infracostPackage
		if err := decoder.Decode(&pkg); err != nil {
			return nil, err
		}
		if pkg.Module != nil {
			fillInfracostChecksums(pkg.Module, checksums)
			packages = append(packages, pkg)
		}
	}
	return packages, nil
}

func infracostSourceFiles(source, buildDir, root string) (map[string][]byte, error) {
	files := map[string][]byte{}
	for name, want := range map[string]string{"LICENSE": infracostLicenseHash, "go.mod": "7af651dda192024bc02ea43a3c74928093f761bd749774425f928c71dbce35e7", "go.sum": "28dad663d9683a319fddd105025db5f75457e39db4108b80eb9e98d68ce8da93"} {
		blob, err := regular(filepath.Join(source, name))
		if err != nil || hash(blob) != want {
			return nil, fmt.Errorf("infracost source pin differs")
		}
		files[name] = blob
	}
	receipt, err := regular(filepath.Join(root, "scripts/release-notices/infracost-source.json"))
	if err != nil || hash(receipt) != infracostSourceHash {
		return nil, fmt.Errorf("infracost source receipt differs")
	}
	files["SOURCE.json"] = receipt
	for _, name := range []string{"go.mod", "go.sum", "PATCHES.json"} {
		path := filepath.Join(root, "scripts/infracost-patches", name)
		blob, err := regular(path)
		if err != nil {
			return nil, err
		}
		files[filepath.Join("build", name)] = blob
		if name != "PATCHES.json" {
			if err := same(path, filepath.Join(buildDir, "source", name)); err != nil {
				return nil, err
			}
		}
	}
	notice, err := regular(filepath.Join(root, "scripts/release-notices/infracost-corresponding-source.txt"))
	if err != nil {
		return nil, err
	}
	files["SOURCE-AVAILABILITY.txt"] = notice
	return files, nil
}

func infracostPatchFiles(packages []infracostPackage, root string, files map[string][]byte) error {
	receipt, err := readInfracostPatches(root)
	if err != nil {
		return err
	}
	modules := map[string]*infracostModule{}
	for _, pkg := range packages {
		modules[pkg.Module.Path] = pkg.Module
	}
	for _, patch := range receipt.Files {
		module := modules[patch.Module]
		if module == nil || module.Version != patch.Version {
			return fmt.Errorf("patched dependency identity differs")
		}
		blob, err := regular(filepath.Join(module.Dir, patch.Path))
		if err != nil || hash(blob) != patch.AfterSHA256 {
			return fmt.Errorf("patched dependency bytes differ")
		}
		files[filepath.Join("build/patches", patch.Module, patch.Path)] = blob
	}
	return nil
}

func infracostModuleIdentity(module *infracostModule) string {
	row := module.Path + "|" + module.Version + "|" + module.Sum
	if module.Replace != nil {
		return row + "|" + infracostModuleIdentity(module.Replace)
	}
	return row
}
func infracostDebugModule(module *debug.Module) *infracostModule {
	result := &infracostModule{Path: module.Path, Version: module.Version, Sum: module.Sum}
	if module.Replace != nil {
		result.Replace = infracostDebugModule(module.Replace)
	}
	return result
}

func verifyInfracostBuild(binary, arch string, packages []infracostPackage) ([]byte, error) {
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		return nil, err
	}
	if info.GoVersion != "go1.26.11" || info.Path != infracostModulePath+"/cmd/infracost" || info.Main.Path != infracostModulePath {
		return nil, fmt.Errorf("unexpected Infracost executable identity")
	}
	settings := map[string]string{}
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	for key, want := range map[string]string{"GOOS": "linux", "GOARCH": arch, "CGO_ENABLED": "0", "-trimpath": "true"} {
		if settings[key] != want {
			return nil, fmt.Errorf("unexpected Infracost executable settings")
		}
	}
	expected := map[string]string{}
	for _, pkg := range packages {
		if pkg.Module.Path != infracostModulePath {
			expected[pkg.Module.Path] = infracostModuleIdentity(pkg.Module)
		}
	}
	actual := map[string]string{}
	for _, module := range info.Deps {
		actual[module.Path] = infracostModuleIdentity(infracostDebugModule(module))
	}
	if !maps.Equal(actual, expected) {
		return nil, fmt.Errorf("infracost executable module graph differs")
	}
	return []byte(info.String()), nil
}

func infracostNoticeFile(path, moduleDir, modulePath string, files map[string][]byte) (bool, error) {
	stat, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if stat.IsDir() {
		return false, nil
	}
	blob, err := regular(path)
	if err != nil {
		return false, err
	}
	relative, err := filepath.Rel(moduleDir, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return false, fmt.Errorf("notice outside module")
	}
	files[filepath.Join("dependencies", modulePath, relative)] = blob
	name := strings.ToUpper(filepath.Base(path))
	return strings.HasPrefix(name, "LICENSE") || strings.HasPrefix(name, "LICENCE") || strings.HasPrefix(name, "COPYING"), nil
}
func infracostLegalDirectory(dir, moduleDir, modulePath string, files map[string][]byte) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	found := false
	for _, entry := range entries {
		name := strings.ToUpper(entry.Name())
		if !infracostLegalName(name) {
			continue
		}
		license, err := infracostNoticeFile(filepath.Join(dir, entry.Name()), moduleDir, modulePath, files)
		if err != nil {
			return false, err
		}
		found = found || license
	}
	return found, nil
}
func infracostLegalName(name string) bool {
	return strings.HasPrefix(name, "LICENSE") || strings.HasPrefix(name, "LICENCE") || strings.HasPrefix(name, "COPYING") || strings.HasPrefix(name, "NOTICE") || strings.HasPrefix(name, "PATENTS")
}

func infracostPackageNotices(pkg infracostPackage, files map[string][]byte) error {
	moduleDir := pkg.Module.Dir
	if pkg.Module.Replace != nil {
		moduleDir = pkg.Module.Replace.Dir
	}
	if moduleDir == "" {
		return fmt.Errorf("dependency source directory unavailable")
	}
	found := false
	for dir := pkg.Dir; ; dir = filepath.Dir(dir) {
		next, err := infracostLegalDirectory(dir, moduleDir, pkg.Module.Path, files)
		if err != nil {
			return err
		}
		found = found || next
		if dir == moduleDir {
			break
		}
		if filepath.Dir(dir) == dir {
			return fmt.Errorf("package outside module")
		}
	}
	if !found {
		return infracostInlineLicense(pkg, files)
	}
	return nil
}

func infracostInventory(packages []infracostPackage, arch string) ([]byte, []byte, error) {
	modules := map[string]*infracostModule{}
	for _, pkg := range packages {
		modules[pkg.Module.Path] = pkg.Module
	}
	rows := []string{}
	for _, module := range modules {
		rows = append(rows, infracostModuleIdentity(module))
	}
	sort.Strings(rows)
	components := []map[string]any{}
	relationships := []map[string]string{}
	for i, row := range rows {
		fields := strings.Split(row, "|")
		name, version, sum := fields[0], fields[1], fields[2]
		if len(fields) == 6 {
			name, version, sum = fields[3], fields[4], fields[5]
		}
		if name == infracostModulePath {
			version = infracostBuildVersion
			sum = infracostCommit + "; patch=" + infracostPatchHash
		}
		id := fmt.Sprintf("SPDXRef-InfracostModule-%d", i)
		components = append(components, map[string]any{"SPDXID": id, "name": name, "versionInfo": version, "downloadLocation": "NOASSERTION", "filesAnalyzed": false, "licenseConcluded": "NOASSERTION", "licenseDeclared": "NOASSERTION", "copyrightText": "NOASSERTION", "sourceInfo": "checksum/source=" + sum + "; importModule=" + fields[0]})
		relationships = append(relationships, map[string]string{"spdxElementId": "SPDXRef-DOCUMENT", "relationshipType": "DESCRIBES", "relatedSpdxElement": id})
	}
	document := map[string]any{"spdxVersion": "SPDX-2.3", "dataLicense": "CC0-1.0", "SPDXID": "SPDXRef-DOCUMENT", "name": "Infracost " + infracostBuildVersion + " linux/" + arch, "documentNamespace": "https://stellwerk.dev/spdx/infracost-" + infracostCommit + "-" + infracostPatchHash + "-" + arch, "creationInfo": map[string]any{"creators": []string{"Tool: Stellwerk production notice collector"}, "created": "2026-11-13T00:00:00Z"}, "packages": components, "relationships": relationships}
	sbom, err := json.MarshalIndent(document, "", "  ")
	return []byte(strings.Join(rows, "\n") + "\n"), sbom, err
}

func infracostInputs(source, arch, binary, root string) (map[string][]byte, error) {
	buildDir := os.Getenv("INFRACOST_BUILD_DIR")
	if buildDir == "" {
		return nil, fmt.Errorf("private Infracost build inputs required")
	}
	files, err := infracostSourceFiles(source, buildDir, root)
	if err != nil {
		return nil, err
	}
	packages, err := infracostTargetGraph(buildDir, arch)
	if err != nil {
		return nil, err
	}
	if err := infracostPatchFiles(packages, root, files); err != nil {
		return nil, err
	}
	files["buildinfo.txt"], err = verifyInfracostBuild(binary, arch, packages)
	if err != nil {
		return nil, err
	}
	binaryBytes, err := regular(binary)
	if err != nil {
		return nil, err
	}
	files["binary.sha256"] = []byte(hash(binaryBytes) + "\n")
	if err := collectInfracostDeclarations(packages, files); err != nil {
		return nil, err
	}
	for _, pkg := range packages {
		if err := infracostPackageNotices(pkg, files); err != nil {
			return nil, err
		}
	}
	files["modules.txt"], files["infracost.spdx.json"], err = infracostInventory(packages, arch)
	return files, err
}

func collectInfracost(output, source, arch, binary, root string) error {
	files, err := infracostInputs(source, arch, binary, root)
	if err != nil {
		return err
	}
	for name, data := range files {
		path := filepath.Join(output, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			return err
		}
	}
	return nil
}
func checkInfracost(bundle, root, arch string) error {
	source, binary := os.Getenv("INFRACOST_SOURCE_DIR"), os.Getenv("INFRACOST_BINARY")
	if source == "" || binary == "" {
		return fmt.Errorf("infracost authoritative source and binary required")
	}
	files, err := infracostInputs(source, arch, binary, root)
	if err != nil {
		return err
	}
	for name, want := range files {
		got, err := regular(filepath.Join(bundle, "infracost", name))
		if err != nil || !bytes.Equal(got, want) {
			return fmt.Errorf("infracost notice closure differs: %s", name)
		}
	}
	return verifyInfracostSourceArchive(bundle, root, arch)
}

func fillInfracostChecksums(module *infracostModule, sums map[string]string) {
	if module.Sum == "" {
		module.Sum = sums[module.Path+" "+module.Version]
	}
	if module.Replace != nil {
		fillInfracostChecksums(module.Replace, sums)
	}
}

type infracostDeclaration struct{ Version, File, SHA256, SPDX string }

var infracostDeclaredLicenses = map[string]infracostDeclaration{
	"go.mozilla.org/gopgagent":        {"v0.0.0-20170926210634-4d7ea76ff71a", "gpgagent.go", "4b3b1c8db16faa3ccae3ff160fece31798b12da04388a203fa0fa8679ddd815e", "Apache-2.0"},
	"github.com/jmespath/go-jmespath": {"v0.4.0", "LICENSE", "03cfaf331a260694d06227a589cdd2b5fcfab474697b72e250736696c313655a", "Apache-2.0"},
}

func infracostDeclaredSource(module *infracostModule) (infracostDeclaration, []byte, error) {
	declaration, ok := infracostDeclaredLicenses[module.Path]
	if !ok || module.Version != declaration.Version {
		return declaration, nil, fmt.Errorf("undeclared inline dependency licence: %s", module.Path)
	}
	blob, err := regular(filepath.Join(module.Dir, declaration.File))
	if err != nil || hash(blob) != declaration.SHA256 {
		return declaration, nil, fmt.Errorf("declared licence source bytes differ")
	}
	return declaration, blob, nil
}
func infracostInlineLicense(pkg infracostPackage, files map[string][]byte) error {
	_, _, err := infracostDeclaredSource(pkg.Module)
	return err
}
func collectInfracostDeclarations(packages []infracostPackage, files map[string][]byte) error {
	modules := map[string]*infracostModule{}
	for _, pkg := range packages {
		modules[pkg.Module.Path] = pkg.Module
	}
	terms := files["LICENSE"]
	marker := []byte("END OF TERMS AND CONDITIONS")
	end := bytes.Index(terms, marker)
	if end < 0 {
		return fmt.Errorf("apache terms unavailable")
	}
	terms = append(bytes.Clone(terms[:end+len(marker)]), '\n')
	for path := range infracostDeclaredLicenses {
		module := modules[path]
		if module == nil {
			return fmt.Errorf("declared licence module missing")
		}
		declaration, blob, err := infracostDeclaredSource(module)
		if err != nil {
			return err
		}
		files[filepath.Join("dependencies", path, declaration.File)] = blob
		files[filepath.Join("dependencies", path, "APACHE-2.0.txt")] = terms
	}
	if err := rejectInfracostIgnoreCollisions(modules); err != nil {
		return err
	}
	data, err := json.MarshalIndent(infracostDeclaredLicenses, "", "  ")
	files["declared-licenses.json"] = data
	return err
}
func rejectInfracostIgnoreCollisions(modules map[string]*infracostModule) error {
	for path := range modules {
		for _, prefix := range []string{infracostModulePath, "go.mozilla.org/gopgagent", "github.com/jmespath/go-jmespath"} {
			if path != prefix && strings.HasPrefix(path, prefix) {
				return fmt.Errorf("ignored licence prefix collides with another module")
			}
		}
	}
	return nil
}
