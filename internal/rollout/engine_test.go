package rollout

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	steps "github.com/stellwerk-labs/platform-orchestrator-runner/internal/pipelinesteps"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
)

type fixture struct {
	engine  *Engine
	config  Config
	key     ed25519.PrivateKey
	command Command
	state   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("tofu"); err != nil {
		t.Skip("OpenTofu is not installed")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	f := &fixture{key: private, state: filepath.Join(directory, "tfstate")}
	f.config = Config{Directory: filepath.Join(directory, "private"), OrgID: "org", RunnerID: "site", PublicKey: public, Executor: &rolloutexec.Executor{}}
	f.engine, err = Open(context.Background(), f.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeFixture(t, f.engine) })
	f.command = Command{
		Version: Version, ID: "rollout-1", OrgID: "org", RunnerID: "site", EnvironmentID: "env",
		IssuedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour), Approval: Approval{Mode: "local"}, ToolsDigest: f.config.Executor.CatalogDigest(),
		Request: rolloutexec.Request{ID: "rollout-1", OrgID: "org", RunnerID: "site", EnvironmentID: "env", Backend: "local", Tool: "opentofu", Manifest: json.RawMessage(`{}`),
			Configuration: fmt.Sprintf("terraform {\n backend \"local\" { path = %q }\n}\nresource \"terraform_data\" \"marker\" { input = \"PRIVATE_PLAN_CANARY\" }\noutput \"value\" { value = terraform_data.marker.output }\n", f.state)},
	}
	return f
}

func closeFixture(t *testing.T, e *Engine) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Error(err)
	}
}

func (f *fixture) signed(t *testing.T, command Command) SignedCommand {
	t.Helper()
	data, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	return SignedCommand{Payload: data, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(f.key, data))}
}

func waitPhase(t *testing.T, e *Engine, id, phase string) Progress {
	t.Helper()
	until := time.Now().Add(30 * time.Second)
	for time.Now().Before(until) {
		p, err := e.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if p.Phase == phase {
			return p
		}
		if terminal(p.Phase) {
			t.Fatalf("expected %s, got %+v", phase, p)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("rollout did not reach " + phase)
	return Progress{}
}

func requireCode(t *testing.T, err error, expected string) {
	t.Helper()
	var safe *Error
	if !errors.As(err, &safe) || safe.Code != expected {
		t.Fatalf("expected %s, got %v", expected, err)
	}
}

func TestSignedImportResumesLocalGateAndAppliesRetainedPlan(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	signed := f.signed(t, f.command)
	if _, err := f.engine.Submit(ctx, signed); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
	assertReview(t, p)
	if _, err := os.Stat(f.state); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("planning wrote state")
	}
	closeFixture(t, f.engine)
	var err error
	f.engine, err = Open(ctx, f.config)
	if err != nil {
		t.Fatal(err)
	}
	restored := waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
	if restored.Subject != p.Subject {
		t.Fatal("approval subject changed on recovery")
	}
	if _, err = f.engine.Decide(ctx, p.ID, "wrong-subject", "approve", "reviewer"); err == nil {
		t.Fatal("wrong approval subject accepted")
	}
	if _, err = f.engine.Decide(ctx, p.ID, p.Subject, "approve", "reviewer"); err != nil {
		t.Fatal(err)
	}
	completed := waitPhase(t, f.engine, p.ID, "completed")
	state, err := os.ReadFile(f.state)
	if err != nil || !bytes.Contains(state, []byte("PRIVATE_PLAN_CANARY")) {
		t.Fatalf("real apply did not persist its state: %v", err)
	}
	assertPrivateArtifacts(t, f, p.ID)
	if _, err = f.engine.Submit(ctx, signed); err != nil {
		t.Fatal("identical import after apply failed", err)
	}
	current, _ := f.engine.Get(ctx, p.ID)
	if current.Revision != completed.Revision {
		t.Fatal("duplicate import changed completed rollout")
	}
	assertPrivateStorage(t, f)
}

func assertPrivateArtifacts(t *testing.T, f *fixture, id string) {
	t.Helper()
	metadata, err := f.engine.GetPlanMetadata(context.Background(), id)
	if err != nil || metadata.PlanBytes == 0 || metadata.ReviewBytes == 0 || metadata.OutputBytes == 0 {
		t.Fatalf("missing retained encrypted artifacts: %+v %v", metadata, err)
	}
	var checkpointed string
	err = f.engine.db.QueryRow(`SELECT group_concat(coalesce(output,'') || coalesce(error,''),'') FROM operation_outputs WHERE workflow_uuid=?`, id).Scan(&checkpointed)
	if err != nil || bytes.Contains([]byte(checkpointed), []byte("PRIVATE_PLAN_CANARY")) {
		t.Fatalf("private material entered DBOS checkpoints: %v", err)
	}
}

func assertPrivateStorage(t *testing.T, f *fixture) {
	t.Helper()
	for _, path := range []string{f.config.Directory, filepath.Join(f.config.Directory, "rollouts.sqlite"), filepath.Join(f.config.Directory, "identity.agekey")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("private storage permissions: %s %v", path, err)
		}
	}
}

