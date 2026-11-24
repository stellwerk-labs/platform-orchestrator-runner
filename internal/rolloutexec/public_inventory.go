package rolloutexec

import (
	"errors"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

type PublicTool struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	Phase                string   `json:"phase"`
	Retry                string   `json:"retry"`
	RequiredCapabilities []string `json:"required_capabilities"`
}

type PublicInventory struct {
	ToolsDigest  string
	Capabilities []string
	Tools        []PublicTool
}

var publicToolName = regexp.MustCompile(`^[^\p{Cc}]{1,120}$`)
var publicCapability = regexp.MustCompile(`^[a-z][a-z0-9_.:/-]{0,127}$`)

// PublicInventory validates installed tools without opening execution storage.
// The opaque digest still binds the original private configuration.
func (e *Executor) PublicInventory() (PublicInventory, error) {
	capabilities, err := publicCapabilities(e.Capabilities)
	if err != nil || len(e.Tools) > 32 || e.validateInventoryCost() != nil || executableDigest("opentofu") == "unavailable" && executableDigest("terraform") == "unavailable" {
		return PublicInventory{}, errors.New("invalid public execution inventory")
	}
	result := PublicInventory{Capabilities: capabilities, Tools: make([]PublicTool, 0, len(e.Tools))}
	seen := map[string]bool{}
	for _, tool := range e.Tools {
		if !identifier.MatchString(tool.ID) || seen[tool.ID] || !utf8.ValidString(tool.Name) || !publicToolName.MatchString(tool.Name) || strings.TrimSpace(tool.Name) == "" || e.validateTool(tool) != nil {
			return PublicInventory{}, errors.New("invalid public execution tool")
		}
		required, err := publicCapabilities(append(append([]string{}, tool.RequiredCapabilities...), tool.Definition.Capabilities...))
		if err != nil {
			return PublicInventory{}, err
		}
		result.Tools = append(result.Tools, PublicTool{ID: tool.ID, Name: tool.Name, Phase: tool.Phase, Retry: tool.Retry, RequiredCapabilities: required})
		seen[tool.ID] = true
	}
	sort.Slice(result.Tools, func(i, j int) bool { return result.Tools[i].ID < result.Tools[j].ID })
	result.ToolsDigest = e.CatalogDigest()
	return result, nil
}

func publicCapabilities(values []string) ([]string, error) {
	result := append([]string{}, values...)
	slices.Sort(result)
	result = slices.Compact(result)
	if len(result) > 32 {
		return nil, errors.New("invalid public execution capabilities")
	}
	for _, capability := range result {
		if !publicCapability.MatchString(capability) {
			return nil, errors.New("invalid public execution capabilities")
		}
	}
	return result, nil
}

func (e *Executor) validateInventoryCost() error {
	for _, capability := range []string{CostCapability, NetworkPricingCapability, LocalPricingCapability} {
		if contains(e.Capabilities, capability) && (e.Cost == nil || capability != CostCapability && capability != e.PricingCapability()) {
			return errors.New("invalid local pricing inventory")
		}
	}
	if e.Cost != nil && (e.Cost.APIKey == "" || e.costIdentity().BinaryDigest == "unavailable") {
		return errors.New("unavailable local pricing inventory")
	}
	return nil
}
