package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"strings"

	pogicost "github.com/stellwerk-labs/platform-orchestrator-runner/internal/thirdparty/pogi/cost"
	pogitf "github.com/stellwerk-labs/platform-orchestrator-runner/internal/thirdparty/pogi/terraform"
)

const maxCostPlanBytes = 8 << 20
const maxCostResources = 2000
const costCatalogueDate = "2026-10-20"

// Only aggregate counts cross the log boundary. Resource identities and values stay private.
type diskEstimate struct {
	Monthly                                    float64
	Resources, Priced, Free, Unpriced, Unknown int
}
type costResource struct {
	Address   string         `json:"address"`
	Mode      string         `json:"mode"`
	Type      string         `json:"type"`
	Name      string         `json:"name"`
	Provider  string         `json:"provider_name"`
	Values    map[string]any `json:"values"`
	Sensitive any            `json:"sensitive_values"`
}
type costModule struct {
	Resources []costResource `json:"resources"`
	Children  []costModule   `json:"child_modules"`
}
type costChange struct {
	costResource
	Change struct {
		Actions   []string `json:"actions"`
		Unknown   any      `json:"after_unknown"`
		Sensitive any      `json:"after_sensitive"`
	} `json:"change"`
}
type costExpression struct {
	Constant   any      `json:"constant_value"`
	References []string `json:"references"`
}
type costConfigModule struct {
	Resources []struct {
		Address     string `json:"address"`
		ProviderKey string `json:"provider_config_key"`
	} `json:"resources"`
	Modules map[string]struct {
		Module *costConfigModule `json:"module"`
	} `json:"module_calls"`
	Variables map[string]struct {
		Sensitive bool `json:"sensitive"`
	} `json:"variables"`
}
type costPlan struct {
	Format  string `json:"format_version"`
	Planned *struct {
		Root *costModule `json:"root_module"`
	} `json:"planned_values"`
	Changes   []costChange `json:"resource_changes"`
	Variables map[string]struct {
		Value any `json:"value"`
	} `json:"variables"`
	Configuration struct {
		Providers map[string]struct {
			Name          string                    `json:"name"`
			ModuleAddress string                    `json:"module_address"`
			Expressions   map[string]costExpression `json:"expressions"`
		} `json:"provider_config"`
		Root costConfigModule `json:"root_module"`
	} `json:"configuration"`
}

var costAddress = regexp.MustCompile(`^(?:module\.[A-Za-z_][A-Za-z0-9_-]*(?:\[(?:[0-9]+|"(?:[^"\\]|\\.)*")\])?\.)*(?:data\.)?[A-Za-z_][A-Za-z0-9_]*\.[A-Za-z_][A-Za-z0-9_-]*(?:\[(?:[0-9]+|"(?:[^"\\]|\\.)*")\])?$`)
var costInstance = regexp.MustCompile(`\[(?:[0-9]+|"(?:[^"\\]|\\.)*")\]`)
var costAWSZone = regexp.MustCompile(`^([a-z]{2}(?:-[a-z]+)+-[0-9]+)[a-z]$`)

