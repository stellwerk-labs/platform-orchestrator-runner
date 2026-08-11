package config

import (
	"encoding/base64"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var (
	deploymentId = uuid.New()
	id, _        = age.GenerateX25519Identity()
)

func setStandardGatewayEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("RUNNER_ID", "runner")
	t.Setenv("RUNNER_GATEWAY_URL", "https://gateway.example.com/runner-gateway")
	t.Setenv("TOKEN", "deployment-token")
}

func TestGetStandardModeConfiguration_MissingRequired(t *testing.T) {
	_, err := GetStandardModeConfiguration()
	require.ErrorContains(t, err, "OrgID: missing required value: ORG_ID")
}

func TestGetGatewayModeConfiguration(t *testing.T) {
	t.Setenv("NATS_URL", "nats://localhost:4222")
	t.Setenv("RUNNER_GATEWAY_RECEIPT_KEY", base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("RUNNER_TOKEN_SALT", "salt")
	t.Setenv("CONTROL_PLANE_URL", "http://platform-orchestrator-control-plane:8080")

	conf, err := GetGatewayModeConfiguration()
	require.NoError(t, err)
	require.Equal(t, 8080, conf.Port)
	require.Equal(t, "/runner-gateway", conf.BasePath)
	require.Equal(t, 25*time.Second, conf.FetchWait)
}

func TestGetGatewayModeConfigurationRequiresOneKeySource(t *testing.T) {
	t.Setenv("NATS_URL", "nats://localhost:4222")
	t.Setenv("RUNNER_GATEWAY_RECEIPT_KEY", base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("RUNNER_TOKEN_SALT", "salt")

	_, err := GetGatewayModeConfiguration()
	require.ErrorContains(t, err, "exactly one gateway public key source")
}

func TestGetStandardModeConfiguration_DefaultValues(t *testing.T) {
	t.Setenv("ORG_ID", "test-org")
	setStandardGatewayEnvironment(t)
	t.Setenv("DEPLOYMENT_ID", deploymentId.String())
	t.Setenv("MODE", "deploy")

	conf, err := GetStandardModeConfiguration()
	require.NoError(t, err)
	require.Equal(t, "info", conf.LogLevel)
	require.Equal(t, "test-org", conf.OrgID)
	require.Equal(t, deploymentId.String(), conf.DeploymentID)
	require.Equal(t, "/opt/runner/tofu", conf.IaCCodeDir)
	require.Equal(t, IaCBackendType("opentofu"), conf.IaCBackend)
}

func TestGetStandardModeConfiguration_LegacyTofuCodeDir(t *testing.T) {
	t.Setenv("ORG_ID", "test-org")
	setStandardGatewayEnvironment(t)
	t.Setenv("DEPLOYMENT_ID", deploymentId.String())
	t.Setenv("MODE", "deploy")
	t.Setenv("TOFU_CODE_DIR", "/custom/tofu/path")

	conf, err := GetStandardModeConfiguration()
	require.NoError(t, err)
	require.Equal(t, "/custom/tofu/path", conf.IaCCodeDir)
}

func TestGetStandardModeConfiguration_InvalidDeploymentId(t *testing.T) {
	t.Setenv("ORG_ID", "test-org")
	setStandardGatewayEnvironment(t)
	t.Setenv("DEPLOYMENT_ID", "00000")

	t.Setenv("MODE", "destroy")

	_, err := GetStandardModeConfiguration()
	require.EqualError(t, err, "DEPLOYMENT_ID should be a valid uuid: invalid UUID length: 5")
}

func TestGetStandardModeConfiguration_InvalidMode(t *testing.T) {
	t.Setenv("ORG_ID", "test-org")
	setStandardGatewayEnvironment(t)
	t.Setenv("DEPLOYMENT_ID", "00000")
	t.Setenv("MODE", "destsroy")

	_, err := GetStandardModeConfiguration()
	require.EqualError(t, err, "mode destsroy is not supported")
}

func TestGetStandardModeConfiguration_InvalidBackend(t *testing.T) {
	t.Setenv("ORG_ID", "test-org")
	setStandardGatewayEnvironment(t)
	t.Setenv("DEPLOYMENT_ID", deploymentId.String())
	t.Setenv("MODE", "deploy")
	t.Setenv("IAC_BACKEND", "pulumi")

	_, err := GetStandardModeConfiguration()
	require.EqualError(t, err, `IAC_BACKEND "pulumi" is not supported, must be "opentofu" or "terraform"`)
}

func TestGetStandardModeConfiguration(t *testing.T) {
	t.Setenv("ORG_ID", "test-org")
	setStandardGatewayEnvironment(t)
	t.Setenv("DEPLOYMENT_ID", deploymentId.String())
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("MODE", "plan_only")
	t.Setenv("ENCRYPTING_KEY", id.Recipient().String())

	conf, err := GetStandardModeConfiguration()
	require.NoError(t, err)
	require.Equal(t, "debug", conf.LogLevel)
	require.Equal(t, "test-org", conf.OrgID)
	require.Equal(t, deploymentId.String(), conf.DeploymentID)
	require.Equal(t, id.Recipient().String(), conf.EncryptingKey)
}

func TestGetRemoteModeConfigurationAllowsGatewayURLFlag(t *testing.T) {
	t.Setenv("ORG_ID", "test-org")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("RUNNER_ID", "remote-runner")
	t.Setenv("PRIVATE_KEY", "private-key")

	conf, err := GetRemoteModeConfiguration("https://gateway.example.com/runner-gateway")
	require.NoError(t, err)
	require.Equal(t, "https://gateway.example.com/runner-gateway", conf.Gateway.URL)
}

func TestGetRemoteModeConfiguration(t *testing.T) {
	t.Setenv("ORG_ID", "test-org")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("RUNNER_ID", "remote-runner")
	t.Setenv("PRIVATE_KEY", "private-key")
	t.Setenv("RUNNER_GATEWAY_URL", "https://gateway.example.com/runner-gateway")

	conf, err := GetRemoteModeConfiguration()
	require.NoError(t, err)
	require.Equal(t, "debug", conf.LogLevel)
	require.Equal(t, "test-org", conf.OrgID)
	require.Equal(t, "remote-runner", conf.RunnerId)
	require.Equal(t, "https://gateway.example.com/runner-gateway", conf.Gateway.URL)

}
