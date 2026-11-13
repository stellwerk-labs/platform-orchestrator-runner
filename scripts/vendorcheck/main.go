// Command vendorcheck applies the reviewed release-scoped vendor disposition.
package main

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
)

//go:embed accepted-opentofu.json
var policyJSON []byte

type artifact struct {
	SHA256         string   `json:"sha256"`
	GoVersion      string   `json:"goVersion"`
	Advisories     []string `json:"advisories"`
	FindingCount   int      `json:"findingCount"`
	FindingsSHA256 string   `json:"findingsSHA256"`
}

type policy struct {
	SchemaVersion  int                 `json:"schemaVersion"`
	ReleaseFamily  string              `json:"releaseFamily"`
	Vendor         string              `json:"vendor"`
	Scanner        string              `json:"scanner"`
	ScannerVersion string              `json:"scannerVersion"`
	Artifacts      map[string]artifact `json:"artifacts"`
}

func main() {
	if len(os.Args) != 6 {
		fmt.Fprintln(os.Stderr, "usage: vendorcheck RELEASE BINARY TEXT_REPORT TEXT_EXIT JSON_REPORT")
		os.Exit(2)
	}
	if err := check(os.Args[1], os.Args[2], os.Args[3], os.Args[4], os.Args[5]); err != nil {
		fmt.Fprintln(os.Stderr, "vendor disposition rejected:", err)
		os.Exit(1)
	}
	fmt.Println("Accepted reviewed official OpenTofu residual findings for", os.Args[1], "only; raw reports remain above.")
}

func check(release, binary, textPath, textExit, jsonPath string) error {
	var p policy
	if err := json.Unmarshal(policyJSON, &p); err != nil {
		return err
	}
	if err := checkRelease(p, release); err != nil {
		return err
	}
	info, err := os.Lstat(binary)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("vendor executable must be a regular file")
	}
	f, err := elf.Open(binary)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	arch := map[elf.Machine]string{elf.EM_X86_64: "amd64", elf.EM_AARCH64: "arm64"}[f.Machine]
	want, ok := p.Artifacts[arch]
	if !ok || f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB {
		return fmt.Errorf("unreviewed ELF platform")
	}
	input, err := os.Open(binary)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, input); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != want.SHA256 {
		return fmt.Errorf("official vendor executable hash differs")
	}
	text, err := os.ReadFile(textPath)
	if err != nil {
		return err
	}
	if err := checkText(text, textExit); err != nil {
		return err
	}
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return err
	}
	if len(data) > 32<<20 {
		return fmt.Errorf("scanner report exceeds bound")
	}
	return checkReport(data, p, want)
}

func checkRelease(p policy, release string) error {
	if p.SchemaVersion != 1 || p.ReleaseFamily != "v3.3.0" || !regexp.MustCompile(`^v3\.3\.0(-rc\.[1-9][0-9]*)?$`).MatchString(release) {
		return fmt.Errorf("release is outside the reviewed v3.3.0 family")
	}
	return nil
}

func checkText(text []byte, status string) error {
	// go run propagates the pinned scanner's finding exit 3 as exit 1.
	if status != "1" || !bytes.HasSuffix(text, []byte("exit status 3\n")) || !bytes.Contains(text, []byte("=== Symbol Results ===")) {
		return fmt.Errorf("text scanner did not finish with its known finding status")
	}
	return nil
}

func checkReport(data []byte, p policy, want artifact) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	configs, sboms := 0, 0
	var findings []string
	ids, osvs := map[string]bool{}, map[string]bool{}
	for {
		v, err := uniqueValue(d, 0)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		message, ok := v.(map[string]any)
		if !ok || len(message) != 1 {
			return fmt.Errorf("malformed scanner message")
		}
		for kind, value := range message {
			if configs == 0 && kind != "config" {
				return fmt.Errorf("scanner configuration must be first")
			}
			body, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("malformed scanner message body")
			}
			switch kind {
			case "config":
				configs++
				if configs != 1 || sboms != 0 || len(findings) != 0 || body["protocol_version"] != "v1.0.0" || body["scanner_name"] != p.Scanner || body["scanner_version"] != p.ScannerVersion || body["db"] != "https://vuln.go.dev" || body["scan_mode"] != "binary" || body["scan_level"] != "symbol" {
					return fmt.Errorf("unreviewed scanner configuration")
				}
			case "SBOM":
				sboms++
				if configs != 1 || sboms != 1 || body["go_version"] != want.GoVersion {
					return fmt.Errorf("missing or changed scanner SBOM")
				}
			case "finding":
				id, ok := body["osv"].(string)
				if !ok || id == "" || configs != 1 || sboms != 1 {
					return fmt.Errorf("malformed scanner finding")
				}
				ids[id] = true
				b, err := json.Marshal(body)
				if err != nil {
					return err
				}
				findings = append(findings, string(b))
			case "osv":
				id, ok := body["id"].(string)
				if !ok || id == "" {
					return fmt.Errorf("malformed advisory record")
				}
				osvs[id] = true
			case "progress":
				if _, ok := body["message"].(string); !ok {
					return fmt.Errorf("malformed scanner progress")
				}
			default:
				return fmt.Errorf("unknown scanner message %q", kind)
			}
		}
	}
	if configs != 1 || sboms != 1 || len(findings) != want.FindingCount || len(ids) != len(want.Advisories) {
		return fmt.Errorf("incomplete or changed finding inventory")
	}
	for _, id := range want.Advisories {
		if !ids[id] || !osvs[id] {
			return fmt.Errorf("missing reviewed advisory %s", id)
		}
	}
	// Preserve every module/package/symbol tuple, fixed version and trace frame.
	// Sorting removes emission-order variation; duplicates still change the hash.
	sort.Strings(findings)
	h := sha256.Sum256([]byte(strings.Join(findings, "\n") + "\n"))
	if hex.EncodeToString(h[:]) != want.FindingsSHA256 {
		return fmt.Errorf("finding tuples differ from the reviewed inventory")
	}
	return nil
}

func uniqueValue(d *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, fmt.Errorf("scanner JSON exceeds nesting bound")
	}
	t, err := d.Token()
	if err != nil {
		if err == io.EOF && depth > 0 {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		m := map[string]any{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			k, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("invalid JSON object key")
			}
			if _, exists := m[k]; exists {
				return nil, fmt.Errorf("duplicate JSON object key")
			}
			v, err := uniqueValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			m[k] = v
		}
		_, err := d.Token()
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return m, err
	case json.Delim('['):
		var a []any
		for d.More() {
			v, err := uniqueValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err := d.Token()
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return a, err
	case json.Delim('}'), json.Delim(']'):
		return nil, fmt.Errorf("unexpected closing JSON delimiter")
	default:
		return t, nil
	}
}
