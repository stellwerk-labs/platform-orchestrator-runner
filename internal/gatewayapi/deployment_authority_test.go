package gatewayapi

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeploymentAuthorityURLUsesExplicitSafeBase(t *testing.T) {
	for _, value := range []string{"", "http://dp:8080", "https://dp.example/base", "https://dp.example/base/", "https://dp.example/"} {
		require.NoError(t, ValidateDeploymentAuthorityURL(value), value)
	}
	for _, value := range []string{
		" ", " https://dp.example", "https://dp.example ", "dp.example", "ftp://dp.example", "https:///base", "https://user@dp.example",
		"https://dp.example?", "https://dp.example?x=y", "https://dp.example#", "https://dp.example#anchor", "https://dp.example/a\\b",
		"https://dp.example/a/../b", "https://dp.example/a/./b", "https://dp.example//base", "https://dp.example/base//",
		"https://dp.example//", "https://dp.example/%20", "https://dp.example/%23", "https://dp.example/%00", "https://dp.example/a b",
		"https://dp.example/%2e", "https://dp.example/%252e", "https://dp.example/a%2fb",
	} {
		require.Error(t, ValidateDeploymentAuthorityURL(value), value)
	}
}

func TestDirectDeploymentAccessRejectsContradictoryNativeProfiles(t *testing.T) {
	require.NoError(t, ValidateDirectDeploymentAccess("", false, true))
	require.NoError(t, ValidateDirectDeploymentAccess("http://dp:8080", false, true))
	require.NoError(t, ValidateDirectDeploymentAccess("", true, false))
	require.Error(t, ValidateDirectDeploymentAccess("http://dp:8080", true, false))
	require.Error(t, ValidateDirectDeploymentAccess("", true, true))
}
