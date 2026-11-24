package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalCostCredentialStaysInPrivateOperatorConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing.key")
	require.NoError(t, os.WriteFile(path, []byte("PRIVATE_PRICING_KEY\n"), 0600))
	cost, err := loadRolloutCost(&rolloutCostConfiguration{APIKeyFile: path, Endpoint: "https://pricing.example.invalid", PricingAccess: "network"})
	require.NoError(t, err)
	require.Equal(t, "PRIVATE_PRICING_KEY", cost.APIKey)
	for _, test := range []struct {
		name          string
		configuration rolloutCostConfiguration
	}{
		{"relative key", rolloutCostConfiguration{APIKeyFile: "pricing.key"}},
		{"relative executable", rolloutCostConfiguration{APIKeyFile: path, Binary: "infracost"}},
		{"cleartext remote", rolloutCostConfiguration{APIKeyFile: path, Endpoint: "http://pricing.example.invalid"}},
		{"embedded credential", rolloutCostConfiguration{APIKeyFile: path, Endpoint: "https://secret@pricing.example.invalid"}},
		{"query", rolloutCostConfiguration{APIKeyFile: path, Endpoint: "https://pricing.example.invalid?token=PRIVATE"}},
		{"invalid access", rolloutCostConfiguration{APIKeyFile: path, PricingAccess: "offline-ish"}},
		{"local without endpoint", rolloutCostConfiguration{APIKeyFile: path, PricingAccess: "local"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadRolloutCost(&test.configuration)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "PRIVATE")
		})
	}
	require.NoError(t, os.Chmod(path, 0644))
	_, err = loadRolloutCost(&rolloutCostConfiguration{APIKeyFile: path})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "PRIVATE")
	require.NoError(t, os.Chmod(path, 0600))
	link := filepath.Join(t.TempDir(), "linked.key")
	require.NoError(t, os.Symlink(path, link))
	_, err = loadRolloutCost(&rolloutCostConfiguration{APIKeyFile: link})
	require.Error(t, err)
}
