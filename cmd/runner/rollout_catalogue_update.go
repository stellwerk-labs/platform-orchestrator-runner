package main

import (
	"context"
	"encoding/json"
	"os"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
)

var rolloutCatalogueExpected, rolloutCatalogueAdminDSN string

func updateRolloutCatalogue(ctx context.Context) error {
	cfg, public, err := loadRolloutConfig()
	if err != nil {
		return &rollout.Error{Code: "catalogue_update_configuration_refused"}
	}
	cost, err := loadRolloutCost(cfg.Cost)
	if err != nil {
		return &rollout.Error{Code: "catalogue_update_configuration_refused"}
	}
	if cost != nil {
		pricing := rolloutexec.NetworkPricingCapability
		if cost.PricingAccess == "local" {
			pricing = rolloutexec.LocalPricingCapability
		}
		cfg.Capabilities = append(cfg.Capabilities, rolloutexec.CostCapability, pricing)
	}
	execution := &rolloutexec.Executor{Cost: cost, KubernetesBackendConfigPath: cfg.KubernetesBackendConfigPath, Tools: cfg.Tools, Capabilities: cfg.Capabilities}
	config := rollout.Config{Postgres: cfg.Postgres, Directory: cfg.Directory, OrgID: cfg.OrgID, RunnerID: cfg.RunnerID, RunnerUUID: cfg.RunnerUUID, PublicKey: public, Executor: execution}
	if err = rollout.UpdatePostgresCatalogue(ctx, config, rolloutCatalogueExpected, rolloutCatalogueAdminDSN); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Status string `json:"status"`
	}{"catalogue_ready"})
}