func estimateDisks(data []byte) (diskEstimate, error) {
	plan, err := parseCostPlan(data)
	if err != nil {
		return diskEstimate{}, err
	}
	eligible, result, err := projectCostDisks(plan)
	if err != nil || len(eligible) == 0 {
		return result, err
	}
	estimate, err := pogicost.Price(&pogitf.Config{Source: "plan", Resources: eligible}, pogicost.Options{})
	if err != nil {
		return diskEstimate{}, errors.New("offline catalogue unavailable")
	}
	for _, row := range estimate.Rows {
		if knownDiskRow(row) {
			result.Priced++
			result.Monthly += row.Monthly
		} else {
			result.Unpriced++
		}
	}
	if math.IsInf(result.Monthly, 0) || math.IsNaN(result.Monthly) {
		return diskEstimate{}, errors.New("invalid offline total")
	}
	return result, nil
}
func parseCostPlan(data []byte) (costPlan, error) {
	var plan costPlan
	invalid := errors.New("invalid or bounded cost plan")
	if len(data) == 0 || len(data) > maxCostPlanBytes {
		return plan, invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := validateCostJSON(decoder, 0); err != nil {
		return plan, invalid
	}
	if _, err := decoder.Token(); err != io.EOF {
		return plan, invalid
	}
	if json.Unmarshal(data, &plan) != nil || plan.Format != "1.2" || plan.Planned == nil || plan.Planned.Root == nil || len(plan.Changes) > maxCostResources {
		return plan, invalid
	}
	return plan, nil
}
func projectCostDisks(plan costPlan) ([]*pogitf.Resource, diskEstimate, error) {
	var result diskEstimate
	invalid := errors.New("ambiguous cost resource identity")
	changes, err := costChangesByAddress(plan.Changes)
	if err != nil {
		return nil, result, err
	}
	resources := []costResource{}
	providers := map[string]string{}
	if !collectCostResources(*plan.Planned.Root, &resources, 0) || !collectCostProviders(plan.Configuration.Root, "", providers, 0) {
		return nil, result, invalid
	}
	seen := map[string]bool{}
	eligible := []*pogitf.Resource{}
	for _, resource := range resources {
		if !validCostIdentity(resource) || seen[resource.Address] {
			return nil, result, invalid
		}
		seen[resource.Address] = true
		change, changed := changes[resource.Address]
		if changed && !matchingCostChange(resource, change) {
			return nil, result, invalid
		}
		if disk := projectCostDisk(plan, providers, resource, change, &result); disk != nil {
			eligible = append(eligible, disk)
		}
	}
	return eligible, result, nil
}
func costChangesByAddress(all []costChange) (map[string]costChange, error) {
	changes := map[string]costChange{}
	for _, change := range all {
		if !validCostIdentity(change.costResource) || !validObservationActions(change.Change.Actions) {
			return nil, errors.New("invalid cost change")
		}
		if _, exists := changes[change.Address]; exists {
			return nil, errors.New("duplicate cost change")
		}
		changes[change.Address] = change
	}
	return changes, nil
}
func matchingCostChange(resource costResource, change costChange) bool {
	if change.Type != resource.Type || change.Name != resource.Name || change.Mode != resource.Mode || change.Provider != resource.Provider {
		return false
	}
	return len(change.Change.Actions) != 1 || change.Change.Actions[0] != "delete" && change.Change.Actions[0] != "forget"
}
func projectCostDisk(plan costPlan, providers map[string]string, resource costResource, change costChange, result *diskEstimate) *pogitf.Resource {
	if resource.Mode == "data" {
		return nil
	}
	result.Resources++
	// These logical objects have no stand-alone provider charge. Other types need explicit pricing.
	if approvedFree(resource) {
		result.Free++
		return nil
	}
	if !reviewedDisk(resource) {
		result.Unpriced++
		return nil
	}
	region := costRegion(plan, providers, resource, change)
	attrs, known := diskPricingValues(resource, change)
	if region == "" || !known {
		result.Unknown++
		return nil
	}
	cloud := "aws"
	if resource.Type == "azurerm_managed_disk" {
		cloud = "azure"
	}
	return &pogitf.Resource{Address: resource.Address, Block: resource.Address, Type: resource.Type, Name: resource.Name, Cloud: cloud, Region: region, Attrs: attrs}
}
func knownDiskRow(row pogicost.Row) bool {
	if row.Status != pogicost.Known || len(row.Lines) == 0 || len(row.UsageKeys) != 0 || row.Monthly <= 0 || math.IsInf(row.Monthly, 0) || math.IsNaN(row.Monthly) {
		return false
	}
	for _, line := range row.Lines {
		if line.Usage || line.Qty <= 0 || line.UnitPrice <= 0 {
			return false
		}
	}
	return true
}
func validCostIdentity(r costResource) bool {
	if len(r.Address) > 1024 || !costAddress.MatchString(r.Address) || r.Provider == "" || (r.Mode != "managed" && r.Mode != "data") {
		return false
	}
	block := costInstance.ReplaceAllString(r.Address, "")
	suffix := r.Type + "." + r.Name
	if r.Mode == "data" {
		suffix = "data." + suffix
	}
	if r.Mode == "managed" && (strings.HasPrefix(block, "data.") || strings.Contains(block, ".data.")) {
		return false
	}
	return block == suffix || strings.HasSuffix(block, "."+suffix)
}
func collectCostResources(module costModule, resources *[]costResource, depth int) bool {
	if depth > 32 || len(*resources)+len(module.Resources) > maxCostResources {
		return false
	}
	*resources = append(*resources, module.Resources...)
	for _, child := range module.Children {
		if !collectCostResources(child, resources, depth+1) {
			return false
		}
	}
	return true
}
func collectCostProviders(module costConfigModule, prefix string, providers map[string]string, depth int) bool {
	if depth > 32 || len(providers)+len(module.Resources) > maxCostResources {
		return false
	}
	for _, r := range module.Resources {
		key := prefix + r.Address
		if _, exists := providers[key]; exists {
			return false
		}
		providers[key] = r.ProviderKey
	}
	for name, call := range module.Modules {
		if call.Module != nil && !collectCostProviders(*call.Module, prefix+"module."+name+".", providers, depth+1) {
			return false
		}
	}
	return true
}
func reviewedDisk(r costResource) bool {
	return r.Type == "aws_ebs_volume" && officialCostProvider(r.Provider, "aws") || r.Type == "azurerm_managed_disk" && officialCostProvider(r.Provider, "azurerm")
}
func officialCostProvider(provider, name string) bool {
	return provider == "registry.terraform.io/hashicorp/"+name || provider == "registry.opentofu.org/hashicorp/"+name
}
func approvedFree(r costResource) bool {
	return r.Type == "aws_vpc" && officialCostProvider(r.Provider, "aws") || r.Type == "azurerm_resource_group" && officialCostProvider(r.Provider, "azurerm")
}

// A true mask at any parent taints the field. Primitive/array masks cannot be silently treated as empty objects.
func costMasked(mask any, field string) bool {
	switch value := mask.(type) {
	case nil:
		return false
	case bool:
		return value
	case map[string]any:
		return maskContainsTrue(value[field])
	default:
		return true
	}
}
func maskContainsTrue(mask any) bool {
	switch value := mask.(type) {
	case nil:
		return false
	case bool:
		return value
	default:
		return true
	}
}
func pricingValue(r costResource, c costChange, field string) (any, bool) {
	if costMasked(r.Sensitive, field) || costMasked(c.Change.Unknown, field) || costMasked(c.Change.Sensitive, field) {
		return nil, false
	}
	value, ok := r.Values[field]
	return value, ok && value != nil
}
func diskPricingValues(r costResource, c costChange) (map[string]any, bool) {
	attrs := map[string]any{}
	fields := []string{"type", "size", "iops", "throughput"}
	if r.Type == "azurerm_managed_disk" {
		fields = []string{"storage_account_type", "disk_size_gb", "location"}
	}
	for _, field := range fields {
		value, ok := pricingValue(r, c, field)
		if !ok {
			return nil, false
		}
		attrs[field] = value
	}
	if r.Type == "aws_ebs_volume" {
		return attrs, attrs["type"] == "gp3" && costNumber(attrs["size"], 1, 65536) && costNumber(attrs["iops"], 3000, 80000) && costNumber(attrs["throughput"], 125, 2000)
	}
	if attrs["storage_account_type"] != "Premium_LRS" && attrs["storage_account_type"] != "Premium_ZRS" || !costNumber(attrs["disk_size_gb"], 1, 32767) {
		return nil, false
	}
	return attrs, defaultAzureDiskCharges(r, c)
}
func defaultAzureDiskCharges(r costResource, c costChange) bool {
	// Pogi does not price custom performance tiers, on-demand bursting, or shared-disk charges.
	for _, field := range []string{"tier", "on_demand_bursting_enabled", "max_shares"} {
		if costMasked(r.Sensitive, field) || costMasked(c.Change.Unknown, field) || costMasked(c.Change.Sensitive, field) {
			return false
		}
		if !defaultAzureDiskCharge(field, r.Values[field]) {
			return false
		}
	}
	return true
}
func defaultAzureDiskCharge(field string, value any) bool {
	if value == nil {
		return true
	}
	switch field {
	case "tier":
		return value == ""
	case "on_demand_bursting_enabled":
		return value == false
	case "max_shares":
		return value == float64(1) || value == float64(0)
	}
	return false
}
func costNumber(value any, min, max float64) bool {
	number, ok := value.(float64)
	return ok && number >= min && number <= max && math.Trunc(number) == number
}
func costRegion(plan costPlan, providers map[string]string, r costResource, c costChange) string {
	if r.Type == "azurerm_managed_disk" {
		location, _ := pricingValue(r, c, "location")
		value, _ := location.(string)
		return strings.ToLower(strings.ReplaceAll(value, " ", ""))
	}
	region, valid := providerCostRegion(plan, providers[costInstance.ReplaceAllString(r.Address, "")])
	if !valid || costMasked(r.Sensitive, "region") || costMasked(c.Change.Unknown, "region") || costMasked(c.Change.Sensitive, "region") {
		return ""
	}
	// AWS resource-level regions override the provider default.
	if value := r.Values["region"]; value != nil {
		var ok bool
		region, ok = value.(string)
		if !ok || region == "" {
			return ""
		}
	}
	value, _ := pricingValue(r, c, "availability_zone")
	zone, _ := value.(string)
	if zoneRegion := costZoneRegion(zone); zoneRegion != "" {
		if region != "" && region != zoneRegion {
			return ""
		}
		region = zoneRegion
	}
	return region
}
func costZoneRegion(zone string) string {
	match := costAWSZone.FindStringSubmatch(zone)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}
func providerCostRegion(plan costPlan, key string) (string, bool) {
	if key == "" {
		return "", true
	}
	provider, ok := plan.Configuration.Providers[key]
	if !ok || provider.Name != "aws" && !officialCostProvider(provider.Name, "aws") {
		return "", false
	}
	expression := provider.Expressions["region"]
	region, _ := expression.Constant.(string)
	if region != "" {
		return region, true
	}
	if provider.ModuleAddress != "" || len(expression.References) != 1 || !strings.HasPrefix(expression.References[0], "var.") {
		return "", true
	}
	name := strings.TrimPrefix(expression.References[0], "var.")
	if plan.Configuration.Root.Variables[name].Sensitive {
		return "", true
	}
	region, _ = plan.Variables[name].Value.(string)
	return region, true
}
func validateCostJSON(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("cost JSON depth")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delimiter != '{' && delimiter != '[' {
		return errors.New("cost JSON delimiter")
	}
	seen := map[string]bool{}
	for decoder.More() {
		if delimiter == '{' {
			if err := costJSONKey(decoder, seen); err != nil {
				return err
			}
		}
		if err := validateCostJSON(decoder, depth+1); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}
func costJSONKey(decoder *json.Decoder, seen map[string]bool) error {
	key, err := decoder.Token()
	if err != nil {
		return err
	}
	name, ok := key.(string)
	if !ok || seen[name] {
		return errors.New("cost JSON duplicate")
	}
	seen[name] = true
	return nil
}
