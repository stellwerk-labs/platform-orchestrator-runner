package rolloutexec

import (
	"context"
	"errors"
	"os"

	api "github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
)

const ObservationCapability = "environment-observation/v1"

// Observe has no task hooks, retained plan, outputs, or infrastructure effects.
func (e *Executor) Observe(ctx context.Context, request Request) (api.DeploymentResultsUpdateBody, error) {
	failure := errors.New("observation unavailable")
	if request.Mode != "observation" || request.EncryptedOutputsRecipient != "" || request.MetadataKey != "" || e.Validate(request, nil) != nil {
		return api.DeploymentResultsUpdateBody{}, failure
	}
	dir, err := e.workDirectory()
	if err != nil {
		return api.DeploymentResultsUpdateBody{}, failure
	}
	defer func() { _ = os.RemoveAll(dir) }()
	rn, err := e.prepareRunner(dir, request)
	if err != nil {
		return api.DeploymentResultsUpdateBody{}, failure
	}
	observer, ok := rn.(runner.ObservationRunner)
	if !ok {
		return api.DeploymentResultsUpdateBody{}, failure
	}
	result, err := observer.Observe(ctx)
	if err != nil || result == nil || !bool(result.Complete) || ctx.Err() != nil {
		return api.DeploymentResultsUpdateBody{}, failure
	}
	return api.DeploymentResultsUpdateBody{Status: api.Success, ObservationResult: result}, nil
}
