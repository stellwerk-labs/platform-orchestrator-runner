package rolloutexec

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	steps "github.com/stellwerk-labs/platform-orchestrator-runner/internal/pipelinesteps"
)

func fixture(t *testing.T) (*Executor, Request) {
	t.Helper()
	if executableDigest("opentofu") == "unavailable" {
		t.Skip("OpenTofu is not installed")
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	return &Executor{Directory: directory, Identity: identity}, Request{
		ID: "rollout", OrgID: "org", EnvironmentID: "environment", RunnerID: "runner", Tool: "opentofu",
		Backend: "local", Configuration: `resource "terraform_data" "test" { input = "approved" }`, Manifest: json.RawMessage(`{}`),
	}
}

func installTask(t *testing.T, executor *Executor, phase, body string) Step {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python is not installed")
	}
	python, err = filepath.EvalSymlinks(python)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "task.py")
	if err := os.WriteFile(file, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	tool := Tool{ID: "check", Name: "Check rollout", Phase: phase, Retry: "read_only", Definition: steps.Definition{
		ProtocolVersion: 1, Name: "check", Version: "1", Phase: phase, Retry: "read_only", Executable: file,
		ExecutableSHA256: fileDigest(t, file), Interpreter: python, InterpreterSHA256: fileDigest(t, python),
	}}
	executor.Tools = []Tool{tool}
	return Step{ID: "step", ToolID: "check", Parameters: json.RawMessage(`{}`)}
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bytesDigest(data)
}

const successfulTask = `import json, os, sys
r = json.load(sys.stdin)
assert 'ROLLOUT_AMBIENT_SECRET' not in os.environ
assert sorted(r['inputs']) == json.loads(os.environ['EXPECTED_INPUTS'])
print(json.dumps({'version': 1, 'task_id': r['task_id'], 'effect_id': r['effect_id'], 'decision': 'allow', 'category': 'ok', 'outputs': {}}))
`

func TestRunStepMaterializesOnlyPhaseInputs(t *testing.T) {
	t.Setenv("ROLLOUT_AMBIENT_SECRET", "must-not-enter-task")
	for _, phase := range []string{"pre_plan", "plan_check", "post_apply"} {
		t.Run(phase, func(t *testing.T) {
			e, request := fixture(t)
			step := installTask(t, e, phase, successfulTask)
			expected := `["manifest"]`
			if phase == "plan_check" {
				expected = `["manifest", "plan"]`
			}
			if phase == "post_apply" {
				expected = `["manifest", "outputs", "plan"]`
			}
			e.Tools[0].Environment = map[string]string{"EXPECTED_INPUTS": expected}
			result, err := e.RunStep(context.Background(), request, step, []byte(`{}`), []byte(`{}`))
			if err != nil || result.Decision != "allow" || len(result.EffectID) != 64 {
				t.Fatalf("result = %+v, error = %v", result, err)
			}
			entries, err := os.ReadDir(e.Directory)
			if err != nil || len(entries) != 0 {
				t.Fatalf("private task inputs were not removed: %v", err)
			}
		})
	}
}

func TestValidationRejectsMissingCapabilityAndChangedExecutable(t *testing.T) {
	e, request := fixture(t)
	step := installTask(t, e, "plan_check", successfulTask)
	e.Tools[0].RequiredCapabilities = []string{"controlplane.online"}
	if e.Validate(request, []Step{step}) == nil {
		t.Fatal("accepted an online-only task at a disconnected site")
	}
	e.Capabilities = []string{"controlplane.online"}
	if err := e.Validate(request, []Step{step}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.Tools[0].Definition.Executable, []byte("changed"), 0700); err != nil {
		t.Fatal(err)
	}
	if e.Validate(request, []Step{step}) == nil {
		t.Fatal("accepted changed local task executable")
	}
}

func TestValidationRequiresReadOnlyPreparationAndChecks(t *testing.T) {
	for _, phase := range []string{"pre_plan", "plan_check", "post_apply"} {
		t.Run(phase, func(t *testing.T) {
			e, request := fixture(t)
			step := installTask(t, e, phase, successfulTask)
			e.Tools[0].Retry, e.Tools[0].Definition.Retry = "receipt", "receipt"
			err := e.Validate(request, []Step{step})
			if (err == nil) != (phase == "post_apply") {
				t.Fatalf("phase %s validation = %v", phase, err)
			}
		})
	}
}

func TestCatalogDigestPinsSiteConfiguration(t *testing.T) {
	e, _ := fixture(t)
	installTask(t, e, "plan_check", successfulTask)
	e.Capabilities = []string{"b", "a"}
	before := e.CatalogDigest()
	e.Capabilities = []string{"a", "b"}
	if before != e.CatalogDigest() {
		t.Fatal("capability ordering changed digest")
	}
	e.Tools[0].Environment = map[string]string{"SITE_CREDENTIAL": "rotated"}
	if before == e.CatalogDigest() {
		t.Fatal("changed site environment did not change digest")
	}
}

func TestRunStepRejectsUnsupportedOutputArtifacts(t *testing.T) {
	e, request := fixture(t)
	step := installTask(t, e, "pre_plan", `import hashlib, json, pathlib, sys
r = json.load(sys.stdin)
p = pathlib.Path(r['work_dir']) / 'report'
p.write_bytes(b'report')
print(json.dumps({'version': 1, 'task_id': r['task_id'], 'effect_id': r['effect_id'], 'decision': 'allow', 'category': 'ok', 'outputs': {'report': {'path': str(p), 'sha256': hashlib.sha256(b'report').hexdigest()}}}))
`)
	if _, err := e.RunStep(context.Background(), request, step, nil, nil); err == nil {
		t.Fatal("accepted task artifact with no durable output transport")
	}
}

func TestOversizePlanLogIsExplicitlyUnavailable(t *testing.T) {
	result := planWithLogs(Plan{}, strings.Repeat("x", PlanLogLimit+1))
	if !result.LogsTooLarge || len(result.Logs) != 0 {
		t.Fatal("oversized logs silently truncated or retained")
	}
}
