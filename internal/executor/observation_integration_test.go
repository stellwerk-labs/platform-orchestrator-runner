//go:build integration

package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/limitedlogsbuffer"
	api "github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
	"github.com/stretchr/testify/require"
)

func TestObservationExecutorRealToolsProtectsResultsAndLogs(t *testing.T) {
	for _, backend := range []config.IaCBackendType{config.BackendOpenTofu, config.BackendTerraform} {
		t.Run(string(backend), func(t *testing.T) {
			for _, scenario := range []struct {
				name        string
				invalid     bool
				protocol    string
				logLevel    string
				disabled    bool
				costEntries int
			}{
				{"success", false, config.ObservationProtocolV1, "info", false, 1},
				{"error level opt-out", false, config.ObservationProtocolV1, "error", false, 0},
				{"cost disabled", false, config.ObservationProtocolV1, "info", true, 0},
				{"diagnostic failure", true, config.ObservationProtocolV1, "info", false, 0},
				{"ordinary plan_only", false, "", "info", false, 1},
			} {
				invalid := scenario.invalid
				t.Run(scenario.name, func(t *testing.T) {
					source := `resource "terraform_data" "SECRET_ADDRESS_CANARY" { input = "SECRET_CONTENT_CANARY" }
output "secret" {
 value = "SECRET_OUTPUT_CANARY"
 sensitive = true
}
`
					if invalid {
						source += "output \"SECRET_ERROR_CANARY\" { value = missing.SECRET_ERROR_CANARY }\n"
					}
					var bundle bytes.Buffer
					compressed := gzip.NewWriter(&bundle)
					archive := tar.NewWriter(compressed)
					require.NoError(t, archive.WriteHeader(&tar.Header{Name: "main.tf", Mode: 0600, Size: int64(len(source))}))
					_, err := archive.Write([]byte(source))
					require.NoError(t, err)
					require.NoError(t, archive.Close())
					require.NoError(t, compressed.Close())
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(bundle.Bytes()) }))
					defer server.Close()
					// SSH unix socket paths have a short OS limit, including on macOS.
					dir, err := os.MkdirTemp("", "obs-exec-")
					require.NoError(t, err)
					defer os.RemoveAll(dir)
					for key, value := range map[string]string{
						"ORG_ID": "org", "RUNNER_ID": "runner", "DEPLOYMENT_ID": "69aa008a-a60c-4542-8f83-fdc5bb0ae599",
						"MODE": "plan_only", "OBSERVATION_PROTOCOL": scenario.protocol, "IAC_BACKEND": string(backend), "IAC_CODE_DIR": dir, "TOKEN": "token", "RUNNER_GATEWAY_URL": server.URL,
						"LOG_LEVEL": scenario.logLevel, "COST_ESTIMATION_ENABLED": strconv.FormatBool(!scenario.disabled), "ENCRYPTING_LOGS_KEY": "",
					} {
						t.Setenv(key, value)
					}
					cfg, err := config.GetStandardModeConfiguration()
					require.NoError(t, err)
					require.Equal(t, config.PlanOnly, cfg.Mode)
					require.Equal(t, scenario.protocol != "", cfg.IsObservation())
					originalLogger := slog.Default()
					defer slog.SetDefault(originalLogger)
					originalStdout := os.Stdout
					read, write, err := os.Pipe()
					require.NoError(t, err)
					os.Stdout = write
					defer func() { os.Stdout = originalStdout; _ = read.Close() }()
					var logs bytes.Buffer
					programLevel := new(slog.LevelVar)
					require.NoError(t, programLevel.UnmarshalText([]byte(cfg.LogLevel)))
					result, executionErr := ExecuteStandardMode(t.Context(), cfg, runner.CreateRunner, limitedlogsbuffer.NewLimitedLogsBuffer(&logs, 1<<20), programLevel)
					os.Stdout = originalStdout
					require.NoError(t, write.Close())
					stdout, err := io.ReadAll(read)
					require.NoError(t, err)
					require.Empty(t, cfg.EncryptingLogsKey)
					require.Equal(t, scenario.costEntries, strings.Count(string(stdout), `"cost_estimation"`))
					require.Equal(t, scenario.costEntries, strings.Count(logs.String(), `"cost_estimation"`))
					encoded, err := json.Marshal(result)
					require.NoError(t, err)
					if cfg.IsObservation() {
						for _, secret := range []string{"SECRET_ADDRESS_CANARY", "SECRET_CONTENT_CANARY", "SECRET_OUTPUT_CANARY", "SECRET_ERROR_CANARY"} {
							require.NotContains(t, string(encoded), secret)
							require.NotContains(t, logs.String(), secret)
							require.NotContains(t, string(stdout), secret)
							if executionErr != nil {
								require.NotContains(t, executionErr.Error(), secret)
							}
						}
						require.Nil(t, result.Outputs)
						require.Empty(t, result.Metadata)
					} else {
						require.NotNil(t, result.Outputs)
						require.Nil(t, result.ObservationResult)
					}
					if invalid {
						require.Error(t, executionErr)
						require.Equal(t, api.Failure, result.Status)
						require.Nil(t, result.ObservationResult)
					} else {
						require.NoError(t, executionErr)
						require.Equal(t, api.Success, result.Status)
						if cfg.IsObservation() {
							require.NotNil(t, result.ObservationResult)
						} else {
							require.Nil(t, result.ObservationResult)
						}
					}
					_, err = os.Stat(filepath.Join(dir, "terraform.tfstate"))
					require.True(t, os.IsNotExist(err), "observation created persistent state")
					plans, err := filepath.Glob(filepath.Join(dir, ".observation-*"))
					require.NoError(t, err)
					require.Empty(t, plans)
				})
			}
		})
	}
}
