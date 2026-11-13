package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// This is the captured pinned scanner output for the authenticated official
// OpenTofu executable, including its advisory records and all finding levels.
func capturedReport(t *testing.T) ([]byte, policy) {
	t.Helper()
	f, err := os.Open("testdata/official-tofu-1.13.2.json.gz")
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
	data, p := capturedReport(t)
	for arch, want := range p.Artifacts {
		t.Run(arch, func(t *testing.T) {
			if err := checkReport(data, p, want); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestChangedOrMalformedReportsRejected(t *testing.T) {
	data, p := capturedReport(t)
	cases := map[string][]byte{
		"truncated":        data[:len(data)-20],
		"trailing-object":  append(bytes.Clone(data), '{'),
		"trailing-garbage": append(bytes.Clone(data), []byte("invalid")...),
		"unknown-message":  append(bytes.Clone(data), []byte(`{"unknown":{}}`)...),
		"duplicate-key":    append(bytes.Clone(data), []byte(`{"finding":{},"finding":{}}`)...),
		"scanner-version":  bytes.ReplaceAll(data, []byte(`"v1.9.0"`), []byte(`"v1.9.1"`)),
		"scanner-mode":     bytes.ReplaceAll(data, []byte(`"binary"`), []byte(`"source"`)),
		"binary-compiler":  bytes.ReplaceAll(data, []byte(`"go1.27.3"`), []byte(`"go1.26.10"`)),
		"database":         bytes.ReplaceAll(data, []byte("https://vuln.go.dev"), []byte("file:///tmp/db")),
		"fixed-version":    bytes.ReplaceAll(data, []byte(`"v0.60.0"`), []byte(`"v0.61.0"`)),
		"module-version":   bytes.ReplaceAll(data, []byte(`"v0.57.0"`), []byte(`"v0.57.1"`)),
		"advisory":         bytes.ReplaceAll(data, []byte("GO-2026-6617"), []byte("GO-2099-9999")),
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
	text, err := os.ReadFile("testdata/official-tofu-1.13.2.txt")
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
	data, p := capturedReport(t)
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
