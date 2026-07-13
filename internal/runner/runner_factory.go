package runner

import (
	"context"
	"net/http"

	"filippo.io/age"
	"github.com/pkg/errors"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/utils"
)

// RunnerFactory is a function that constructs a RunnerInterface from configuration.
type RunnerFactory func(ctx context.Context, cfg *config.StandardModeConfiguration) (RunnerInterface, error)

// CreateRunner builds the appropriate RunnerInterface based on cfg.TFBackend.
func CreateRunner(ctx context.Context, cfg *config.StandardModeConfiguration) (RunnerInterface, error) {
	apiClient, err := platformorchestratorapi.NewClientWithResponses(
		cfg.PlatformOrchestratorApiPrefix,
		platformorchestratorapi.WithHTTPClient(utils.WrapHttpClientWithRetries(http.DefaultClient)),
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to initialize api client")
	}

	var recipient age.Recipient
	if cfg.EncryptingKey != "" {
		recipient, _ = age.ParseX25519Recipient(cfg.EncryptingKey)
	}

	base := &baseRunner{
		apiClient:    apiClient,
		orgID:        cfg.OrgID,
		deploymentID: cfg.DeploymentID,
		token:        cfg.Token,
		folder:       cfg.IaCCodeDir,
		recipient:    recipient,
	}

	switch cfg.IaCBackend {
	case config.BackendTerraform:
		return newTerraformRunner(base), nil
	default:
		return newOpenTofuRunner(base), nil
	}
}
