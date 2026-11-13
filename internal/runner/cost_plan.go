package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const maxCostPlanBytes = 8 << 20
const maxCostResources = 2000
const infracostVersion = "v0.10.49"
const infracostBuild = "v0.10.49+stellwerk.1"
const infracostBinary = "/opt/runner/infracost"
const infracostPricingEndpoint = "https://pricing.api.infracost.io"

type costEstimate struct {
	Monthly                                                  string
	Resources, Priced, Free, Unpriced, Unknown, MissingUsage int
}

type costOutputResource struct {
	CostComponents []struct {
		MonthlyCost     *string `json:"monthlyCost"`
		MonthlyQuantity *string `json:"monthlyQuantity"`
		PriceNotFound   bool    `json:"priceNotFound"`
		UsageBased      bool    `json:"usageBased"`
	} `json:"costComponents"`
	Subresources []costOutputResource `json:"subresources"`
}

type costOutput struct {
	Version          string  `json:"version"`
	Currency         string  `json:"currency"`
	TotalMonthlyCost *string `json:"totalMonthlyCost"`
	Projects         []struct {
		Breakdown *struct {
			Resources []costOutputResource `json:"resources"`
		} `json:"breakdown"`
	} `json:"projects"`
	Summary struct {
		Supported   int `json:"totalSupportedResources"`
		Unsupported int `json:"totalUnsupportedResources"`
		Free        int `json:"totalNoPriceResources"`
	} `json:"summary"`
}

func (r *baseRunner) estimateCosts(ctx context.Context, data []byte) (costEstimate, string) {
	if r.infracostAPIKey == "" {
		return costEstimate{}, "api_key_missing"
	}
	data, result, reason := prepareCostPlan(data)
	if reason != "" {
		return result, reason
	}
	output, reason := r.runInfracost(ctx, data)
	if reason != "" {
		return result, reason
	}
	return parseCostOutput(output, result)
}

func prepareCostPlan(data []byte) ([]byte, costEstimate, string) {
	var result costEstimate
	if len(data) == 0 || len(data) > maxCostPlanBytes {
		return nil, result, "plan_limit"
	}
	var plan map[string]any
	if decodeCostJSON(data, &plan) != nil {
		return nil, result, "invalid_plan"
	}
	if sensitiveCostRegion(plan) {
		return nil, result, "sensitive_pricing_configuration"
	}
	planned, ok := plan["planned_values"].(map[string]any)
	if !ok {
		return nil, result, "invalid_plan"
	}
	root, ok := planned["root_module"].(map[string]any)
	if !ok || !sanitizeCostModule(root, &result, 0) {
		return nil, result, "invalid_plan"
	}
	// State and changes are unnecessary for a planned-resource breakdown.
	delete(plan, "prior_state")
	delete(plan, "resource_changes")
	delete(plan, "output_changes")
	delete(planned, "outputs")
	data, err := json.Marshal(plan)
	if err != nil {
		return nil, result, "invalid_plan"
	}
	return data, result, ""
}

func (r *baseRunner) runInfracost(ctx context.Context, data []byte) ([]byte, string) {
	dir, err := os.MkdirTemp("", "runner-cost-")
	if err != nil {
		return nil, "temporary_storage"
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "plan.json")
	if os.WriteFile(path, data, 0600) != nil {
		return nil, "temporary_storage"
	}
	binary := r.costBinary
	if binary == "" {
		binary = infracostBinary
	}
	endpoint := r.costEndpoint
	if endpoint == "" {
		endpoint = infracostPricingEndpoint
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "breakdown", "--path", path, "--format", "json", "--no-color")
	cmd.Dir = dir
	cmd.WaitDelay = time.Second
	cmd.Env = []string{
		"HOME=" + dir, "XDG_CONFIG_HOME=" + dir, "XDG_CACHE_HOME=" + dir,
		"INFRACOST_API_KEY=" + r.infracostAPIKey, "INFRACOST_PRICING_API_ENDPOINT=" + endpoint,
		"INFRACOST_EVENTSDISABLED=true", "INFRACOST_ENABLE_CLOUD=false",
		"INFRACOST_ENABLE_CLOUD_UPLOAD=false", "INFRACOST_ENABLE_DASHBOARD=false",
		"INFRACOST_SKIP_UPDATE_CHECK=true", "INFRACOST_DISABLE_ENVFILE=true",
		"INFRACOST_PRICING_CACHE_DISABLED=true", "INFRACOST_NO_COLOR=true", "INFRACOST_CURRENCY=USD",
	}
	out := boundedObservationBuffer{remaining: maxCostPlanBytes}
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil || ctx.Err() != nil || out.exceeded {
		return nil, "estimator_unavailable"
	}
	return out.buffer.Bytes(), ""
}

func decodeCostJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := validateObservationJSON(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing cost JSON")
	}
	return json.Unmarshal(data, value)
}

