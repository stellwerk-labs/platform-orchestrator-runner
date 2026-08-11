package runner

import (
	"context"

	"filippo.io/age"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gatewayapi"
)

// RunnerFactory is a function that constructs a RunnerInterface from configuration.
type RunnerFactory func(ctx context.Context, cfg *config.StandardModeConfiguration) (RunnerInterface, error)

// CreateRunner builds the appropriate RunnerInterface based on cfg.TFBackend.
func CreateRunner(ctx context.Context, cfg *config.StandardModeConfiguration) (RunnerInterface, error) {
	var recipient age.Recipient
	if cfg.EncryptingKey != "" {
		recipient, _ = age.ParseX25519Recipient(cfg.EncryptingKey)
	}

	base := &baseRunner{
		orgID:        cfg.OrgID,
		deploymentID: cfg.DeploymentID,
		folder:       cfg.IaCCodeDir,
		recipient:    recipient,
	}
	client, err := gatewayapi.NewClient(gatewayapi.ClientConfig{
		BaseURL: cfg.Gateway.URL, OrganizationID: cfg.OrgID, RunnerID: cfg.RunnerID,
		CAFile: cfg.Gateway.CAFile, ClientCertFile: cfg.Gateway.ClientCertFile,
		ClientKeyFile: cfg.Gateway.ClientKeyFile,
	})
	if err != nil {
		return nil, err
	}
	base.bundleLoader = func(ctx context.Context) ([]byte, error) {
		return client.GetBundle(ctx, cfg.DeploymentID, cfg.DeploymentToken)
	}

	switch cfg.IaCBackend {
	case config.BackendTerraform:
		return newTerraformRunner(base), nil
	default:
		return newOpenTofuRunner(base), nil
	}
}
