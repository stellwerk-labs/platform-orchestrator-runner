//go:build integration

package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	api "github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stretchr/testify/require"
)

// Supply the binary built from immutable published stable b7ae8ca3. This proves
// the compatible wire combination cannot apply even before observation support.
func TestObservationLegacyRunnerPlanOnlyWireCannotApply(t *testing.T) {
	binary := os.Getenv("OBSERVATION_LEGACY_RUNNER_BINARY")
	if binary == "" {
		t.Skip("set OBSERVATION_LEGACY_RUNNER_BINARY to the immutable stable Runner binary")
	}
	for _, backend := range []string{"opentofu", "terraform"} {
		t.Run(backend, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			dir, err := os.MkdirTemp("", "legacy-obs-")
			require.NoError(t, err)
			defer os.RemoveAll(dir)
			marker := filepath.Join(dir, "APPLY_MUST_NOT_RUN")
			source := fmt.Sprintf(`resource "terraform_data" "proof" {
 input = "fixture"
 provisioner "local-exec" { command = %q }
}
`, "touch "+marker)
			var bundle bytes.Buffer
			gzipWriter := gzip.NewWriter(&bundle)
			archive := tar.NewWriter(gzipWriter)
			require.NoError(t, archive.WriteHeader(&tar.Header{Name: "main.tf", Mode: 0600, Size: int64(len(source))}))
			_, err = archive.Write([]byte(source))
			require.NoError(t, err)
			require.NoError(t, archive.Close())
			require.NoError(t, gzipWriter.Close())
			results := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = w.Write(bundle.Bytes())
					return
				}
				if r.Method == http.MethodPost {
					body, _ := io.ReadAll(r.Body)
					results <- body
					w.WriteHeader(http.StatusAccepted)
					return
				}
				w.WriteHeader(http.StatusMethodNotAllowed)
			}))
			defer server.Close()
			command := exec.CommandContext(ctx, binary, "standard")
			override := map[string]string{"ORG_ID": "legacy-fixture-org", "RUNNER_ID": "legacy-fixture-runner", "DEPLOYMENT_ID": "69aa008a-a60c-4542-8f83-fdc5bb0ae599", "MODE": "plan_only", "OBSERVATION_PROTOCOL": config.ObservationProtocolV1, "IAC_BACKEND": backend, "IAC_CODE_DIR": dir, "TOKEN": "fixture-token", "RUNNER_GATEWAY_URL": server.URL, "ENCRYPTING_KEY": "", "ENCRYPTING_LOGS_KEY": ""}
			for _, entry := range os.Environ() {
				key, _, _ := strings.Cut(entry, "=")
				if _, exists := override[key]; !exists {
					command.Env = append(command.Env, entry)
				}
			}
			for key, value := range override {
				command.Env = append(command.Env, key+"="+value)
			}
			output, err := command.CombinedOutput()
			require.NoError(t, err, "legacy Runner execution: %s", output)
			select {
			case body := <-results:
				var result api.DeploymentResultsUpdateBody
				require.NoError(t, json.Unmarshal(body, &result))
				require.Equal(t, api.Success, result.Status)
				require.Nil(t, result.ObservationResult, "legacy result must never be interpreted as authoritative clean observation")
				require.Equal(t, 1, result.TfResourceCounts.NumResourcesAdded)
			case <-ctx.Done():
				t.Fatal("legacy Runner did not publish its ordinary plan result")
			}
			// The shipped public entry point already rejects unknown MODE values;
			// the executor fallthrough alone is not evidence of an exploitable CLI bug.
			unsupported := exec.CommandContext(ctx, binary, "standard")
			for _, entry := range command.Env {
				key, _, _ := strings.Cut(entry, "=")
				if key != "MODE" {
					unsupported.Env = append(unsupported.Env, entry)
				}
			}
			unsupported.Env = append(unsupported.Env, "MODE=observation")
			rejected, err := unsupported.CombinedOutput()
			require.Error(t, err)
			require.Contains(t, string(rejected), "mode observation is not supported")
			for _, name := range []string{marker, filepath.Join(dir, "terraform.tfstate")} {
				_, err := os.Stat(name)
				require.True(t, os.IsNotExist(err), "legacy Runner applied or wrote persistent state: %s", name)
			}
		})
	}
}
