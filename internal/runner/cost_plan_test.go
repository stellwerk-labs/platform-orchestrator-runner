package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCostOutputCoverageAndDecimal(t *testing.T) {
	data := []byte(`{"version":"0.2","currency":"USD","totalMonthlyCost":"12.34567890123456789","summary":{"totalSupportedResources":2,"totalNoPriceResources":1},"projects":[{"breakdown":{"resources":[{"costComponents":[{"monthlyCost":"12.34567890123456789","monthlyQuantity":"100"}],"subresources":[{"costComponents":[{"usageBased":true,"monthlyCost":null,"monthlyQuantity":null}]}]},{"costComponents":[{"monthlyCost":null,"priceNotFound":true}]}]}}]}`)
	result, reason := parseCostOutput(data, costEstimate{Resources: 5})
	require.Empty(t, reason)
	require.Equal(t, "12.34567890123456789", result.Monthly)
	require.Equal(t, 1, result.Priced)
	require.Equal(t, 1, result.Free)
	require.Equal(t, 2, result.Unpriced)
	require.Equal(t, 1, result.Unknown)
	require.Equal(t, 1, result.MissingUsage)
	for _, data := range []string{`{"version":"0.2","currency":"USD","totalMonthlyCost":"0","summary":{}}`, `{"version":"wrong","currency":"USD"}`, `{"version":"0.2","currency":"EUR"}`, `{"version":"0.2","version":"0.2","currency":"USD"}`} {
		result, _ := parseCostOutput([]byte(data), costEstimate{Resources: 1})
		require.Empty(t, result.Monthly, "unknown costs must never become a free zero")
	}
}

func TestCostSensitiveResourcesStayPrivate(t *testing.T) {
	root := map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(`{"resources":[{"mode":"managed","values":{"instance_type":"SECRET"},"sensitive_values":{"instance_type":true}},{"mode":"managed","values":{"size":100}},{"mode":"data"}],"child_modules":[{"resources":[{"mode":"managed"}]}]}`), &root))
	var result costEstimate
	require.True(t, sanitizeCostModule(root, &result, 0))
	require.Equal(t, 3, result.Resources)
	require.Equal(t, 1, result.Unknown)
	data, err := json.Marshal(root)
	require.NoError(t, err)
	require.NotContains(t, string(data), "SECRET")
}

func TestCostMissingKeyAndFailureLogsContainNoSecrets(t *testing.T) {
	original := slog.Default()
	t.Cleanup(func() { slog.SetDefault(original) })
	for _, key := range []string{"", "KEY_SECRET"} {
		var logs bytes.Buffer
		slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
		r := baseRunner{costEstimationEnabled: true, infracostAPIKey: key, costBinary: "/missing/infracost"}
		r.logCostEstimate(context.Background(), []byte(`{"planned_values":{"root_module":{"resources":[]}},"SECRET":"SECRET_PLAN"}`))
		require.Contains(t, logs.String(), `"status":"unknown"`)
		require.NotContains(t, logs.String(), "KEY_SECRET")
		require.NotContains(t, logs.String(), "SECRET_PLAN")
		require.NotContains(t, logs.String(), "/missing/")
	}
}

func TestInfrastructureCommandsCannotInheritInfracostKey(t *testing.T) {
	t.Setenv("INFRACOST_API_KEY", "PRIVATE_INFRACOST_KEY")
	t.Setenv("TF_VAR_provider_credential", "LEGITIMATE_PROVIDER_CREDENTIAL")
	output, err := runCmd(t.Context(), "/usr/bin/env")
	require.NoError(t, err)
	require.NotContains(t, output, "PRIVATE_INFRACOST_KEY")
	require.NotContains(t, output, "INFRACOST_API_KEY")
	require.Contains(t, output, "TF_VAR_provider_credential=LEGITIMATE_PROVIDER_CREDENTIAL")
	r := baseRunner{binaryPath: "/usr/bin/env"}
	observed, err := r.observationCommand(t.Context(), 1<<20)
	require.NoError(t, err)
	require.NotContains(t, string(observed), "INFRACOST_API_KEY")
}
