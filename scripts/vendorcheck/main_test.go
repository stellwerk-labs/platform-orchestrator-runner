package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

// This is the captured pinned scanner output for the authenticated official
// OpenTofu executable, including its advisory records and all finding levels.
func capturedReport(t *testing.T, arch string) ([]byte, policy) {
	t.Helper()
	f, err := os.Open(fmt.Sprintf("testdata/official-tofu-1.13.10-%s.json.gz", arch))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = z.Close() }()
	data, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	var p policy
	if err := json.Unmarshal(policyJSON, &p); err != nil {
		t.Fatal(err)
	}
	return data, p
}

func TestCapturedOfficialInventory(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			data, p := capturedReport(t, arch)
			if err := checkReport(data, p, p.Artifacts[arch]); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReviewedReleaseFamily(t *testing.T) {
	var p policy
	if err := json.Unmarshal(policyJSON, &p); err != nil {
		t.Fatal(err)
	}
	for _, release := range []string{"v3.3.1", "v3.3.1-rc.1", "v3.3.1-rc.12"} {
		if err := checkRelease(p, release); err != nil {
			t.Errorf("reviewed release %q rejected: %v", release, err)
		}
	}
	for _, release := range []string{
		"", "v3.3.0", "v3.3.0-rc.1", "v3.3.2", "v3.3.2-rc.1", "v3.4.0", "v3.4.0-rc.1",
		"3.3.1", "v3.3.1-rc.0", "v3.3.1-rc.01", "v3.3.1-rc.-1", "v3.3.1-rc.1.1",
		"v3.3.1-beta.1", "v3.3.1-rc.1+build", "v3.3.1+build", "v3.3.1\n",
	} {
		if err := checkRelease(p, release); err == nil {
			t.Errorf("unreviewed release %q accepted", release)
		}
	}
	for _, changed := range []policy{
		{SchemaVersion: 2, ReleaseFamily: p.ReleaseFamily},
		{SchemaVersion: 1, ReleaseFamily: "v3.3.0"},
		{SchemaVersion: 1, ReleaseFamily: "v3.3.2"},
	} {
		if err := checkRelease(changed, "v3.3.1"); err == nil {
			t.Errorf("unreviewed policy %+v accepted", changed)
		}
	}
}

func TestChangedOrMalformedReportsRejected(t *testing.T) {
	data, p := capturedReport(t, "amd64")
	cases := map[string][]byte{
		"truncated":         data[:len(data)-20],
		"trailing-object":   append(bytes.Clone(data), '{'),
		"trailing-garbage":  append(bytes.Clone(data), []byte("invalid")...),
		"unknown-message":   append(bytes.Clone(data), []byte(`{"unknown":{}}`)...),
		"duplicate-key":     append(bytes.Clone(data), []byte(`{"finding":{},"finding":{}}`)...),
		"scanner-version":   bytes.ReplaceAll(data, []byte(`"v1.9.0"`), []byte(`"v1.9.1"`)),
		"scanner-mode":      bytes.ReplaceAll(data, []byte(`"binary"`), []byte(`"source"`)),
		"binary-compiler":   bytes.ReplaceAll(data, []byte(`"go1.27.4"`), []byte(`"go1.27.3"`)),
		"database":          bytes.ReplaceAll(data, []byte("https://vuln.go.dev"), []byte("file:///tmp/db")),
		"fixed-version":     bytes.ReplaceAll(data, []byte(`"v0.60.0"`), []byte(`"v0.61.0"`)),
		"module-version":    bytes.ReplaceAll(data, []byte(`"v0.57.0"`), []byte(`"v0.57.1"`)),
		"advisory":          bytes.ReplaceAll(data, []byte("GO-2026-6617"), []byte("GO-2099-9999")),
		"compress-advisory": bytes.ReplaceAll(data, []byte("GO-2026-6865"), []byte("GO-2099-9999")),
		"otel-sdk-advisory": bytes.ReplaceAll(data, []byte("GO-2026-6974"), []byte("GO-2099-9999")),
		"baggage-advisory":  bytes.ReplaceAll(data, []byte("GO-2026-6975"), []byte("GO-2099-9999")),
	}
	for name, changed := range cases {
		t.Run(name, func(t *testing.T) {
			if bytes.Equal(changed, data) {
				t.Fatal("negative did not change captured report")
			}
			if err := checkReport(changed, p, p.Artifacts["arm64"]); err == nil {
				t.Fatal("changed report accepted")
			}
		})
	}
}

func TestTextExitContract(t *testing.T) {
	text, err := os.ReadFile("testdata/official-tofu-1.13.10.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkText(text, "1"); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"0", "2", "3", "127", ""} {
		if err := checkText(text, status); err == nil {
			t.Fatalf("unexpected text exit %q accepted", status)
		}
	}
	for name, changed := range map[string][]byte{
		"missing-status": bytes.TrimSuffix(text, []byte("exit status 3\n")),
		"tool-error":     bytes.ReplaceAll(text, []byte("exit status 3"), []byte("exit status 2")),
		"malformed":      []byte("exit status 3\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkText(changed, "1"); err == nil {
				t.Fatal("malformed text status accepted")
			}
		})
	}
}

