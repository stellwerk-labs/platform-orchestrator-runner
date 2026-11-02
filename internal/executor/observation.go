package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	api "github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/sshagent"
)

func executeObservation(ctx context.Context, cfg *config.StandardModeConfiguration, factory runner.RunnerFactory) (api.DeploymentResultsUpdateBody, error) {
	fail := func(stage string) (api.DeploymentResultsUpdateBody, error) {
		err := errors.New("observation " + stage + " failed")
		return api.DeploymentResultsUpdateBody{Status: api.Failure, Error: &api.Error{Error: "OBSERVATION_UNAVAILABLE", Message: err.Error()}}, err
	}
	if !cfg.IsObservation() {
		return fail("protocol validation")
	}
	rn, err := factory(ctx, cfg)
	if err != nil {
		return fail("runner creation")
	}
	observer, ok := rn.(runner.ObservationRunner)
	if !ok {
		return fail("unsupported runner")
	}
	if err := rn.CreateIaCFolder(ctx); err != nil {
		return fail("bundle preparation")
	}
	if _, err := os.Stat(filepath.Join(cfg.IaCCodeDir, "main.tf")); err != nil {
		return fail("configuration validation")
	}
	closeAgent, err := sshagent.Launch(filepath.Join(cfg.IaCCodeDir, ".ssh-agent"))
	if err != nil {
		return fail("credential preparation")
	}
	defer closeAgent()
	result, err := observer.Observe(ctx)
	if err != nil || result == nil || !bool(result.Complete) || ctx.Err() != nil {
		return fail("execution")
	}
	return api.DeploymentResultsUpdateBody{Status: api.Success, ObservationResult: result}, nil
}
