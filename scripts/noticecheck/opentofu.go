package main

import (
	"archive/zip"
	"bytes"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const opentofuModule = "github.com/opentofu/opentofu"
const opentofuVersion = "1.13.9-stellwerk.remediation.20261123.1"
const opentofuCompiler = "go1.27.4"
const opentofuCommit = "c83e7e50daff10ac704cb0811e9ffd0ad2523759"
const opentofuZIPHash = "57a1cb18f2a91f7a00be94648dbb77d1539e59e303433f2946afdd6707388637"
const opentofuBuildHash = "1245f2205f5d4ad0ba2f6a761fe068e1b63168296b5eb8075eac511e646c20e9"
const opentofuSourceHash = "eb956b5d0884862c0e5446df7c64b06176c1dd9661d69980cf44433d70a6bf62"

type opentofuReceipt struct {
	Version, UpstreamVersion, SourceCommit, ModuleSum, GoModSum, Compiler string
	OriginalSHA256, EffectiveSHA256, NoticeSHA256                         map[string]string
	Archives                                                              map[string]string `json:"correspondingSourceSHA256"`
}

func readOpentofuReceipt(root string) (opentofuReceipt, error) {
	var receipt opentofuReceipt
	data, err := regular(filepath.Join(root, "scripts/opentofu-inputs/BUILD.json"))
	if err != nil || hash(data) != opentofuBuildHash || json.Unmarshal(data, &receipt) != nil || receipt.Version != opentofuVersion || receipt.SourceCommit != opentofuCommit || receipt.Compiler != opentofuCompiler {
		return receipt, fmt.Errorf("OpenTofu authenticated build receipt differs")
	}
	return receipt, nil
}

// Only this vendor uses the newer compiler; the Runner verifier remains pinned.
func opentofuGo(build, dir, arch string, args ...string) ([]byte, error) {
	binary := os.Getenv("OPENTOFU_GO")
	if !filepath.IsAbs(binary) {
		return nil, fmt.Errorf("explicit absolute OpenTofu compiler required")
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOWORK=off", "GOENV=off", "GOROOT=", "GOFLAGS=-p=1", "GOEXPERIMENT=", "GOMAXPROCS=2", "GOMEMLIMIT=2GiB", "GOPRIVATE=", "GONOSUMDB=", "GONOPROXY=", "GOPROXY=https://proxy.golang.org", "GOSUMDB=sum.golang.org", "CGO_ENABLED=0", "GOOS=", "GOARCH=", "GOAMD64=", "GOARM64=")
	if build != "" {
		cmd.Env = append(cmd.Env, "GOMODCACHE="+filepath.Join(build, "modules"))
	}
	if arch != "" {
		cmd.Env = append(cmd.Env, "GOOS=linux", "GOARCH="+arch)
	}
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("OpenTofu build input command failed: %s", diagnostics.String())
	}
	return data, nil
}

func writeOpentofuSourceEntry(file *zip.File, name, destination, root string, receipt opentofuReceipt) error {
	input, err := file.Open()
	if err != nil {
		return err
	}
	blob, readErr := io.ReadAll(input)
	closeErr := input.Close()
	if readErr != nil || closeErr != nil {
		return fmt.Errorf("OpenTofu source entry unavailable")
	}
	if expected, modified := receipt.EffectiveSHA256[name]; modified {
		if hash(blob) != receipt.OriginalSHA256[name] {
			return fmt.Errorf("OpenTofu original manifest differs")
		}
		blob, err = regular(filepath.Join(root, "scripts/opentofu-inputs", name))
		if err != nil || hash(blob) != expected {
			return fmt.Errorf("OpenTofu effective manifest differs")
		}
	}
	path := filepath.Join(destination, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, blob, 0644)
}

func copyOpentofuZIP(archive, destination, root string) error {
	data, err := regular(archive)
	if err != nil || hash(data) != opentofuZIPHash {
		return fmt.Errorf("OpenTofu source archive differs")
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	receipt, err := readOpentofuReceipt(root)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, file := range reader.File {
		name := strings.TrimPrefix(file.Name, opentofuModule+"@v1.13.9/")
		if name == file.Name || !safeInfracostArchiveName(name) || seen[name] || !file.Mode().IsRegular() {
			return fmt.Errorf("unsafe OpenTofu source archive entry")
		}
		seen[name] = true
		if err := writeOpentofuSourceEntry(file, name, destination, root, receipt); err != nil {
			return err
		}
	}
	if len(seen) != 5136 {
		return fmt.Errorf("OpenTofu source file count differs")
	}
	return nil
}

func opentofuCompilerArchive(archivePath, moduleVersion string) (archiveSHA256, compilerSHA256 string, err error) {
	binary, err := regular(os.Getenv("OPENTOFU_GO"))
	if err != nil {
		return "", "", err
	}
	archive, err := regular(archivePath)
	if err != nil {
		return "", "", err
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return "", "", err
	}
	input, err := reader.Open("golang.org/toolchain@" + moduleVersion + "/bin/go")
	if err != nil {
		return "", "", err
	}
	contents, readErr := io.ReadAll(input)
	closeErr := input.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(contents, binary) {
		return "", "", fmt.Errorf("OpenTofu compiler differs from authenticated archive")
	}
	return hash(archive), hash(binary), nil
}

// TMPDIR can itself be inside a checkout. Own go.mod prevents parent inheritance.
func opentofuAcquisitionDirectory() (string, error) {
	dir, err := os.MkdirTemp("", "opentofu-acquisition-")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module stellwerk.invalid/opentofu-acquisition\n\ngo 1.26.11\n"), 0600); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

func opentofuToolchainInput() ([]byte, error) {
	acquisition, err := opentofuAcquisitionDirectory()
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(acquisition) }()
	data, err := opentofuGo("", acquisition, "", "env", "-json", "GOVERSION", "GOHOSTOS", "GOHOSTARCH", "GOROOT")
	if err != nil {
		return nil, err
	}
	var environment map[string]string
	if json.Unmarshal(data, &environment) != nil || environment["GOVERSION"] != opentofuCompiler {
		return nil, fmt.Errorf("OpenTofu toolchain version differs")
	}
	moduleVersion := "v0.0.1-" + opentofuCompiler + "." + environment["GOHOSTOS"] + "-" + environment["GOHOSTARCH"]
	data, err = opentofuGo("", acquisition, "", "mod", "download", "-json", "golang.org/toolchain@"+moduleVersion)
	if err != nil {
		return nil, err
	}
	var module struct {
		Path, Version, Sum, GoModSum, Zip, Dir string
	}
	if json.Unmarshal(data, &module) != nil || module.Path != "golang.org/toolchain" || module.Version != moduleVersion || !strings.HasPrefix(module.Sum, "h1:") || module.Dir != environment["GOROOT"] {
		return nil, fmt.Errorf("OpenTofu toolchain authentication differs")
	}
	archiveSHA256, compilerSHA256, err := opentofuCompilerArchive(module.Zip, moduleVersion)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(map[string]string{
		"module":         module.Path,
		"version":        module.Version,
		"sum":            module.Sum,
		"goModSum":       module.GoModSum,
		"archiveSHA256":  archiveSHA256,
		"compilerSHA256": compilerSHA256,
		"goVersion":      opentofuCompiler,
		"hostOS":         environment["GOHOSTOS"],
		"hostArch":       environment["GOHOSTARCH"],
	}, "", "  ")
}

func downloadOpentofuSource(output string, receipt opentofuReceipt) ([]byte, []byte, error) {
	acquisition, err := opentofuAcquisitionDirectory()
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = os.RemoveAll(acquisition) }()
	metadata, err := opentofuGo(output, acquisition, "", "mod", "download", "-json", opentofuModule+"@v1.13.9")
	if err != nil {
		return nil, nil, err
	}
	var source struct {
		Path, Version, Sum, GoModSum, Zip string
		Origin                            *struct{ Hash string }
	}
	if json.Unmarshal(metadata, &source) != nil || source.Path != opentofuModule || source.Version != "v1.13.9" || source.Sum != receipt.ModuleSum || source.GoModSum != receipt.GoModSum || source.Origin == nil || source.Origin.Hash != opentofuCommit {
		return nil, nil, fmt.Errorf("OpenTofu module authentication differs")
	}
	archive, err := regular(source.Zip)
	if err != nil || hash(archive) != opentofuZIPHash {
		return nil, nil, fmt.Errorf("OpenTofu authenticated ZIP differs")
	}
	return archive, metadata, nil
}

func prepareOpentofu(output, root string) error {
	receipt, err := readOpentofuReceipt(root)
	if err != nil {
		return err
	}
	if err := os.Mkdir(output, 0755); err != nil {
		return err
	}
	toolchain, err := opentofuToolchainInput()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(output, "toolchain-input.json"), toolchain, 0644); err != nil {
		return err
	}
	archive, metadata, err := downloadOpentofuSource(output, receipt)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(output, "upstream.zip"), archive, 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(output, "upstream-module.json"), metadata, 0644); err != nil {
		return err
	}
	privateSource := filepath.Join(output, "source")
	if err := copyOpentofuZIP(filepath.Join(output, "upstream.zip"), privateSource, root); err != nil {
		return err
	}
	if _, err := opentofuGo(output, privateSource, "", "mod", "download"); err != nil {
		return err
	}
	_, err = opentofuGo(output, privateSource, "", "mod", "verify")
	return err
}