func sanitizeCostModule(module map[string]any, result *costEstimate, depth int) bool {
	if depth > 64 {
		return false
	}
	resources, _ := module["resources"].([]any)
	kept := []any{}
	for _, item := range resources {
		resource, ok := item.(map[string]any)
		if !ok {
			return false
		}
		if resource["mode"] != "managed" {
			continue
		}
		result.Resources++
		if result.Resources > maxCostResources {
			return false
		}
		// Broad calculator coverage cannot justify disclosing sensitive pricing inputs.
		if costMaskPresent(resource["sensitive_values"]) {
			result.Unknown++
			continue
		}
		delete(resource, "sensitive_values")
		kept = append(kept, resource)
	}
	module["resources"] = kept
	children, _ := module["child_modules"].([]any)
	for _, child := range children {
		next, ok := child.(map[string]any)
		if !ok || !sanitizeCostModule(next, result, depth+1) {
			return false
		}
	}
	return true
}

func costMaskPresent(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case map[string]any:
		for _, item := range v {
			if costMaskPresent(item) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if costMaskPresent(item) {
				return true
			}
		}
	default:
		return value != nil
	}
	return false
}

func parseCostOutput(data []byte, result costEstimate) (costEstimate, string) {
	var output costOutput
	if decodeCostJSON(data, &output) != nil || output.Version != "0.2" || output.Currency != "USD" {
		return result, "invalid_estimator_output"
	}
	if !foldCostCoverage(output, &result) {
		return result, "invalid_estimator_output"
	}
	monthly, valid := costMonthlyAmount(output.TotalMonthlyCost)
	if !valid {
		return result, "invalid_estimator_output"
	}
	if result.Priced+result.Free > 0 {
		result.Monthly = monthly
	}
	if output.TotalMonthlyCost == nil && result.Resources > 0 && result.Free == result.Resources {
		result.Monthly = "0"
	}
	return result, ""
}

func foldCostCoverage(output costOutput, result *costEstimate) bool {
	result.Priced, result.Free = output.Summary.Supported, output.Summary.Free
	result.Unpriced = result.Resources - result.Unknown - result.Priced - result.Free
	if result.Priced < 0 || result.Free < 0 || result.Unpriced < 0 {
		return false
	}
	for _, project := range output.Projects {
		if project.Breakdown == nil {
			continue
		}
		for _, resource := range project.Breakdown.Resources {
			price, usage := costComponentGaps(resource)
			if price {
				result.Unknown++
				result.Priced--
			}
			if usage {
				result.MissingUsage++
			}
		}
	}
	return result.Priced >= 0
}

func costMonthlyAmount(value *string) (string, bool) {
	if value == nil {
		return "", true
	}
	if len(*value) > 64 || strings.ContainsAny(*value, "/eE") {
		return "", false
	}
	amount, ok := new(big.Rat).SetString(*value)
	if !ok || amount.Sign() < 0 {
		return "", false
	}
	return *value, true
}

func costComponentGaps(resource costOutputResource) (bool, bool) {
	missingPrice, missingUsage := false, false
	for _, component := range resource.CostComponents {
		missingPrice = missingPrice || component.PriceNotFound || component.MonthlyCost == nil && !component.UsageBased
		missingUsage = missingUsage || component.UsageBased && component.MonthlyQuantity == nil
	}
	for _, child := range resource.Subresources {
		price, usage := costComponentGaps(child)
		missingPrice, missingUsage = missingPrice || price, missingUsage || usage
	}
	return missingPrice, missingUsage
}

// Provider regions can refer to sensitive inputs outside planned resource masks.
func sensitiveCostRegion(plan map[string]any) bool {
	configuration, _ := plan["configuration"].(map[string]any)
	root, _ := configuration["root_module"].(map[string]any)
	sensitive := map[string]bool{}
	collectSensitiveCostVariables(root, sensitive)
	providers, _ := configuration["provider_config"].(map[string]any)
	for _, provider := range providers {
		p, _ := provider.(map[string]any)
		expressions, _ := p["expressions"].(map[string]any)
		for _, field := range []string{"region", "zone", "location"} {
			expression, _ := expressions[field].(map[string]any)
			references, _ := expression["references"].([]any)
			for _, reference := range references {
				ref, _ := reference.(string)
				parts := strings.Split(ref, ".")
				if len(parts) > 1 && parts[0] == "var" && sensitive[parts[1]] {
					return true
				}
			}
		}
	}
	return false
}
func collectSensitiveCostVariables(module map[string]any, sensitive map[string]bool) {
	variables, _ := module["variables"].(map[string]any)
	for name, value := range variables {
		declaration, _ := value.(map[string]any)
		if declaration["sensitive"] == true {
			sensitive[name] = true
		}
	}
	calls, _ := module["module_calls"].(map[string]any)
	for _, value := range calls {
		call, _ := value.(map[string]any)
		child, _ := call["module"].(map[string]any)
		collectSensitiveCostVariables(child, sensitive)
	}
}
