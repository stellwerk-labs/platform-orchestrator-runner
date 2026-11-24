package main

import (
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
)

// Only an operator-local secret reference enters the site configuration. The
// delegated command and public catalogue never carry credential material.
type rolloutCostConfiguration struct {
	PricingAccess string `json:"pricing_access,omitempty"`
	APIKeyFile    string `json:"api_key_file"`
	Binary        string `json:"binary,omitempty"`
	Endpoint      string `json:"endpoint,omitempty"`
}

func loadRolloutCost(cfg *rolloutCostConfiguration) (*runner.CostConfiguration, error) {
	if cfg == nil {
		return nil, nil
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	value, err := readPrivatePricingKey(cfg.APIKeyFile)
	if err != nil {
		return nil, err
	}
	return &runner.CostConfiguration{PricingAccess: cfg.PricingAccess, APIKey: value, Binary: cfg.Binary, Endpoint: cfg.Endpoint}, nil
}

func readPrivatePricingKey(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4097 {
		return "", errors.New("invalid site pricing credential")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("read site pricing credential")
	}
	defer func() { _ = f.Close() }()
	key, err := io.ReadAll(io.LimitReader(f, 4098))
	value := strings.TrimSpace(string(key))
	if err != nil || len(key) > 4097 || value == "" || len(value) > 4096 || strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", errors.New("invalid site pricing credential")
	}
	return value, nil
}

func localPricingEndpoint(endpoint *url.URL) bool {
	if endpoint.Scheme != "http" {
		return false
	}
	ip := net.ParseIP(endpoint.Hostname())
	return ip != nil && ip.IsLoopback()
}

func (cfg rolloutCostConfiguration) validate() error {
	switch cfg.PricingAccess {
	case "", "network":
	case "local":
		if cfg.Endpoint == "" {
			return errors.New("invalid site pricing access")
		}
	default:
		return errors.New("invalid site pricing access")
	}
	if !validPricingEndpoint(cfg.Endpoint) {
		return errors.New("invalid site pricing endpoint")
	}

	if cfg.Binary != "" && !filepath.IsAbs(cfg.Binary) {
		return errors.New("invalid site cost executable")
	}
	if !filepath.IsAbs(cfg.APIKeyFile) {
		return errors.New("invalid site pricing credential")
	}

	return nil
}

func validPricingEndpoint(value string) bool {
	if value == "" {
		return true
	}
	endpoint, err := url.Parse(value)
	if err != nil {
		return false
	}
	return endpoint.Host != "" && endpoint.User == nil && endpoint.RawQuery == "" && endpoint.Fragment == "" && (endpoint.Scheme == "https" || localPricingEndpoint(endpoint))
}
