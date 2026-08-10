package config

import (
	"testing"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var (
	deploymentId = uuid.New()
	id, _        = age.GenerateX25519Identity()
)

func TestGetStandardModeConfiguration_MissingRequired(t *testing.T) {
	_, err := GetStandardModeConfiguration()
	require.ErrorContains(t, err, "OrgID: missing required value: ORG_ID")
}

func TestGetStandardModeConfiguration_DefaultValues(t *testing.T) {
	t.Setenv("ORG_ID", "test-org")
	t.Setenv("NATS_URL", "nats://localhost:4222")
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
	t.Setenv("NATS_URL", "nats://localhost:4222")
	t.Setenv("DEPLOYMENT_ID", deploymentId.String())
	t.Setenv("MODE", "deploy")
	t.Setenv("TOFU_CODE_DIR", "/custom/tofu/path")

	conf, err := GetStandardModeConfiguration()
	require.NoError(t, err)
	require.Equal(t, "/custom/tofu/path", conf.IaCCodeDir)
}

func TestGetStandardModeConfiguration_InvalidDeploymentId(t *testing.T) {
	t.Setenv("ORG_ID", "test-org")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	t.Setenv("DEPLOYMENT_ID", "00000")

	t.Setenv("MODE", "destroy")

	_, err := GetStandardModeConfiguration()
	require.EqualError(t, err, "DEPLOYMENT_ID should be a valid uuid: invalid UUID length: 5")
}

func TestGetStandardModeConfiguration_InvalidMode(t *testing.T) {
	t.Setenv("ORG_ID", "test-org")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	t.Setenv("DEPLOYMENT_ID", "00000")
	t.Setenv("MODE", "destsroy")

	_, err := GetStandardModeConfiguration()
	require.EqualError(t, err, "mode destsroy is not supported")
}

func TestGetStandardModeConfiguration_InvalidBackend(t *testing.T) {
	t.Setenv("ORG_ID", "test-org")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	t.Setenv("DEPLOYMENT_ID", deploymentId.String())
	t.Setenv("MODE", "deploy")
	t.Setenv("IAC_BACKEND", "pulumi")

	_, err := GetStandardModeConfiguration()
	require.EqualError(t, err, `IAC_BACKEND "pulumi" is not supported, must be "opentofu" or "terraform"`)
}

func TestGetStandardModeConfiguration(t *testing.T) {
	t.Setenv("ORG_ID", "test-org")
	t.Setenv("NATS_URL", "nats://localhost:4222")
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

func TestGetRemoteModeConfiguration_AllowsRemoteConnectFlag(t *testing.T) {
	t.Setenv("ORG_ID", "test-org")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("RUNNER_ID", "remote-runner")
	t.Setenv("NATS_URL", "")

	conf, err := GetRemoteModeConfiguration("nats://localhost:4222")
	require.NoError(t, err)
	require.Equal(t, "nats://localhost:4222", conf.NATS.URL)
}

func TestGetRemoteModeConfiguration(t *testing.T) {
	t.Setenv("ORG_ID", "test-org")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("RUNNER_ID", "remote-runner")
	t.Setenv("NATS_URL", "nats://dev-platform-orchestrator.dev:4222")

	conf, err := GetRemoteModeConfiguration()
	require.NoError(t, err)
	require.Equal(t, "debug", conf.LogLevel)
	require.Equal(t, "test-org", conf.OrgID)
	require.Equal(t, "remote-runner", conf.RunnerId)
	require.Equal(t, "nats://dev-platform-orchestrator.dev:4222", conf.NATS.URL)

}
