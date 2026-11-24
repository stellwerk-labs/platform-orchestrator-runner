//go:build integration

package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/limitedlogsbuffer"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
	"github.com/stretchr/testify/require"
)

func TestRetainedExecutorNative(t *testing.T) {
	tofu, err := exec.LookPath("tofu")
	if err != nil {
		t.Skip("tofu unavailable")
	}
	root, err := os.MkdirTemp("", "retained-")
	require.NoError(t, err)
	defer os.RemoveAll(root)
	dir := filepath.Join(root, "work")
	require.NoError(t, os.Mkdir(dir, 0700))
	bin := filepath.Join(root, "bin")
	require.NoError(t, os.Mkdir(bin, 0700))
	marker := filepath.Join(root, "plans")
	script := "#!/bin/sh\ncase \" $* \" in *' plan '*) echo plan >> '" + marker + "';; esac\nexec '" + tofu + "' \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "tofu"), []byte(script), 0700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	binding := runner.RetainedBinding{Schema: "stellwerk.retained-plan/v1", OrgID: "org", EnvironmentID: "env", RunnerID: "runner", SourceDeploymentID: "plan", BaseDeploymentID: "base", Backend: "local", Tool: "opentofu"}
	bindingJSON, err := json.Marshal(binding)
	require.NoError(t, err)
	source := `terraform {
 backend "local" {
  path = "` + filepath.Join(root, "state.tfstate") + `"
 }
}
resource "terraform_data" "test" { input = "SECRET_RETAINED_CANARY" }
output "platform_orchestrator_metadata" { value = {} }
`
	bundle := func(name string, data []byte) []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data))}))
		_, e := tw.Write(data)
		require.NoError(t, e)
		require.NoError(t, tw.Close())
		require.NoError(t, gz.Close())
		return buf.Bytes()
	}
	payload := bundle("main.tf", []byte(source))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/retained-plan-key") {
			_ = json.NewEncoder(w).Encode(map[string]string{"identity": id.String()})
			return
		}
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	cfg := &config.StandardModeConfiguration{OrgID: "org", RunnerID: "runner", DeploymentID: "plan", DeploymentEnvUUID: "env", Mode: config.PlanOnly, RetainedPlansEnabled: true, RetainPlan: true, RetainedBinding: string(bindingJSON), EncryptingKey: id.Recipient().String(), IaCCodeDir: dir, IaCBackend: config.BackendOpenTofu, MetadataKey: "platform_orchestrator_metadata", DeploymentToken: "fixture", Gateway: config.GatewayClientConfiguration{URL: server.URL}}
	original := slog.Default()
	defer slog.SetDefault(original)
	var logs bytes.Buffer
	level := new(slog.LevelVar)
	result, err := ExecuteStandardMode(t.Context(), cfg, runner.CreateRunner, limitedlogsbuffer.NewLimitedLogsBuffer(&logs, 1<<20), level)
	require.NoError(t, err)
	require.NotNil(t, result.Outputs)
	require.NotContains(t, logs.String(), "SECRET_RETAINED_CANARY")
	cipher, err := base64.StdEncoding.DecodeString(*result.Outputs)
	require.NoError(t, err)
	sum := sha256.Sum256(cipher)
	payload = bundle(".stellwerk-retained-plan-v1.age", cipher)
	require.NoError(t, os.RemoveAll(dir))
	require.NoError(t, os.Mkdir(dir, 0700))
	cfg.Mode = config.ApplyRetained
	cfg.RetainPlan = false
	cfg.DeploymentID = "apply"
	cfg.RetainedSHA256 = hex.EncodeToString(sum[:])
	logs.Reset()
	result, err = ExecuteStandardMode(t.Context(), cfg, runner.CreateRunner, limitedlogsbuffer.NewLimitedLogsBuffer(&logs, 1<<20), level)
	require.NoError(t, err)
	require.NotNil(t, result.TfResourceCounts)
	require.NotContains(t, logs.String(), "SECRET_RETAINED_CANARY")
	plans, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, "plan\n", string(plans))
	state, err := os.ReadFile(filepath.Join(root, "state.tfstate"))
	require.NoError(t, err)
	require.Contains(t, string(state), "SECRET_RETAINED_CANARY")
}
