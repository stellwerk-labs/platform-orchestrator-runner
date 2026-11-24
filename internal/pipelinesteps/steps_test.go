package steps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func digest(t *testing.T, path string) string {
	t.Helper()
	b, e := os.ReadFile(path) // #nosec G304 -- test fixture path.
	if e != nil {
		t.Fatal(e)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func setup(t *testing.T, script string) (Definition, Request) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "task.sh")
	if e := os.WriteFile(path, []byte(script), 0700); e != nil { // #nosec G306 -- test executable must be executable.
		t.Fatal(e)
	}
	return Definition{
		ProtocolVersion:   1,
		Name:              "check",
		Version:           "1.0",
		Phase:             "plan_check",
		Executable:        path,
		ExecutableSHA256:  digest(t, path),
		Interpreter:       "/bin/sh",
		InterpreterSHA256: digest(t, "/bin/sh"),
		Retry:             "read_only",
	}, Request{
		Version:    1,
		TaskID:     "test",
		Inputs:     map[string]Input{},
		Parameters: json.RawMessage(`{}`),
		WorkDir:    dir,
	}
}
func TestProtocolValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"valid", `printf '%s' '{"version":1,"task_id":"test","effect_id":"","decision":"allow","category":"ok","outputs":{}}'`, true},
		{"identity mismatch", `printf '%s' '{"version":1,"task_id":"other","effect_id":"","decision":"allow","category":"ok"}'`, false},
		{"unknown field", `printf '%s' '{"version":1,"task_id":"test","effect_id":"","decision":"allow","category":"ok","outputs":{},"secret":"x"}'`, false},
		{"version", `printf '%s' '{"version":2,"task_id":"test","effect_id":"","decision":"allow","category":"ok","outputs":{}}'`, false},
		{"duplicate", `printf '%s' '{"version":1,"task_id":"test","effect_id":"","decision":"deny","decision":"allow","category":"ok"}'`, false},
		{"category", `printf '%s' '{"version":1,"task_id":"test","effect_id":"","decision":"fault","category":"secret","outputs":{}}'`, false},
		{"two values", `printf '%s' '{"version":1,"task_id":"test","effect_id":"","decision":"allow","category":"ok"} {}'`, false},
		{"oversize", `head -c 70000 /dev/zero`, false},
		{"bad json", `printf '{'`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, r := setup(t, tc.body)
			_, err := Execute(context.Background(), d, r, nil)
			if (err == nil) != tc.ok {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
func TestInputAndExecutableDigest(t *testing.T) {
	d, r := setup(t, `printf '%s' '{"version":1,"task_id":"test","effect_id":"","decision":"allow","category":"ok"}'`)
	p := filepath.Join(r.WorkDir, "input")
	if err := os.WriteFile(p, []byte("plan"), 0600); err != nil {
		t.Fatal(err)
	}
	r.Inputs["plan"] = Input{Path: p, SHA256: digest(t, p)}
	if _, err := Execute(context.Background(), d, r, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(context.Background(), d, r, nil); err == nil {
		t.Fatal("changed input accepted")
	}
	delete(r.Inputs, "plan")
	d.ExecutableSHA256 = strings.Repeat("0", 64)
	if _, err := Execute(context.Background(), d, r, nil); err == nil {
		t.Fatal("changed executable accepted")
	}
}
func TestOutputConfinement(t *testing.T) {
	d, r := setup(t, `printf '%s' "$RESULT"`)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(r.WorkDir, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, link} {
		result := Result{Version: 1, TaskID: "test", Decision: "allow", Category: "ok", Outputs: map[string]Input{"report": {Path: path, SHA256: digest(t, outside)}}}
		b, _ := json.Marshal(result)
		if _, err := Execute(context.Background(), d, r, map[string]string{"RESULT": string(b)}); err == nil {
			t.Fatalf("accepted %s", path)
		}
	}
}
func TestCancellationKillsProcessGroup(t *testing.T) {
	late := filepath.Join(t.TempDir(), "late-child")
	d, r := setup(t, `trap '' INT; (sleep 1; touch "$LATE_MARKER") & wait`)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := Execute(ctx, d, r, map[string]string{"PATH": "/bin:/usr/bin", "LATE_MARKER": late}); err == nil {
		t.Fatal("cancel accepted")
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("cancellation hung")
	}
	time.Sleep(800 * time.Millisecond)
	if _, err := os.Stat(late); !os.IsNotExist(err) {
		t.Fatal("child process survived task cancellation")
	}
}
func TestNoAmbientEnvironmentOrStderr(t *testing.T) {
	t.Setenv("SECRET_EXAMPLE", "unwanted")
	d, r := setup(t, `if [ -n "$SECRET_EXAMPLE" ]; then exit 2; fi; echo 'private-secret' >&2; exit 1`)
	_, err := Execute(context.Background(), d, r, nil)
	if err == nil || strings.Contains(err.Error(), "private-secret") {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestDecisionCategoryAndPhase(t *testing.T) {
	for _, tc := range []struct {
		phase, decision, category string
		accepted                  bool
	}{
		{"plan_check", "allow", "ok", true},
		{"plan_check", "allow", "unknown_outcome", false},
		{"plan_check", "deny", "policy", true},
		{"post_apply", "deny", "policy", false},
		{"plan_check", "deny", "ok", false},
		{"post_apply", "fault", "unknown_outcome", true},
		{"post_apply", "fault", "ok", false},
		{"plan_check", "fault", "policy", false},
	} {
		t.Run(tc.phase+"/"+tc.decision+"/"+tc.category, func(t *testing.T) {
			d, r := setup(t, `printf '%s' "$RESULT"`)
			d.Phase = tc.phase
			result := Result{Version: 1, TaskID: r.TaskID, EffectID: r.EffectID, Decision: tc.decision, Category: tc.category}
			b, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Execute(context.Background(), d, r, map[string]string{"RESULT": string(b)})
			if (err == nil) != tc.accepted {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
