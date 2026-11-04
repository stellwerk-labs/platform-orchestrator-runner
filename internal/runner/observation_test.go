package runner

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stretchr/testify/require"
)

func TestObservationPlanSanitizesOnlySafeSummary(t *testing.T) {
	input := `{"format_version":"1.2","terraform_version":"1.17.1","complete":true,"errored":false,"configuration":{},"planned_values":{},"resource_drift":[{"address":"local_file.secret[\"SECRET_CANARY\"]","type":"local_file","change":{"actions":["delete"],"before":{"content":"SECRET_CANARY"}}}],"resource_changes":[{"address":"SECRET_CANARY","type":"local_file","change":{"actions":["create"],"after":{"content":"SECRET_CANARY"}}},{"type":"terraform_data","change":{"actions":["no-op"]}}],"output_changes":{"SECRET_CANARY":{"actions":["update"],"after":"SECRET_CANARY"}}}`
	result, err := parseObservationPlan([]byte(input), "terraform")
	require.NoError(t, err)
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "SECRET_CANARY")
	require.NotContains(t, string(encoded), "address")
	require.Len(t, result.ObservedChanges, 1)
	require.Len(t, result.ProposedChanges, 1)
	require.Equal(t, 1, result.OutputChangeCount)
}

func TestObservationPlanRejectsIncompleteAndUnsupported(t *testing.T) {
	valid := `{"format_version":"1.2","terraform_version":"1.17.1","complete":true,"errored":false,"configuration":{},"planned_values":{}}`
	for name, input := range map[string]string{
		"empty": "", "state": "{}", "truncated": valid[:len(valid)-1],
		"trailing": valid + "{}", "prefix": "diagnostic\n" + valid,
		"unknown-format":   strings.Replace(valid, "1.2", "2.0", 1),
		"partial":          strings.Replace(valid, `"complete":true`, `"complete":false`, 1),
		"missing-complete": strings.Replace(valid, `"complete":true,`, "", 1),
		"errored":          strings.Replace(valid, `"errored":false`, `"errored":true`, 1),
		"missing-error":    strings.Replace(valid, `"errored":false,`, "", 1),
		"duplicate":        strings.Replace(valid, `"complete":true`, `"complete":false,"complete":true`, 1),
		"failed-check":     strings.Replace(valid, `"configuration":{}`, `"checks":[{"status":"fail"}],"configuration":{}`, 1),
		"unknown-check":    strings.Replace(valid, `"configuration":{}`, `"checks":[{"status":"unknown"}],"configuration":{}`, 1),
		"deferred":         strings.Replace(valid, `"configuration":{}`, `"deferred_changes":[{}],"configuration":{}`, 1),
		"unknown-action":   strings.Replace(valid, `"configuration":{}`, `"resource_changes":[{"type":"local_file","change":{"actions":["invoke"]}}],"configuration":{}`, 1),
		"bad-type":         strings.Replace(valid, `"configuration":{}`, `"resource_changes":[{"type":"secret/key","change":{"actions":["create"]}}],"configuration":{}`, 1),
		"import":           strings.Replace(valid, `"configuration":{}`, `"resource_changes":[{"type":"local_file","change":{"actions":["no-op"],"importing":{"id":"SECRET_CANARY"}}}],"configuration":{}`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			result, err := parseObservationPlan([]byte(input), "terraform")
			require.Error(t, err)
			require.Nil(t, result)
			require.NotContains(t, err.Error(), "SECRET_CANARY")
		})
	}
	_, err := parseObservationPlan([]byte(valid), "unsupported")
	require.Error(t, err)
	_, err = parseObservationPlan(make([]byte, maxObservationPlanBytes+1), "terraform")
	require.Error(t, err)
}

func TestObservationActionsFailClosed(t *testing.T) {
	for _, actions := range [][]string{nil, {}, {"create", "update"}, {"no-op", "delete"}, {"invoke"}, {"create", "delete", "read"}} {
		require.False(t, validObservationActions(actions))
	}
	for _, actions := range [][]string{{"create"}, {"read"}, {"update"}, {"delete"}, {"no-op"}, {"forget"}, {"create", "delete"}, {"delete", "create"}} {
		require.True(t, validObservationActions(actions))
	}
}

func TestObservationConfiguredRunnerRefusesUnsafeEntryPoints(t *testing.T) {
	r := &baseRunner{observationOnly: true, binaryPath: "must-never-run"}
	_, err := r.init(t.Context())
	require.Error(t, err)
	_, _, _, err = r.plan(t.Context())
	require.Error(t, err)
	_, _, err = r.apply(t.Context())
	require.Error(t, err)
	_, _, err = r.destroy(t.Context())
	require.Error(t, err)
	_, err = r.Version(t.Context())
	require.Error(t, err)
	_, err = r.output(t.Context())
	require.Error(t, err)
	_, err = r.OutputMetadata(t.Context(), "secret")
	require.Error(t, err)
}

func TestRunnerFactoryRefusesUnsupportedModesAndBackends(t *testing.T) {
	for _, cfg := range []*config.StandardModeConfiguration{
		{Mode: "future-observation", IaCBackend: config.BackendOpenTofu},
		{Mode: config.PlanOnly, ObservationProtocol: "SECRET_UNKNOWN_PROTOCOL"},
		{Mode: config.Deploy, ObservationProtocol: config.ObservationProtocolV1},
		{Mode: config.Destroy, ObservationProtocol: config.ObservationProtocolV1},
		{Mode: config.PlanOnly, ObservationProtocol: config.ObservationProtocolV1, IaCBackend: "future-tool", Gateway: config.GatewayClientConfiguration{URL: "http://127.0.0.1"}},
	} {
		rn, err := CreateRunner(t.Context(), cfg)
		require.Error(t, err)
		require.Nil(t, rn)
	}
}

func TestObservationBufferLimitCannotBeBypassedByIOCopy(t *testing.T) {
	buffer := &boundedObservationBuffer{remaining: 8}
	source := struct{ io.Reader }{strings.NewReader(strings.Repeat("x", 16))}
	_, err := io.Copy(buffer, source)
	require.Error(t, err)
	require.True(t, buffer.exceeded)
	require.LessOrEqual(t, buffer.buffer.Len(), 8)
}
