//go:build integration

package rolloutexec

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"filippo.io/age"
)

func TestLocalRetainedApplyUsesActualIaCWithoutReplanning(t *testing.T) {
	for _, tool := range []string{"opentofu", "terraform"} {
		t.Run(tool, func(t *testing.T) { nativeRetainedApply(t, tool) })
	}
}

func nativeRetainedApply(t *testing.T, tool string) {
	t.Helper()
	e, request := fixture(t)
	request.Tool = tool
	program := tool
	if program == "opentofu" {
		program = "tofu"
	}
	binary, err := exec.LookPath(program)
	if err != nil {
		t.Skip("IaC binary is not installed")
	}
	root := t.TempDir()
	trace := filepath.Join(root, "commands")
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	wrapper := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + quote(trace) + "\nexec " + quote(binary) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, program), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TF_CLI_ARGS_show", "-json")
	if tool == "opentofu" {
		t.Setenv("TF_CLI_ARGS_show", "-json -show-sensitive")
	}
	state := filepath.Join(root, "state.tfstate")
	request.Configuration = "terraform {\n backend \"local\" {\n path = " + strconv.Quote(state) + "\n }\n}\nresource \"terraform_data\" \"approved\" { input = \"approved\" }\noutput \"value\" { value = terraform_data.approved.output }\noutput \"secret\" {\n value = \"PRIVATE_SENSITIVE_LOG_CANARY\"\n sensitive = true\n }\n"
	plan, err := e.Plan(context.Background(), request)
	if err != nil || plan.Changes.Added != 1 || !json.Valid(plan.Review) {
		t.Fatalf("actual %s plan failed: %v", tool, err)
	}
	if !strings.Contains(string(plan.Logs), "terraform_data.approved") || !strings.Contains(string(plan.Logs), "approved") || strings.Contains(string(plan.Logs), "PRIVATE_SENSITIVE_LOG_CANARY") || !strings.Contains(string(plan.Logs), "(sensitive value)") {
		t.Fatal("human plan detail or sensitive masking lost")
	}
	if _, err := e.Apply(context.Background(), request, plan.Ciphertext, strings.Repeat("0", 64)); err == nil {
		t.Fatal("accepted bad ciphertext digest")
	}
	wrong := request
	wrong.EnvironmentID = "different-environment"
	if _, err := e.Apply(context.Background(), wrong, plan.Ciphertext, bytesDigest(plan.Ciphertext)); err == nil {
		t.Fatal("accepted wrong environment binding")
	}
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	restarted := *e
	restarted.Identity = other
	if _, err := restarted.Apply(context.Background(), request, plan.Ciphertext, bytesDigest(plan.Ciphertext)); err == nil {
		t.Fatal("accepted wrong site key")
	}
	restarted.Identity = e.Identity
	request.Configuration = strings.ReplaceAll(request.Configuration, "approved", "unapproved")
	outputs, err := restarted.Apply(context.Background(), request, plan.Ciphertext, bytesDigest(plan.Ciphertext))
	if err != nil {
		t.Fatalf("actual %s saved apply failed: %v", tool, err)
	}
	var values map[string]string
	if json.Unmarshal(outputs, &values) != nil || values["value"] != "approved" {
		t.Fatalf("apply did not use the original saved configuration: %s", outputs)
	}
	commands, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	for operation, expected := range map[string]int{"init": 1, "plan": 1, "apply": 1, "output": 1} {
		count := 0
		for _, line := range strings.Split(string(commands), "\n") {
			fields := strings.Fields(line)
			if len(fields) > 1 && fields[1] == operation {
				count++
			}
		}
		if count != expected {
			t.Fatalf("%s ran %d times, expected %d", operation, count, expected)
		}
	}
}
