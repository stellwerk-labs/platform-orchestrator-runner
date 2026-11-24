//go:build integration

package runner

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The actual pinned CLI computes these costs. Only the pricing catalogue is a
// deterministic owned fixture; this does not claim live hosted-pricing proof.
func TestInfracostBinaryBeforeAfterComparisons(t *testing.T) {
	binary := os.Getenv("INFRACOST_TEST_BINARY")
	if binary == "" {
		binary = infracostBinary
	}
	require.FileExists(t, binary)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/graphql", r.URL.Path)
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		require.NoError(t, err)
		for _, secret := range []string{"PRIVATE_", "planned_values", "prior_state", "resource_changes"} {
			require.NotContains(t, string(body), secret)
		}
		var queries []json.RawMessage
		require.NoError(t, json.Unmarshal(body, &queries))
		response := make([]json.RawMessage, len(queries))
		for i := range response {
			response[i] = json.RawMessage(`{"data":{"products":[{"prices":[{"priceHash":"fixture","USD":"0.1"}]}]}}`)
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	configuration := CostConfiguration{APIKey: "OWNED_FIXTURE_KEY", Binary: binary, Endpoint: server.URL}
	for _, test := range []struct {
		name          string
		before, after int
		delta         string
		automatic     bool
	}{
		{"increase", 100, 200, "10", false},
		{"flat", 100, 100, "0", true},
		{"decrease", 100, 50, "-5", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := ComparePlanCosts(t.Context(), configuration, costComparisonFixture(t, test.before, test.after))
			require.Equal(t, "complete", result.Status, "%+v", result)
			require.Empty(t, result.Reason)
			require.Equal(t, test.delta, result.DeltaMonthly)
			require.Equal(t, test.automatic, result.NonIncreasing())
			data, err := json.Marshal(result)
			require.NoError(t, err)
			require.NotContains(t, string(data), "PRIVATE_")
			require.NotContains(t, string(data), configuration.APIKey)
		})
	}
	plan := costComparisonFixture(t, 100, 100)
	plan = []byte(strings.Replace(string(plan), `"resource_changes":[]`, `"resource_changes":[{"change":{"after_unknown":{"size":true}}}]`, 1))
	require.False(t, ComparePlanCosts(t.Context(), configuration, plan).NonIncreasing())
	configuration.APIKey = ""
	missing := ComparePlanCosts(t.Context(), configuration, costComparisonFixture(t, 100, 100))
	require.Equal(t, "api_key_missing", missing.Reason)
	require.False(t, missing.NonIncreasing())
}

func costComparisonFixture(t *testing.T, before, after int) []byte {
	t.Helper()
	var document map[string]any
	require.NoError(t, json.Unmarshal([]byte(infracostPlanFixture), &document))
	root := document["planned_values"].(map[string]any)["root_module"].(map[string]any)
	resource := root["resources"].([]any)[0].(map[string]any)
	resource["values"].(map[string]any)["size"] = after
	root["resources"] = []any{resource}
	priorBytes, err := json.Marshal(document["planned_values"])
	require.NoError(t, err)
	var prior map[string]any
	require.NoError(t, json.Unmarshal(priorBytes, &prior))
	prior["root_module"].(map[string]any)["resources"].([]any)[0].(map[string]any)["values"].(map[string]any)["size"] = before
	document["prior_state"] = map[string]any{"format_version": "1.0", "values": prior}
	document["resource_changes"] = []any{}
	data, err := json.Marshal(document)
	require.NoError(t, err)
	return data
}