func verifyOpentofuSource(build, root string) error {
	work, err := os.MkdirTemp("", "opentofu-authoritative-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()
	if err := copyOpentofuZIP(filepath.Join(build, "upstream.zip"), work, root); err != nil {
		return err
	}
	count := 0
	err = filepath.WalkDir(filepath.Join(build, "source"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(filepath.Join(build, "source"), path)
		if err != nil {
			return err
		}
		count++
		return same(path, filepath.Join(work, relative))
	})
	if err != nil || count != 5136 {
		return fmt.Errorf("OpenTofu effective source tree differs")
	}
	return nil
}

// Reuse the existing Go module records and legal-file walk, not its toolchain selection.
func opentofuTargetGraph(build, arch string) ([]infracostPackage, error) {
	if arch != "amd64" && arch != "arm64" {
		return nil, fmt.Errorf("unsupported OpenTofu target")
	}
	data, err := opentofuGo(build, filepath.Join(build, "source"), arch, "list", "-mod=readonly", "-deps", "-json", "./cmd/tofu")
	if err != nil {
		return nil, err
	}
	sums, err := regular(filepath.Join(build, "source/go.sum"))
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
	var packages []infracostPackage
	decoder := json.NewDecoder(bytes.NewReader(data))
	for decoder.More() {
		var pkg struct {
			ImportPath, Dir string
			Module          *infracostModule
		}
		if err := decoder.Decode(&pkg); err != nil {
			return nil, err
		}
		if strings.HasPrefix(pkg.ImportPath, "golang.org/x/crypto/openpgp") {
			return nil, fmt.Errorf("legacy OpenPGP production package refused")
		}
		if pkg.Module != nil {
			fillInfracostChecksums(pkg.Module, checksums)
			packages = append(packages, infracostPackage{Dir: pkg.Dir, Module: pkg.Module})
		}
	}
	return packages, nil
}

func verifyOpentofuBuild(binary, arch string, packages []infracostPackage) ([]byte, error) {
	info, err := buildinfo.ReadFile(binary)
	if err != nil || info.GoVersion != opentofuCompiler || info.Path != opentofuModule+"/cmd/tofu" || info.Main.Path != opentofuModule {
		return nil, fmt.Errorf("unexpected OpenTofu executable identity")
	}
	settings := map[string]string{}
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	cpuKey, cpuLevel := "GOAMD64", "v1"
	if arch == "arm64" {
		cpuKey, cpuLevel = "GOARM64", "v8.0"
	}
	for key, value := range map[string]string{"GOOS": "linux", "GOARCH": arch, "CGO_ENABLED": "0", "-trimpath": "true", cpuKey: cpuLevel} {
		if settings[key] != value {
			return nil, fmt.Errorf("unexpected OpenTofu executable settings")
		}
	}
	expected := map[string]string{}
	for _, pkg := range packages {
		expected[pkg.Module.Path] = infracostModuleIdentity(pkg.Module)
	}
	delete(expected, opentofuModule)
	actual := map[string]string{}
	for _, module := range info.Deps {
		actual[module.Path] = infracostModuleIdentity(infracostDebugModule(module))
	}
	if !maps.Equal(actual, expected) {
		return nil, fmt.Errorf("OpenTofu linked module graph differs")
	}
	return []byte(info.String()), nil
}

func opentofuSourceFiles(build, root string) (map[string][]byte, error) {
	if err := verifyOpentofuSource(build, root); err != nil {
		return nil, err
	}
	toolchain, err := opentofuToolchainInput()
	if err != nil {
		return nil, err
	}
	goroot, err := opentofuGo("", root, "", "env", "GOROOT")
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for name, source := range map[string]string{
		"LICENSE":                    filepath.Join(build, "source/LICENSE"),
		"README.md":                  filepath.Join(build, "source/README.md"),
		"CHANGELOG.md":               filepath.Join(build, "source/CHANGELOG.md"),
		"SOURCE.json":                filepath.Join(root, "scripts/release-notices/opentofu-source.json"),
		"SOURCE-AVAILABILITY.txt":    filepath.Join(root, "scripts/release-notices/opentofu-corresponding-source.txt"),
		"build/BUILD.json":           filepath.Join(root, "scripts/opentofu-inputs/BUILD.json"),
		"build/LICENSE":              filepath.Join(root, "LICENSE"),
		"build/go.mod":               filepath.Join(root, "scripts/opentofu-inputs/go.mod"),
		"build/go.sum":               filepath.Join(root, "scripts/opentofu-inputs/go.sum"),
		"build/build-opentofu.sh":    filepath.Join(root, "scripts/build-opentofu.sh"),
		"build/toolchain-input.json": filepath.Join(build, "toolchain-input.json"),
		"go/LICENSE":                 filepath.Join(strings.TrimSpace(string(goroot)), "LICENSE"),
		"go/PATENTS":                 filepath.Join(strings.TrimSpace(string(goroot)), "PATENTS"),
	} {
		files[name], err = regular(source)
		if err != nil {
			return nil, err
		}
	}
	if !bytes.Equal(toolchain, files["build/toolchain-input.json"]) || hash(files["SOURCE.json"]) != opentofuSourceHash {
		return nil, fmt.Errorf("OpenTofu toolchain or source receipt differs")
	}
	return files, nil
}

func opentofuInputs(build, arch, binary, root string) (map[string][]byte, error) {
	files, err := opentofuSourceFiles(build, root)
	if err != nil {
		return nil, err
	}
	packages, err := opentofuTargetGraph(build, arch)
	if err != nil {
		return nil, err
	}
	files["buildinfo.txt"], err = verifyOpentofuBuild(binary, arch, packages)
	if err != nil {
		return nil, err
	}
	files["target.txt"] = []byte("linux/" + arch + "\n")
	binaryBytes, err := regular(binary)
	if err != nil {
		return nil, err
	}
	files["binary.sha256"] = []byte(hash(binaryBytes) + "\n")
	rows := map[string]bool{}
	for _, pkg := range packages {
		if pkg.Module.Path != opentofuModule && strings.HasPrefix(pkg.Module.Path, opentofuModule) ||
			pkg.Module.Path != "github.com/jmespath/go-jmespath" && strings.HasPrefix(pkg.Module.Path, "github.com/jmespath/go-jmespath") {
			return nil, fmt.Errorf("OpenTofu license ignore prefix collides")
		}
		rows[infracostModuleIdentity(pkg.Module)] = true
		if pkg.Module.Path != opentofuModule {
			if err := infracostPackageNotices(pkg, files); err != nil {
				return nil, err
			}
		}
	}
	if err := addOpentofuDeclaredLicense(packages, files); err != nil {
		return nil, err
	}
	list := make([]string, 0, len(rows))
	for row := range rows {
		list = append(list, row)
	}
	sort.Strings(list)
	files["modules.txt"] = []byte(strings.Join(list, "\n") + "\n")
	return files, nil
}

func addOpentofuDeclaredLicense(packages []infracostPackage, files map[string][]byte) error {
	for _, pkg := range packages {
		if pkg.Module.Path != "github.com/jmespath/go-jmespath" {
			continue
		}
		if pkg.Module.Replace != nil {
			return fmt.Errorf("declared OpenTofu dependency replacement refused")
		}
		declaration, notice, err := infracostDeclaredSource(pkg.Module)
		if err != nil {
			return err
		}
		terms := files["dependencies/go.opentelemetry.io/otel/LICENSE"]
		if hash(terms) != "1ae07514be1d7bb33f0698f8d91fb51b8b9fe1463157ec1c72081a49b9bc6f40" {
			return fmt.Errorf("declared OpenTofu dependency Apache terms differ")
		}
		files[filepath.Join("dependencies", pkg.Module.Path, declaration.File)] = notice
		files[filepath.Join("dependencies", pkg.Module.Path, "APACHE-2.0.txt")] = terms
		return nil
	}
	return fmt.Errorf("declared OpenTofu dependency missing")
}

func collectOpentofu(output, build, arch, binary, root string) error {
	files, err := opentofuInputs(build, arch, binary, root)
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

func archiveOpentofu(classified, output, build, root string) error {
	if err := verifyOpentofuSource(build, root); err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "opentofu-corresponding-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()
	for destination, source := range map[string]string{"opentofu": filepath.Join(build, "source"), "dependency-source": classified} {
		if err := exec.Command("cp", "-R", source, filepath.Join(work, destination)).Run(); err != nil {
			return err
		}
	}
	if err := os.Mkdir(filepath.Join(work, "build"), 0755); err != nil {
		return err
	}
	for name, source := range map[string]string{"BUILD.json": "scripts/opentofu-inputs/BUILD.json", "build-opentofu.sh": "scripts/build-opentofu.sh", "README.txt": "scripts/release-notices/opentofu-corresponding-source.txt", "LICENSE": "LICENSE"} {
		data, err := regular(filepath.Join(root, source))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(work, "build", name), data, 0644); err != nil {
			return err
		}
	}
	return archiveInfracostSources(work, output)
}

func checkOpentofu(bundle, root, arch string) error {
	build, binary := os.Getenv("OPENTOFU_BUILD_DIR"), os.Getenv("OPENTOFU_BINARY")
	if build == "" || binary == "" {
		return fmt.Errorf("OpenTofu authoritative build inputs and binary required")
	}
	files, err := opentofuInputs(build, arch, binary, root)
	if err != nil {
		return err
	}
	for name, expected := range files {
		data, err := regular(filepath.Join(bundle, "opentofu", name))
		if err != nil || !bytes.Equal(data, expected) {
			return fmt.Errorf("OpenTofu notice closure differs: %s", name)
		}
	}
	var receipt opentofuReceipt
	if json.Unmarshal(files["SOURCE.json"], &receipt) != nil {
		return fmt.Errorf("invalid OpenTofu source receipt")
	}
	archive, err := regular(filepath.Join(bundle, "opentofu/corresponding-source.tar.gz"))
	expected := receipt.Archives["linux/"+arch]
	if err != nil || len(expected) != 64 || len(archive) == 0 || hash(archive) != expected {
		return fmt.Errorf("OpenTofu corresponding source differs or is not materialized")
	}
	return nil
}