func TestImportRefusesTamperChangedContentAndCompetingEnvironment(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	signed := f.signed(t, f.command)
	tampered := signed
	tampered.Payload = append([]byte{}, signed.Payload...)
	tampered.Payload[0] = '['
	_, err := f.engine.Submit(ctx, tampered)
	requireCode(t, err, "signature_refused")
	if _, err = f.engine.Submit(ctx, signed); err != nil {
		t.Fatal(err)
	}
	changed := f.command
	changed.Request.Configuration += "\n# changed"
	_, err = f.engine.Submit(ctx, f.signed(t, changed))
	requireCode(t, err, "command_id_conflict")
	changed = f.command
	changed.ID, changed.Request.ID = "rollout-2", "rollout-2"
	_, err = f.engine.Submit(ctx, f.signed(t, changed))
	requireCode(t, err, "environment_busy")
	_, err = Open(ctx, f.config)
	requireCode(t, err, "coordinator_busy")
}

func TestStartedApplyIsFencedOnRecoveryAndNeverRetried(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.engine.Submit(ctx, f.signed(t, f.command)); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
	if _, err := f.engine.db.Exec(`INSERT INTO rollout_effects(rollout,step,status) VALUES(?,'apply','started')`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Decide(ctx, p.ID, p.Subject, "approve", "reviewer"); err != nil {
		t.Fatal(err)
	}
	unknown := waitPhase(t, f.engine, p.ID, "outcome_unknown")
	if unknown.ErrorCode != "apply_outcome_unknown" {
		t.Fatalf("wrong failure: %+v", unknown)
	}
	if _, err := os.Stat(f.state); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("uncertain apply was rerun")
	}
	if _, err := f.engine.Cancel(ctx, p.ID, "operator"); err == nil {
		t.Fatal("cancel released an unknown effect")
	}
	changed := f.command
	changed.ID, changed.Request.ID = "replacement", "replacement"
	_, err := f.engine.Submit(ctx, f.signed(t, changed))
	requireCode(t, err, "environment_busy")
}

func addTask(t *testing.T, f *fixture, phase, retry, body string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python is not installed")
	}
	python, err = filepath.EvalSymlinks(python)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "task.py")
	if err = os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(python)
	if err != nil {
		t.Fatal(err)
	}
	f.config.Executor.Tools = []rolloutexec.Tool{{ID: "task", Name: "task", Phase: phase, Retry: retry, Definition: steps.Definition{
		ProtocolVersion: 1, Name: "task", Version: "1", Phase: phase, Retry: retry,
		Executable: script, ExecutableSHA256: digest([]byte(body)), Interpreter: python, InterpreterSHA256: digest(binary),
	}}}
	f.command.Steps = []rolloutexec.Step{{ID: "task-1", ToolID: "task", Parameters: json.RawMessage(`{}`)}}
	f.command.ToolsDigest = f.config.Executor.CatalogDigest()
}

func TestFaultCheckCannotAuthorizeApply(t *testing.T) {
	f := newFixture(t)
	addTask(t, f, "plan_check", "read_only", `import json,sys
r=json.load(sys.stdin)
print(json.dumps({'version':1,'task_id':r['task_id'],'effect_id':r['effect_id'],'decision':'fault','category':'execution','outputs':{}}))
`)
	if _, err := f.engine.Submit(context.Background(), f.signed(t, f.command)); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, f.engine, f.command.ID, "failed")
	if p.ErrorCode != "task_failed" || p.FailedStep != "task-1" {
		t.Fatalf("fault result was not retained safely: %+v", p)
	}
	if _, err := os.Stat(f.state); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fault check permitted apply")
	}
}

