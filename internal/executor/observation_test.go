package executor

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/limitedlogsbuffer"
	api "github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
	"github.com/stretchr/testify/require"
)

func TestObservationProtocolRefusesInvalidBeforeFactory(t *testing.T) {
	for _, cfg := range []*config.StandardModeConfiguration{
		{Mode: config.PlanOnly, ObservationProtocol: "SECRET_UNSUPPORTED_PROTOCOL"},
		{Mode: config.Deploy, ObservationProtocol: config.ObservationProtocolV1},
		{Mode: config.Destroy, ObservationProtocol: config.ObservationProtocolV1},
		{Mode: "observation", ObservationProtocol: config.ObservationProtocolV1},
	} {
		original := slog.Default()
		result, err := ExecuteStandardMode(t.Context(), cfg, func(context.Context, *config.StandardModeConfiguration) (runner.RunnerInterface, error) {
			t.Fatal("invalid observation protocol reached the runner factory")
			return nil, nil
		}, &limitedlogsbuffer.LimitedLogsBuffer{}, new(slog.LevelVar))
		slog.SetDefault(original)
		require.Error(t, err)
		require.Equal(t, api.Failure, result.Status)
		require.Nil(t, result.ObservationResult)
		require.NotContains(t, err.Error(), "SECRET_UNSUPPORTED_PROTOCOL")
	}
}
