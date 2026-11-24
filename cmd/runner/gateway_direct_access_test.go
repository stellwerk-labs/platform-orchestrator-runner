package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
)

func TestGatewayDirectAccessRefusesInvalidProfileBeforeBrokerConnection(t *testing.T) {
	for _, cfg := range []config.GatewayModeConfiguration{
		{StaticRunnerUUID: "not-a-uuid"},
		{StaticRunnerUUID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", RetainedPlansEnabled: true},
		{StaticRunnerUUID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", DeploymentAuthorityDPURL: "http://dp:8080"},
		{ControlPlaneURL: "http://core:8080", DeploymentAuthorityDPURL: "http://dp:8080#"},
	} {
		invalid := cfg.ValidateDirectAccess()
		require.Error(t, invalid)
		err := runGateway(t.Context(), &cfg)
		require.EqualError(t, err, invalid.Error(), "configuration refusal must precede the empty broker configuration")
	}
}