func TestRemovedOrDuplicatedFindingsRejected(t *testing.T) {
	data, p := capturedReport(t, "amd64")
	d := json.NewDecoder(bytes.NewReader(data))
	var messages []json.RawMessage
	var finding json.RawMessage
	for {
		var m json.RawMessage
		err := d.Decode(&m)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if finding == nil && bytes.Contains(m, []byte(`"finding"`)) {
			finding = m
			continue
		}
		messages = append(messages, m)
	}
	for name, tail := range map[string][]json.RawMessage{"removed": nil, "duplicate": {finding, finding}} {
		t.Run(name, func(t *testing.T) {
			var b strings.Builder
			for _, m := range append(messages, tail...) {
				b.Write(m)
				b.WriteByte('\n')
			}
			if err := checkReport([]byte(b.String()), p, p.Artifacts["arm64"]); err == nil {
				t.Fatal("incomplete or duplicate inventory accepted")
			}
		})
	}
}

// rewriteFindings re-encodes a captured report after edit has changed its findings.
func rewriteFindings(t *testing.T, data []byte, edit func(finding, frame map[string]any)) []byte {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var out bytes.Buffer
	for {
		var message map[string]any
		err := d.Decode(&message)
		if err == io.EOF {
			return out.Bytes()
		}
		if err != nil {
			t.Fatal(err)
		}
		if finding, ok := message["finding"].(map[string]any); ok {
			edit(finding, finding["trace"].([]any)[0].(map[string]any))
		}
		b, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		out.Write(b)
		out.WriteByte('\n')
	}
}

func TestSymbolModuleLabelsDoNotChangeInventory(t *testing.T) {
	data, p := capturedReport(t, "amd64")
	relabelled := 0
	changed := rewriteFindings(t, data, func(finding, frame map[string]any) {
		if frame["package"] != nil {
			frame["module"] = "example.com/relabelled"
			frame["version"] = "v0.0.1"
			finding["fixed_version"] = "v0.0.2"
			relabelled++
		}
	})
	if relabelled == 0 {
		t.Fatal("no package or symbol records relabelled")
	}
	for name, report := range map[string][]byte{"captured": data, "relabelled": changed} {
		if err := checkReport(report, p, p.Artifacts["amd64"]); err != nil {
			t.Errorf("%s report rejected: %v", name, err)
		}
	}
}

func TestChangedSymbolRecordRejected(t *testing.T) {
	data, p := capturedReport(t, "amd64")
	for _, field := range []string{"package", "function"} {
		t.Run(field, func(t *testing.T) {
			done := false
			changed := rewriteFindings(t, data, func(_, frame map[string]any) {
				if !done && frame["function"] != nil {
					frame[field] = fmt.Sprint(frame[field], "Changed")
					done = true
				}
			})
			if err := checkReport(changed, p, p.Artifacts["amd64"]); err == nil {
				t.Fatal("changed symbol record accepted")
			}
		})
	}
}

func TestChangedModuleRecordRejected(t *testing.T) {
	data, p := capturedReport(t, "amd64")
	for _, field := range []string{"module", "version", "fixed_version"} {
		t.Run(field, func(t *testing.T) {
			done := false
			changed := rewriteFindings(t, data, func(finding, frame map[string]any) {
				if !done && frame["package"] == nil {
					target := frame
					if field == "fixed_version" {
						target = finding
					}
					target[field] = fmt.Sprint(target[field], "-changed")
					done = true
				}
			})
			if err := checkReport(changed, p, p.Artifacts["amd64"]); err == nil {
				t.Fatal("changed module-level record accepted")
			}
		})
	}
}