func TestReceiptPostRepairReusesEffectAndDoesNotApplyAgain(t *testing.T) {
	f := newFixture(t)
	receipt := filepath.Join(t.TempDir(), "receipt.json")
	addTask(t, f, "post_apply", "receipt", fmt.Sprintf(`import json,sys,pathlib
r=json.load(sys.stdin)
p=pathlib.Path(%q)
if p.exists():
 assert p.read_text()==r['effect_id']
 decision,category='allow','ok'
else:
 p.write_text(r['effect_id'])
 sys.exit(1)
print(json.dumps({'version':1,'task_id':r['task_id'],'effect_id':r['effect_id'],'decision':decision,'category':category,'outputs':{}}))
`, receipt))
	f.command.Approval = Approval{Mode: "policy", MaxAdd: 1}
	if _, err := f.engine.Submit(context.Background(), f.signed(t, f.command)); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, f.engine, f.command.ID, "post_failed")
	before, err := os.ReadFile(f.state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.Repair(context.Background(), p.ID, p.FailedStep, "operator"); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, f.engine, p.ID, "completed")
	after, err := os.ReadFile(f.state)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("post repair changed Terraform state")
	}
	var applies int
	if err = f.engine.db.QueryRow(`SELECT count(*) FROM rollout_effects WHERE rollout=? AND step='apply' AND status='done'`, p.ID).Scan(&applies); err != nil || applies != 1 {
		t.Fatalf("apply binding count: %d %v", applies, err)
	}
}

func TestPolicyBoundsRefuseUnexpectedPlan(t *testing.T) {
	f := newFixture(t)
	f.command.Approval = Approval{Mode: "policy"}
	if _, err := f.engine.Submit(context.Background(), f.signed(t, f.command)); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, f.engine, f.command.ID, "failed")
	if p.ErrorCode != "approval_policy_denied" {
		t.Fatalf("wrong policy result: %+v", p)
	}
	if _, err := os.Stat(f.state); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("policy permitted an unauthorized change")
	}
}

func TestExplicitUnknownTaskOutcomeIsNeverRepaired(t *testing.T) {
	f := newFixture(t)
	addTask(t, f, "post_apply", "receipt", `import json,sys
r=json.load(sys.stdin)
print(json.dumps({'version':1,'task_id':r['task_id'],'effect_id':r['effect_id'],'decision':'fault','category':'unknown_outcome','outputs':{}}))
`)
	f.command.Approval = Approval{Mode: "policy", MaxAdd: 1}
	if _, err := f.engine.Submit(context.Background(), f.signed(t, f.command)); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, f.engine, f.command.ID, "outcome_unknown")
	if p.ErrorCode != "task_outcome_unknown" {
		t.Fatalf("explicit unknown outcome was not fenced: %+v", p)
	}
	if _, err := f.engine.Repair(context.Background(), p.ID, "task-1", "operator"); err == nil {
		t.Fatal("explicit unknown outcome accepted repair")
	}
}

