package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
)

type rolloutCatalogueExport struct {
	SupportedProtocols []string                 `json:"supported_protocols"`
	Capabilities       []string                 `json:"capabilities"`
	NativeExecution    rolloutCatalogueManifest `json:"native_execution"`
}

type rolloutCatalogueManifest struct {
	ContractVersion string                   `json:"contract_version"`
	RuntimeVersion  string                   `json:"runtime_version"`
	ToolsDigest     string                   `json:"tools_digest"`
	Tools           []rolloutexec.PublicTool `json:"tools"`
}

func exportRolloutCatalogue() error {
	file, err := os.Open(os.Getenv("ROLLOUT_CONFIG_FILE"))
	if err != nil {
		return errors.New("open local inventory configuration")
	}
	defer func() { _ = file.Close() }()
	var cfg rolloutConfig
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return errors.New("invalid local inventory configuration")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cfg) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid local inventory configuration")
	}
	runtimeVersion := rollout.RuntimeVersion
	protocols := []string{rollout.PipelineVersion, "stellwerk.pipeline.plan-apply/v1", "environment-observation/v1"}
	execution := &rolloutexec.Executor{Tools: cfg.Tools, Capabilities: cfg.Capabilities, KubernetesBackendConfigPath: cfg.KubernetesBackendConfigPath}
	if cfg.Postgres != nil {
		runtimeVersion = rollout.NativeRuntimeVersion
		protocols = []string{rollout.NativePipelineVersion, "stellwerk.pipeline.plan-apply/v2", "environment-observation/v1"}
		identity, err := rollout.LoadStorageIdentity(cfg.Postgres.IdentityFile)
		if err != nil {
			return err
		}
		if err := execution.PrepareNativeCatalogue(identity); err != nil {
			return err
		}
	}
	cost, err := loadRolloutCost(cfg.Cost)
	if err != nil {
		return err
	}
	if cost != nil {
		pricing := rolloutexec.NetworkPricingCapability
		if cost.PricingAccess == "local" {
			pricing = rolloutexec.LocalPricingCapability
		}
		cfg.Capabilities = append(cfg.Capabilities, rolloutexec.CostCapability, pricing)
	}
	execution.Cost, execution.Capabilities = cost, cfg.Capabilities
	if cfg.Postgres != nil {
		if err := execution.PrepareNativeBackend(); err != nil {
			return err
		}
	}
	inventory, err := execution.PublicInventory()
	if err != nil {
		return err
	}
	manifest := rolloutCatalogueManifest{ContractVersion: "stellwerk.runner.catalogue/v1", RuntimeVersion: runtimeVersion, ToolsDigest: inventory.ToolsDigest, Tools: inventory.Tools}
	encoded, err := json.Marshal(manifest)
	if err != nil || len(encoded) > 64<<10 {
		return errors.New("public execution catalogue exceeds its size limit")
	}
	return json.NewEncoder(os.Stdout).Encode(rolloutCatalogueExport{
		SupportedProtocols: protocols,
		Capabilities:       inventory.Capabilities, NativeExecution: manifest,
	})
}
