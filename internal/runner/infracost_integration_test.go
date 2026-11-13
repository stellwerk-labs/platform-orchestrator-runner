//go:build integration

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const infracostPlanFixture = `{"format_version":"1.2","terraform_version":"1.13.2","planned_values":{"root_module":{"resources":[{"address":"aws_ebs_volume.PRIVATE_ADDRESS","mode":"managed","type":"aws_ebs_volume","name":"PRIVATE_ADDRESS","provider_name":"registry.terraform.io/hashicorp/aws","values":{"region":"eu-central-1","availability_zone":"eu-central-1a","type":"gp3","size":100,"iops":3000,"throughput":125,"tags":{"PRIVATE_TAG":"PRIVATE_VALUE"}}},{"address":"terraform_data.private","mode":"managed","type":"terraform_data","name":"private","provider_name":"terraform.io/builtin/terraform","values":{"input":"PRIVATE_INPUT"}},{"address":"aws_ebs_volume.sensitive","mode":"managed","type":"aws_ebs_volume","name":"sensitive","provider_name":"registry.terraform.io/hashicorp/aws","values":{"region":"SECRET_REGION","size":100},"sensitive_values":{"region":true}}]}},"configuration":{"provider_config":{"aws":{"name":"aws","full_name":"registry.terraform.io/hashicorp/aws","expressions":{"region":{"constant_value":"eu-central-1"}}}},"root_module":{"resources":[{"address":"aws_ebs_volume.PRIVATE_ADDRESS","mode":"managed","type":"aws_ebs_volume","name":"PRIVATE_ADDRESS","provider_config_key":"aws"}]}}}`

func TestInfracostBinaryPricingAndPrivacy(t *testing.T) {
	binary := os.Getenv("INFRACOST_TEST_BINARY")
	if binary == "" {
		binary = infracostBinary
	}
	require.FileExists(t, binary, "this gate requires the actual pinned source-built CLI")
	original := slog.Default()
	t.Cleanup(func() { slog.SetDefault(original) })
	t.Setenv("INFRACOST_ENV", "test") // The isolated child must not inherit testing mode.
	t.Setenv("AWS_ACCESS_KEY_ID", "PRIVATE_CLOUD_KEY")
	t.Setenv("INFRACOST_EVENTSDISABLED", "false")
	t.Setenv("INFRACOST_ENABLE_CLOUD", "true")
	t.Setenv("INFRACOST_ENABLE_CLOUD_UPLOAD", "true")
	t.Setenv("INFRACOST_SKIP_UPDATE_CHECK", "false")
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	var mu sync.Mutex
	paths := []string{}
	bodies := []string{}
	auth := []string{}
	fileModes := []os.FileMode{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, r.URL.Path)
		bodies = append(bodies, string(body))
		auth = append(auth, r.Header.Get("X-Api-Key"))
		dirs, _ := filepath.Glob(filepath.Join(temp, "runner-cost-*"))
		for _, dir := range dirs {
			for _, p := range []string{dir, filepath.Join(dir, "plan.json")} {
				if info, err := os.Stat(p); err == nil {
					fileModes = append(fileModes, info.Mode().Perm())
				}
			}
		}
		var queries []json.RawMessage
		if r.URL.Path != "/graphql" || json.Unmarshal(body, &queries) != nil {
			http.Error(w, "PRIVATE_ERROR", 400)
			return
		}
		response := make([]json.RawMessage, len(queries))
		for i := range response {
			response[i] = json.RawMessage(`{"data":{"products":[{"prices":[{"priceHash":"fixture","USD":"0.1"}]}]}}`)
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	t.Setenv("INFRACOST_PRICING_API_ENDPOINT", "https://poison.invalid")
	t.Setenv("INFRACOST_DASHBOARD_API_ENDPOINT", server.URL)
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	r := baseRunner{costEstimationEnabled: true, infracostAPIKey: "CUSTOMER_TEST_KEY", costBinary: binary, costEndpoint: server.URL}
	r.logCostEstimate(t.Context(), []byte(infracostPlanFixture))
	require.Contains(t, logs.String(), `"estimated_monthly_subtotal":"10"`)
	require.Contains(t, logs.String(), `"resources":3`)
	require.Contains(t, logs.String(), `"unknown_resources":1`)
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, paths)
	for i, path := range paths {
		require.Equal(t, "/graphql", path)
		require.Equal(t, "CUSTOMER_TEST_KEY", auth[i])
		for _, secret := range []string{"PRIVATE_", "CUSTOMER_TEST_KEY", "SECRET_REGION", "planned_values", "resource_changes"} {
			require.NotContains(t, bodies[i], secret)
		}
	}
	require.Contains(t, fileModes, os.FileMode(0700))
	require.Contains(t, fileModes, os.FileMode(0600))
	for _, secret := range []string{"PRIVATE_", "CUSTOMER_TEST_KEY", "SECRET_REGION"} {
		require.NotContains(t, logs.String(), secret)
	}
	dirs, err := filepath.Glob(filepath.Join(temp, "runner-cost-*"))
	require.NoError(t, err)
	require.Empty(t, dirs)
}