func TestDuplicateImportStartsPreviouslyCommittedCommand(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	signed := f.signed(t, f.command)
	tx, err := f.engine.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	encrypted, err := f.engine.encrypt(signed.Payload)
	require.NoError(t, err)
	if _, err = tx.Exec(`INSERT INTO rollout_commands(id,digest,payload,progress,version,environment,strategy) VALUES(?,?,?,?,?,?,?)`, f.command.ID, digest(signed.Payload), encrypted, "{}", f.command.Version, f.command.EnvironmentID, f.command.Strategy); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO rollout_environments(environment,rollout) VALUES(?,?)`, f.command.EnvironmentID, f.command.ID); err != nil {
		t.Fatal(err)
	}
	p := Progress{ID: f.command.ID, EnvironmentID: f.command.EnvironmentID, Phase: "accepted", CreatedAt: time.Now().UTC()}
	if err = saveProgress(ctx, tx, &p); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.Submit(ctx, signed); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, f.engine, p.ID, "awaiting_approval")
}

func assertReview(t *testing.T, p Progress) {
	t.Helper()
	if p.Subject == "" || p.Changes == nil || p.Changes.Added != 1 {
		t.Fatalf("missing review material: %+v", p)
	}
}

func TestPlanLogsAreEncryptedBoundAndSurviveRestart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.engine.Submit(ctx, f.signed(t, f.command)); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
	logs, err := f.engine.GetPlanLogs(ctx, p.ID, p.Subject)
	if err != nil || !bytes.Contains(logs.Content, []byte("terraform_data.marker")) || !bytes.Contains(logs.Content, []byte("PRIVATE_PLAN_CANARY")) {
		t.Fatalf("saved plan logs unavailable: %v", err)
	}
	if logs.Subject != p.Subject {
		t.Fatal("plan logs were not bound to approval")
	}
	var ciphertext []byte
	require.NoError(t, f.engine.db.QueryRowContext(ctx, `SELECT content FROM rollout_plan_logs WHERE rollout=?`, p.ID).Scan(&ciphertext))
	if !bytes.HasPrefix(ciphertext, []byte("age-encryption.org/v1")) || bytes.Contains(ciphertext, []byte("PRIVATE_PLAN_CANARY")) {
		t.Fatal("plaintext plan logs stored")
	}
	_, err = f.engine.GetPlanLogs(ctx, p.ID, "stale-subject")
	requireCode(t, err, "plan_logs_subject_changed")
	require.NoError(t, f.engine.saveFailedPlanLogs(ctx, p.ID, rolloutexec.Plan{Logs: []byte("replacement diagnostics")}))
	closeFixture(t, f.engine)
	f.engine, err = Open(ctx, f.config)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := f.engine.GetPlanLogs(ctx, p.ID, p.Subject)
	if err != nil || !bytes.Equal(logs.Content, restored.Content) {
		t.Fatal("logs changed after restart")
	}
	// Simulate a record written before log capture existed; never rerun its plan.
	if _, err = f.engine.db.ExecContext(ctx, `DELETE FROM rollout_plan_logs WHERE rollout=?`, p.ID); err != nil {
		t.Fatal(err)
	}
	_, err = f.engine.GetPlanLogs(ctx, p.ID, p.Subject)
	requireCode(t, err, "plan_logs_unavailable")
}

func TestFailedPlanPreservesEncryptedDiagnosticsWithoutApproval(t *testing.T) {
	f := newFixture(t)
	f.command.Request.Configuration = "resource \"terraform_data\" \"broken\" { input = }"
	ctx := context.Background()
	if _, err := f.engine.Submit(ctx, f.signed(t, f.command)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	var p Progress
	for time.Now().Before(deadline) {
		p, _ = f.engine.Get(ctx, f.command.ID)
		if terminal(p.Phase) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if p.Phase != "failed" || p.Subject != "" {
		t.Fatalf("invalid plan became approvable: %+v", p)
	}
	logs, err := f.engine.GetPlanLogs(ctx, p.ID, "")
	if err != nil || !bytes.Contains(logs.Content, []byte("Invalid expression")) {
		t.Fatalf("failure diagnostics missing: %v", err)
	}
	// Recovery may repeat a read-only failed plan before its workflow checkpoint.
	require.NoError(t, f.engine.saveFailedPlanLogs(ctx, p.ID, rolloutexec.Plan{Logs: logs.Content}))
}

func TestArtifactDecryptionNeverReturnsPartialOversizeContent(t *testing.T) {
	f := newFixture(t)
	oversized := bytes.Repeat([]byte("x"), (4<<20)+1)
	encrypted, err := f.engine.encrypt(oversized)
	require.NoError(t, err)
	plain, err := f.engine.decrypt(encrypted)
	requireCode(t, err, "artifact_too_large")
	if len(plain) != 0 {
		t.Fatal("partial decrypted artifact escaped")
	}
	closeFixture(t, f.engine)
	_, err = f.engine.GetPlanLogs(context.Background(), "missing", "")
	var refusal *Error
	if err == nil || errors.As(err, &refusal) {
		t.Fatal("database outage reported as missing logs")
	}
}
