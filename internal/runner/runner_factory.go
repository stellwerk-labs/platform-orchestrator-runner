package runner

import (
	"context"
	"fmt"

	"filippo.io/age"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/natstransport"
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
	bucket := cfg.NATS.BundleBucket
	key := cfg.NATS.BundleKey
	if key == "" {
		key = cfg.OrgID + "/" + cfg.DeploymentID
	}
	base.bundleLoader = func(ctx context.Context) ([]byte, error) {
		connection, err := natstransport.Connect(natstransport.Config{
			URL: cfg.NATS.URL, Token: cfg.NATS.Token, CredentialsFile: cfg.NATS.CredentialsFile,
			CAFile: cfg.NATS.CAFile, ClientCertFile: cfg.NATS.ClientCertFile,
			ClientKeyFile: cfg.NATS.ClientKeyFile, Name: "platform-orchestrator-bundle/" + cfg.DeploymentID,
		})
		if err != nil {
			return nil, err
		}
		defer connection.Close()
		js, err := connection.JetStream()
		if err != nil {
			return nil, err
		}
		store, err := js.ObjectStore(bucket)
		if err != nil {
			return nil, fmt.Errorf("open pre-provisioned bundle Object Store %s: %w", bucket, err)
		}
		return store.GetBytes(key)
	}

	switch cfg.IaCBackend {
	case config.BackendTerraform:
		return newTerraformRunner(base), nil
	default:
		return newOpenTofuRunner(base), nil
	}
}