func TestInfracostBinaryFailureAndCancellation(t *testing.T) {
	binary := os.Getenv("INFRACOST_TEST_BINARY")
	if binary == "" {
		binary = infracostBinary
	}
	require.FileExists(t, binary)
	original := slog.Default()
	t.Cleanup(func() { slog.SetDefault(original) })
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "api_error", true: "cancelled"}[cancelled], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if cancelled {
					select {
					case <-r.Context().Done():
					case <-time.After(time.Second):
					}
					return
				}
				require.Equal(t, "/graphql", r.URL.Path)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"PRIVATE_DIAGNOSTIC"}`)
			}))
			defer server.Close()
			r := baseRunner{costEstimationEnabled: true, infracostAPIKey: "PRIVATE_CUSTOMER_KEY", costBinary: binary, costEndpoint: server.URL}
			var logs bytes.Buffer
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			if cancelled {
				ctx, cancel = context.WithTimeout(ctx, 150*time.Millisecond)
				defer cancel()
			}
			r.logCostEstimate(ctx, []byte(infracostPlanFixture))
			require.Contains(t, logs.String(), `"status":"unknown"`)
			require.NotContains(t, logs.String(), "PRIVATE_")
			require.Equal(t, 1, strings.Count(logs.String(), `"cost_estimation"`))
		})
	}
}

func TestInfracostSensitiveProviderRegionNeverQueriesPrices(t *testing.T) {
	binary := os.Getenv("INFRACOST_TEST_BINARY")
	if binary == "" {
		binary = infracostBinary
	}
	require.FileExists(t, binary)
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		http.Error(w, "unexpected pricing call", 500)
	}))
	defer server.Close()
	var plan map[string]any
	require.NoError(t, json.Unmarshal([]byte(infracostPlanFixture), &plan))
	plan["variables"] = map[string]any{"pricing_region": map[string]any{"value": "SENSITIVE_REGION_CANARY"}}
	configuration := plan["configuration"].(map[string]any)
	root := configuration["root_module"].(map[string]any)
	root["variables"] = map[string]any{"pricing_region": map[string]any{"sensitive": true}}
	provider := configuration["provider_config"].(map[string]any)["aws"].(map[string]any)
	provider["expressions"] = map[string]any{"region": map[string]any{"references": []any{"var.pricing_region"}}}
	data, err := json.Marshal(plan)
	require.NoError(t, err)
	r := baseRunner{infracostAPIKey: "TEST_KEY", costBinary: binary, costEndpoint: server.URL}
	_, reason := r.estimateCosts(t.Context(), data)
	require.Equal(t, "sensitive_pricing_configuration", reason)
	mu.Lock()
	defer mu.Unlock()
	require.Zero(t, requests)
}

func TestInfracostBinaryBuildIdentity(t *testing.T) {
	binary := os.Getenv("INFRACOST_TEST_BINARY")
	if binary == "" {
		binary = infracostBinary
	}
	require.FileExists(t, binary)
	dir := t.TempDir()
	cmd := exec.CommandContext(t.Context(), binary, "--version")
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + dir, "XDG_CONFIG_HOME=" + dir, "XDG_CACHE_HOME=" + dir, "INFRACOST_SKIP_UPDATE_CHECK=true", "INFRACOST_DISABLE_ENVFILE=true", "INFRACOST_EVENTSDISABLED=true", "INFRACOST_ENABLE_CLOUD=false", "INFRACOST_ENABLE_CLOUD_UPLOAD=false", "INFRACOST_ENABLE_DASHBOARD=false"}
	output, err := cmd.Output()
	require.NoError(t, err)
	require.Equal(t, "Infracost "+infracostBuild, strings.TrimSpace(string(output)))
}
