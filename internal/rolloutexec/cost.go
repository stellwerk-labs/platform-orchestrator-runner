package rolloutexec

import (
	"context"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
)

const CostCapability = "cost.infracost.v1"
const NetworkPricingCapability = "network.pricing"
const LocalPricingCapability = "pricing.local"

// EstimateCosts uses operator-local configuration. Its absence is uncertainty,
// never a free estimate. Commands carry only the explicit opt-in, not credentials.
func (e *Executor) EstimateCosts(ctx context.Context, review []byte) runner.CostComparison {
	var configuration runner.CostConfiguration
	if e.Cost != nil {
		configuration = *e.Cost
	}
	return runner.ComparePlanCosts(ctx, configuration, review)
}

func (e *Executor) costIdentity() *costIdentity {
	if e.Cost == nil {
		return nil
	}
	binary, endpoint := e.Cost.Binary, e.Cost.Endpoint
	if binary == "" {
		binary = "/opt/runner/infracost"
	}
	if endpoint == "" {
		endpoint = "https://pricing.api.infracost.io"
	}
	return &costIdentity{PricingAccess: e.PricingCapability(), Version: "v0.10.49+stellwerk.1", BinaryDigest: executableDigest(binary), Endpoint: endpoint}
}

func (e *Executor) PricingCapability() string {
	if e.Cost != nil && e.Cost.PricingAccess == "local" {
		return LocalPricingCapability
	}
	return NetworkPricingCapability
}

type costIdentity struct {
	PricingAccess string `json:"pricing_access"`
	Version       string `json:"version"`
	BinaryDigest  string `json:"binary_digest"`
	Endpoint      string `json:"endpoint"`
}
