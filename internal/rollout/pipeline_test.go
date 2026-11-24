package rollout

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	api "github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stretchr/testify/require"
)

func canonicalFixture(t *testing.T, strategy, tool string) (*fixture, string, *age.X25519Identity) {
	t.Helper()
	f := newFixture(t)
	program := tool
	if tool == "opentofu" {
		program = "tofu"
	}
	binary, err := exec.LookPath(program)
	if err != nil {
		t.Skip("IaC tool not installed")
	}
	bin := t.TempDir()
	trace := filepath.Join(bin, "trace")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + quote(trace) + "\nexec " + quote(binary) + " \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, program), []byte(script), 0700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TF_CLI_ARGS_apply", "-destroy")
	recipient, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	main := fmt.Sprintf("terraform {\n backend \"local\" { path = %q }\n}\nmodule \"marker\" { source = \"./modules/marker/v1\" }\noutput \"value\" { value = module.marker.value }\noutput \"platform_orchestrator_metadata\" { value = { node = { installed = true } } }\n", f.state)
	f.command.Request.Bundle = executionBundle(t, map[string]string{"main.tf": main, "modules/marker/v1/main.tf": "resource \"terraform_data\" \"marker\" { input = \"PRIVATE_PIPELINE_VALUE\" }\noutput \"value\" { value = terraform_data.marker.output }\n"})
	f.command.Request.BundleSHA256 = digest(f.command.Request.Bundle)
	f.command.Request.Configuration = ""
	f.command.Request.Tool = tool
	f.command.Request.Mode = "deploy"
	if strategy == "plan" {
		f.command.Request.Mode = "plan_only"
	}
	if strategy == "destroy" {
		f.command.Request.Mode = "destroy"
	}
	f.command.Request.EncryptedOutputsRecipient = recipient.Recipient().String()
	f.command.Request.DeploymentToken = "fixture-deployment-token"
	f.command.Version, f.command.Strategy = PipelineVersion, strategy
	f.command.Approval = Approval{}
	if strategy == "reviewed" {
		f.command.Approval = Approval{Mode: "local"}
	}
	f.command.ToolsDigest = f.config.Executor.CatalogDigest()
	return f, trace, recipient
}

func executionBundle(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	writer := tar.NewWriter(gz)
	for name, body := range files {
		require.NoError(t, writer.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(body))}))
		_, err := writer.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	require.NoError(t, gz.Close())
	return data.Bytes()
}

func operationCount(t *testing.T, trace, operation string) int {
	t.Helper()
	data, err := os.ReadFile(trace)
	require.NoError(t, err)
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && fields[1] == operation {
			count++
		}
	}
	return count
}

func TestCanonicalStrategiesUseActualCompiledClosure(t *testing.T) {
	for _, test := range []struct{ tool, strategy, mode string }{
		{"opentofu", "apply", "deploy"}, {"opentofu", "plan", "plan_only"}, {"opentofu", "reviewed", "deploy"},
		{"terraform", "apply", "deploy"}, {"terraform", "plan", "plan_only"}, {"terraform", "reviewed", "deploy"},
		{"opentofu", "apply", "rollback"}, {"opentofu", "reviewed", "rollback"},
		{"terraform", "apply", "rollback"}, {"terraform", "reviewed", "rollback"},
	} {
		t.Run(test.tool+"/"+test.strategy+"/"+test.mode, func(t *testing.T) { canonicalStrategy(t, test.tool, test.strategy, test.mode) })
	}
}

func canonicalStrategy(t *testing.T, tool, strategy, mode string) {
	t.Helper()
	f, trace, recipient := canonicalFixture(t, strategy, tool)
	f.command.Request.Mode = mode
	ctx := context.Background()
	_, err := f.engine.Submit(ctx, f.signed(t, f.command))
	require.NoError(t, err)
	if strategy == "reviewed" {
		p := waitPhase(t, f.engine, f.command.ID, "awaiting_approval")
		_, err = f.engine.Decide(ctx, p.ID, p.Subject, "approve", "operator")
		require.NoError(t, err)
	}
	p := waitPhase(t, f.engine, f.command.ID, "completed")
	expectedPlan, expectedApply := 0, 1
	if strategy != "apply" {
		expectedPlan = 1
	}
	if strategy == "plan" {
		expectedApply = 0
	}
	require.Equal(t, expectedPlan, operationCount(t, trace, "plan"))
	require.Equal(t, expectedApply, operationCount(t, trace, "apply"))
	require.Equal(t, strategy != "plan", p.InfrastructureApplied)
	require.Equal(t, 1, p.Changes.Added)
	token, body, ready, err := f.engine.Completion(ctx, p)
	require.NoError(t, err)
	require.True(t, ready)
	require.Equal(t, "fixture-deployment-token", token)
	var result api.DeploymentResultsUpdateBody
	require.NoError(t, json.Unmarshal(body, &result))
	require.Equal(t, api.Success, result.Status)
	require.NotContains(t, string(body), "PRIVATE_PIPELINE_VALUE")
	require.NotNil(t, result.Outputs)
	cipher, err := base64.StdEncoding.DecodeString(*result.Outputs)
	require.NoError(t, err)
	plain, err := age.Decrypt(bytes.NewReader(cipher), recipient)
	require.NoError(t, err)
	outputs, err := io.ReadAll(plain)
	require.NoError(t, err)

	if strategy != "plan" {
		require.Contains(t, string(outputs), "PRIVATE_PIPELINE_VALUE")
		require.Len(t, result.Metadata, 1)
	}
	logs, err := f.engine.GetExecutionLogs(ctx, p.ID, "")
	require.NoError(t, err)
	require.NotEmpty(t, logs.Content)
}

func TestCanonicalDirectPostRepairKeepsOneApplyAndResult(t *testing.T) {
	f, trace, _ := canonicalFixture(t, "apply", "opentofu")
	receipt := filepath.Join(t.TempDir(), "receipt")
	addTask(t, f, "post_apply", "receipt", fmt.Sprintf(`import json,sys,pathlib
r=json.load(sys.stdin)
assert 'plan' not in r['inputs']
p=pathlib.Path(%q)
if not p.exists():
 p.write_text(r['effect_id'])
 sys.exit(1)
assert p.read_text()==r['effect_id']
print(json.dumps({'version':1,'task_id':r['task_id'],'effect_id':r['effect_id'],'decision':'allow','category':'ok'}))`, receipt))
	ctx := context.Background()
	_, err := f.engine.Submit(ctx, f.signed(t, f.command))
	require.NoError(t, err)
	p := waitPhase(t, f.engine, f.command.ID, "post_failed")
	require.True(t, p.InfrastructureApplied)
	_, _, ready, err := f.engine.Completion(ctx, p)
	require.NoError(t, err)
	require.False(t, ready)
	closeFixture(t, f.engine)
	f.engine, err = Open(ctx, f.config)
	require.NoError(t, err)
	_, err = f.engine.Repair(ctx, p.ID, p.FailedStep, "operator")
	require.NoError(t, err)
	p = waitPhase(t, f.engine, p.ID, "completed")
	require.True(t, p.InfrastructureApplied)
	require.Equal(t, 0, operationCount(t, trace, "plan"))
	require.Equal(t, 1, operationCount(t, trace, "apply"))
	_, _, ready, err = f.engine.Completion(ctx, p)
	require.NoError(t, err)
	require.True(t, ready)
}

func TestCanonicalPlanIsReadOnlyAndDirectUnknownIsFenced(t *testing.T) {
	f, trace, _ := canonicalFixture(t, "apply", "opentofu")
	ctx := context.Background()
	signed := f.signed(t, f.command)
	_, err := f.engine.importCommand(ctx, f.command, signed.Payload)
	require.NoError(t, err)
	require.NoError(t, f.engine.phase(ctx, f.command.ID, "preparing"))
	_, err = f.engine.claim(ctx, f.command.ID, "apply", true)
	require.NoError(t, err)
	closeFixture(t, f.engine)
	f.engine, err = Open(ctx, f.config)
	require.NoError(t, err)
	p := waitPhase(t, f.engine, f.command.ID, "outcome_unknown")
	require.Equal(t, "apply_outcome_unknown", p.ErrorCode)
	require.Equal(t, 0, operationCount(t, trace, "apply"))
	replacement := f.command
	replacement.ID = "replacement"
	replacement.Request.ID = replacement.ID
	_, err = f.engine.Submit(ctx, f.signed(t, replacement))
	requireCode(t, err, "environment_busy")
	replacement.ID = "manual-plan"
	replacement.Request.ID = replacement.ID
	replacement.Strategy = "plan"
	replacement.Request.Mode = "plan_only"
	_, err = f.engine.Submit(ctx, f.signed(t, replacement))
	require.NoError(t, err)
	p = waitPhase(t, f.engine, replacement.ID, "completed")
	require.False(t, p.InfrastructureApplied)
	require.Equal(t, 1, operationCount(t, trace, "plan"))
	require.Equal(t, 0, operationCount(t, trace, "apply"))
	var holder string
	require.NoError(t, f.engine.db.QueryRowContext(ctx, `SELECT rollout FROM rollout_environments WHERE environment=?`, f.command.EnvironmentID).Scan(&holder))
	require.Equal(t, f.command.ID, holder)
}

func TestCanonicalSignedStrategiesCannotEscapeAdmittedMode(t *testing.T) {
	f, _, _ := canonicalFixture(t, "apply", "opentofu")
	for _, test := range []struct{ strategy, mode string }{{"apply", "plan_only"}, {"reviewed", "plan_only"}, {"plan", "deploy"}, {"destroy", "deploy"}, {"", "deploy"}, {"apply_retained", "deploy"}} {
		command := f.command
		command.Strategy = test.strategy
		command.Request.Mode = test.mode
		_, err := f.engine.Submit(context.Background(), f.signed(t, command))
		require.Error(t, err)
	}
}

func TestCanonicalDestroyReusesStateAndDoesNotAddPlanStep(t *testing.T) {
	f, trace, _ := canonicalFixture(t, "apply", "opentofu")
	ctx := context.Background()
	_, err := f.engine.Submit(ctx, f.signed(t, f.command))
	require.NoError(t, err)
	waitPhase(t, f.engine, f.command.ID, "completed")
	command := f.command
	command.ID = "destroy"
	command.Request.ID = command.ID
	command.Strategy = "destroy"
	command.Request.Mode = "destroy"
	_, err = f.engine.Submit(ctx, f.signed(t, command))
	require.NoError(t, err)
	p := waitPhase(t, f.engine, command.ID, "completed")
	require.Equal(t, 1, p.Changes.Remove)
	require.Equal(t, 0, operationCount(t, trace, "plan"))
	require.Equal(t, 1, operationCount(t, trace, "destroy"))
}
