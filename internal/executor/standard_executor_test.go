package executor

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/limitedlogsbuffer"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/ref"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
	mockrunner "github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner/mocks"
)

const (
	orgId        = "test-org"
	deploymentId = "test-deployment"
)

var (
	stdCfgConfig *config.StandardModeConfiguration
	programLevel = new(slog.LevelVar)
)

func cleanTmpDir() {
	_ = os.RemoveAll("./tmp")
}

func init() {
	stdCfgConfig = &config.StandardModeConfiguration{
		OrgID:                         orgId,
		DeploymentID:                  deploymentId,
		LogLevel:                      "DEBUG",
		Token:                         "test-token",
		PlatformOrchestratorApiPrefix: "http://localhost:8080",
		IaCCodeDir:                    "./tmp/opt/runner/tofu",
		EncryptingKey:                 "test-encrypting-key",
	}
}

func getMockRunnerFactory(runnerMock *mockrunner.MockRunnerInterface) runner.RunnerFactory {
	return func(ctx context.Context, cfg *config.StandardModeConfiguration) (runner.RunnerInterface, error) {
		return runnerMock, nil
	}
}

func TestExecuteStandardMode_Init_error(t *testing.T) {
	defer cleanTmpDir()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	runnerMock := mockrunner.NewMockRunnerInterface(ctrl)
	runnerMock.EXPECT().CreateIaCFolder(gomock.Any()).DoAndReturn(func(ctx context.Context) error {
		_ = os.MkdirAll("./tmp/opt/runner/tofu", os.ModePerm)
		_, err := os.Create("./tmp/opt/runner/tofu/main.tf")
		return err
	}).Times(1)
	runnerMock.EXPECT().Version(gomock.Any()).Return("1.9.0", nil).Times(1)
	runnerMock.EXPECT().Init(gomock.Any()).Return("", errors.New("failed to init")).Times(1)

	cfg := stdCfgConfig
	cfg.Mode = config.Destroy
	apiUpdate, err := ExecuteStandardMode(t.Context(), cfg, getMockRunnerFactory(runnerMock), &limitedlogsbuffer.LimitedLogsBuffer{}, programLevel)
	require.Equal(t, platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Failure, Error: &platformorchestratorapi.Error{Message: "failed to init", Error: "IAC_COMMAND_ERROR"}}, apiUpdate)
	require.EqualError(t, err, "[IAC]init error: failed to init")
}

func TestExecuteStandardMode_Destroy_success(t *testing.T) {
	defer cleanTmpDir()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	runnerMock := mockrunner.NewMockRunnerInterface(ctrl)
	runnerMock.EXPECT().CreateIaCFolder(gomock.Any()).DoAndReturn(func(ctx context.Context) error {
		_ = os.MkdirAll("./tmp/opt/runner/tofu", os.ModePerm)
		_, err := os.Create("./tmp/opt/runner/tofu/main.tf")
		return err
	}).Times(1)
	runnerMock.EXPECT().Version(gomock.Any()).Return("1.9.0", nil).Times(1)
	runnerMock.EXPECT().Init(gomock.Any()).Return("init-output", nil).Times(1)
	runnerMock.EXPECT().Destroy(gomock.Any()).Return("destroy-output", &runner.IaCChanges{
		Added:  0,
		Change: 0,
		Import: 0,
		Remove: 1,
	}, nil).Times(1)

	cfg := stdCfgConfig
	cfg.Mode = config.Destroy
	apiUpdate, err := ExecuteStandardMode(t.Context(), cfg, getMockRunnerFactory(runnerMock), &limitedlogsbuffer.LimitedLogsBuffer{}, programLevel)
	require.Equal(t, platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Success, TfResourceCounts: platformorchestratorapi.DeploymentTFResourceCounts{NumResourcesRemoved: 1}}, apiUpdate)
	require.NoError(t, err)
}

func TestExecuteStandardMode_Destroy_error(t *testing.T) {
	defer cleanTmpDir()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	runnerMock := mockrunner.NewMockRunnerInterface(ctrl)
	runnerMock.EXPECT().CreateIaCFolder(gomock.Any()).DoAndReturn(func(ctx context.Context) error {
		_ = os.MkdirAll("./tmp/opt/runner/tofu", os.ModePerm)
		_, err := os.Create("./tmp/opt/runner/tofu/main.tf")
		return err
	}).Times(1)
	runnerMock.EXPECT().Version(gomock.Any()).Return("1.9.0", nil).Times(1)
	runnerMock.EXPECT().Init(gomock.Any()).Return("init-output", nil).Times(1)
	runnerMock.EXPECT().Destroy(gomock.Any()).Return("destroy-output", nil, errors.New("failed to destroy")).Times(1)

	cfg := stdCfgConfig
	cfg.Mode = config.Destroy
	apiUpdate, err := ExecuteStandardMode(t.Context(), cfg, getMockRunnerFactory(runnerMock), &limitedlogsbuffer.LimitedLogsBuffer{}, programLevel)
	require.Equal(t, platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Failure, Error: &platformorchestratorapi.Error{Message: "failed to destroy", Error: "IAC_COMMAND_ERROR"}}, apiUpdate)
	require.EqualError(t, err, "[IAC]destroy error: failed to destroy")
}

func TestExecuteStandardMode_PlanOnly_success(t *testing.T) {
	defer cleanTmpDir()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	runnerMock := mockrunner.NewMockRunnerInterface(ctrl)
	runnerMock.EXPECT().CreateIaCFolder(gomock.Any()).DoAndReturn(func(ctx context.Context) error {
		_ = os.MkdirAll("./tmp/opt/runner/tofu", os.ModePerm)
		_, err := os.Create("./tmp/opt/runner/tofu/main.tf")
		return err
	}).Times(1)
	runnerMock.EXPECT().Version(gomock.Any()).Return("1.9.0", nil).Times(1)
	runnerMock.EXPECT().Init(gomock.Any()).Return("init-output", nil).Times(1)
	runnerMock.EXPECT().Plan(gomock.Any()).Return("plan-output", map[string]interface{}{}, &runner.IaCChanges{}, nil).Times(1)
	runnerMock.EXPECT().EncryptString("{}").Return("encrypted-plan-output", nil).Times(1)

	cfg := stdCfgConfig
	cfg.Mode = config.PlanOnly
	apiUpdate, err := ExecuteStandardMode(t.Context(), cfg, getMockRunnerFactory(runnerMock), &limitedlogsbuffer.LimitedLogsBuffer{}, programLevel)
	require.NoError(t, err)
	require.Equal(t, platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Success, Outputs: ref.Ref("encrypted-plan-output")}, apiUpdate)
}

func TestExecuteStandardMode_PlanOnly_error(t *testing.T) {
	defer cleanTmpDir()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	runnerMock := mockrunner.NewMockRunnerInterface(ctrl)
	runnerMock.EXPECT().CreateIaCFolder(gomock.Any()).DoAndReturn(func(ctx context.Context) error {
		_ = os.MkdirAll("./tmp/opt/runner/tofu", os.ModePerm)
		_, err := os.Create("./tmp/opt/runner/tofu/main.tf")
		return err
	}).Times(1)
	runnerMock.EXPECT().Version(gomock.Any()).Return("1.9.0", nil).Times(1)
	runnerMock.EXPECT().Init(gomock.Any()).Return("init-output", nil).Times(1)
	runnerMock.EXPECT().Plan(gomock.Any()).Return("", nil, nil, errors.New("plan error")).Times(1)

	cfg := stdCfgConfig
	cfg.Mode = config.PlanOnly
	apiUpdate, err := ExecuteStandardMode(t.Context(), cfg, getMockRunnerFactory(runnerMock), &limitedlogsbuffer.LimitedLogsBuffer{}, programLevel)
	require.EqualError(t, err, "[IAC]plan error: plan error")
	require.Equal(t, platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Failure, Error: &platformorchestratorapi.Error{Message: "plan error", Error: "IAC_COMMAND_ERROR"}}, apiUpdate)
}

func TestExecuteStandardMode_Deploy_success(t *testing.T) {
	defer cleanTmpDir()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	runnerMock := mockrunner.NewMockRunnerInterface(ctrl)
	runnerMock.EXPECT().CreateIaCFolder(gomock.Any()).DoAndReturn(func(ctx context.Context) error {
		_ = os.MkdirAll("./tmp/opt/runner/tofu", os.ModePerm)
		_, err := os.Create("./tmp/opt/runner/tofu/main.tf")
		return err
	}).Times(1)
	runnerMock.EXPECT().Version(gomock.Any()).Return("1.9.0", nil).Times(1)
	runnerMock.EXPECT().Init(gomock.Any()).Return("init-output", nil).Times(1)
	runnerMock.EXPECT().Plan(gomock.Any()).Return("plan-output", nil, &runner.IaCChanges{}, nil).Times(1)
	runnerMock.EXPECT().Apply(gomock.Any()).Return("apply-output", &runner.IaCChanges{
		Added:  2,
		Change: 1,
		Import: 0,
		Remove: 0,
	}, nil).Times(1)
	runnerMock.EXPECT().OutputMetadata(gomock.Any(), "platform_orchestrator_metadata").Return(`{"node1":{"foo":"bar"}}`, nil).Times(1)
	runnerMock.EXPECT().Output(gomock.Any()).Return(map[string]interface{}{
		"metadata": map[string]interface{}{"foo": "bar"},
		"output1":  "value1",
	}, nil).Times(1)
	runnerMock.EXPECT().EncryptString(gomock.Any()).DoAndReturn(func(s string) (string, error) {
		return "encrypted-outputs", nil
	}).Times(1)

	cfg := stdCfgConfig
	cfg.Mode = config.Deploy
	cfg.MetadataKey = "platform_orchestrator_metadata"
	cfg.EncryptingKey = "some-key"
	apiUpdate, err := ExecuteStandardMode(t.Context(), cfg, getMockRunnerFactory(runnerMock), &limitedlogsbuffer.LimitedLogsBuffer{}, programLevel)
	require.NoError(t, err)
	require.Equal(t, platformorchestratorapi.DeploymentResultsUpdateBody{
		Status:           platformorchestratorapi.Success,
		Outputs:          ref.Ref("encrypted-outputs"),
		TfResourceCounts: platformorchestratorapi.DeploymentTFResourceCounts{NumResourcesAdded: 2, NumResourcesChanged: 1},
		Metadata: []platformorchestratorapi.DeploymentResultMetadataPerNode{
			{NodeId: "node1", Metadata: map[string]interface{}{"foo": "bar"}},
		},
	}, apiUpdate)
}

func TestExecuteStandardMode_Deploy_apply_error(t *testing.T) {
	defer cleanTmpDir()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	runnerMock := mockrunner.NewMockRunnerInterface(ctrl)
	runnerMock.EXPECT().CreateIaCFolder(gomock.Any()).DoAndReturn(func(ctx context.Context) error {
		_ = os.MkdirAll("./tmp/opt/runner/tofu", os.ModePerm)
		_, err := os.Create("./tmp/opt/runner/tofu/main.tf")
		return err
	}).Times(1)
	runnerMock.EXPECT().Version(gomock.Any()).Return("1.9.0", nil).Times(1)
	runnerMock.EXPECT().Init(gomock.Any()).Return("init-output", nil).Times(1)
	runnerMock.EXPECT().Plan(gomock.Any()).Return("plan-output", nil, &runner.IaCChanges{}, nil).Times(1)
	runnerMock.EXPECT().Apply(gomock.Any()).Return("", nil, errors.New("apply failed")).Times(1)

	cfg := stdCfgConfig
	cfg.Mode = config.Deploy
	cfg.MetadataKey = "platform_orchestrator_metadata"
	cfg.EncryptingKey = "some-key"
	apiUpdate, err := ExecuteStandardMode(t.Context(), cfg, getMockRunnerFactory(runnerMock), &limitedlogsbuffer.LimitedLogsBuffer{}, programLevel)
	require.EqualError(t, err, "[IAC]apply error: apply failed")
	require.Equal(t, platformorchestratorapi.DeploymentResultsUpdateBody{
		Status: platformorchestratorapi.Failure,
		Error:  &platformorchestratorapi.Error{Error: "IAC_COMMAND_ERROR", Message: "apply failed"},
	}, apiUpdate)
}

func TestExecuteStandardMode_Deploy_output_metadata_error(t *testing.T) {
	defer cleanTmpDir()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	runnerMock := mockrunner.NewMockRunnerInterface(ctrl)
	runnerMock.EXPECT().CreateIaCFolder(gomock.Any()).DoAndReturn(func(ctx context.Context) error {
		_ = os.MkdirAll("./tmp/opt/runner/tofu", os.ModePerm)
		_, err := os.Create("./tmp/opt/runner/tofu/main.tf")
		return err
	}).Times(1)
	runnerMock.EXPECT().Version(gomock.Any()).Return("1.9.0", nil).Times(1)
	runnerMock.EXPECT().Init(gomock.Any()).Return("init-output", nil).Times(1)
	runnerMock.EXPECT().Plan(gomock.Any()).Return("plan-output", nil, &runner.IaCChanges{}, nil).Times(1)
	runnerMock.EXPECT().Apply(gomock.Any()).Return("apply-output", &runner.IaCChanges{
		Added:  1,
		Change: 2,
		Import: 0,
		Remove: 0,
	}, nil).Times(1)
	runnerMock.EXPECT().OutputMetadata(gomock.Any(), "platform_orchestrator_metadata").Return("", errors.New("metadata fetch failed")).Times(1)

	cfg := stdCfgConfig
	cfg.Mode = config.Deploy
	cfg.MetadataKey = "platform_orchestrator_metadata"
	cfg.EncryptingKey = "some-key"
	apiUpdate, err := ExecuteStandardMode(t.Context(), cfg, getMockRunnerFactory(runnerMock), &limitedlogsbuffer.LimitedLogsBuffer{}, programLevel)
	require.EqualError(t, err, "[IAC]fetch metadata error: metadata fetch failed")
	require.Equal(t, platformorchestratorapi.DeploymentResultsUpdateBody{
		Status: platformorchestratorapi.Failure,
		Error:  &platformorchestratorapi.Error{Error: "IAC_COMMAND_ERROR", Message: "metadata fetch failed"},
	}, apiUpdate)
}

func TestExecuteStandardMode_PlanOnly_excludes_metadata_key(t *testing.T) {
	defer cleanTmpDir()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	runnerMock := mockrunner.NewMockRunnerInterface(ctrl)
	runnerMock.EXPECT().CreateIaCFolder(gomock.Any()).DoAndReturn(func(ctx context.Context) error {
		_ = os.MkdirAll("./tmp/opt/runner/tofu", os.ModePerm)
		_, err := os.Create("./tmp/opt/runner/tofu/main.tf")
		return err
	}).Times(1)
	runnerMock.EXPECT().Version(gomock.Any()).Return("1.9.0", nil).Times(1)
	runnerMock.EXPECT().Init(gomock.Any()).Return("init-output", nil).Times(1)
	runnerMock.EXPECT().Plan(gomock.Any()).Return("plan-output", map[string]interface{}{
		"platform_orchestrator_metadata": map[string]interface{}{"node1": map[string]interface{}{"foo": "bar"}},
		"output1": "value1",
	}, &runner.IaCChanges{}, nil).Times(1)
	runnerMock.EXPECT().EncryptString(gomock.Any()).DoAndReturn(func(s string) (string, error) {
		var outputs map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(s), &outputs))
		require.NotContains(t, outputs, "platform_orchestrator_metadata")
		require.Contains(t, outputs, "output1")
		return "encrypted-plan-output", nil
	}).Times(1)

	cfg := stdCfgConfig
	cfg.Mode = config.PlanOnly
	cfg.MetadataKey = "platform_orchestrator_metadata"
	cfg.EncryptingKey = "some-key"
	apiUpdate, err := ExecuteStandardMode(t.Context(), cfg, getMockRunnerFactory(runnerMock), &limitedlogsbuffer.LimitedLogsBuffer{}, programLevel)
	require.NoError(t, err)
	require.Equal(t, platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Success, Outputs: ref.Ref("encrypted-plan-output")}, apiUpdate)
}

func TestExecuteStandardMode_Deploy_excludes_metadata_key_from_outputs(t *testing.T) {
	defer cleanTmpDir()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	runnerMock := mockrunner.NewMockRunnerInterface(ctrl)
	runnerMock.EXPECT().CreateIaCFolder(gomock.Any()).DoAndReturn(func(ctx context.Context) error {
		_ = os.MkdirAll("./tmp/opt/runner/tofu", os.ModePerm)
		_, err := os.Create("./tmp/opt/runner/tofu/main.tf")
		return err
	}).Times(1)
	runnerMock.EXPECT().Version(gomock.Any()).Return("1.9.0", nil).Times(1)
	runnerMock.EXPECT().Init(gomock.Any()).Return("init-output", nil).Times(1)
	runnerMock.EXPECT().Plan(gomock.Any()).Return("plan-output", nil, &runner.IaCChanges{}, nil).Times(1)
	runnerMock.EXPECT().Apply(gomock.Any()).Return("apply-output", &runner.IaCChanges{Added: 1}, nil).Times(1)
	runnerMock.EXPECT().OutputMetadata(gomock.Any(), "platform_orchestrator_metadata").Return(`{"node1":{"foo":"bar"}}`, nil).Times(1)
	runnerMock.EXPECT().Output(gomock.Any()).Return(map[string]interface{}{
		"platform_orchestrator_metadata": map[string]interface{}{"node1": map[string]interface{}{"foo": "bar"}},
		"output1": "value1",
	}, nil).Times(1)
	runnerMock.EXPECT().EncryptString(gomock.Any()).DoAndReturn(func(s string) (string, error) {
		var outputs map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(s), &outputs))
		require.NotContains(t, outputs, "platform_orchestrator_metadata")
		require.Contains(t, outputs, "output1")
		return "encrypted-outputs", nil
	}).Times(1)

	cfg := stdCfgConfig
	cfg.Mode = config.Deploy
	cfg.MetadataKey = "platform_orchestrator_metadata"
	cfg.EncryptingKey = "some-key"
	apiUpdate, err := ExecuteStandardMode(t.Context(), cfg, getMockRunnerFactory(runnerMock), &limitedlogsbuffer.LimitedLogsBuffer{}, programLevel)
	require.NoError(t, err)
	require.Equal(t, platformorchestratorapi.DeploymentResultsUpdateBody{
		Status:           platformorchestratorapi.Success,
		Outputs:          ref.Ref("encrypted-outputs"),
		TfResourceCounts: platformorchestratorapi.DeploymentTFResourceCounts{NumResourcesAdded: 1},
		Metadata: []platformorchestratorapi.DeploymentResultMetadataPerNode{
			{NodeId: "node1", Metadata: map[string]interface{}{"foo": "bar"}},
		},
	}, apiUpdate)
}

func TestExecuteStandardMode_Deploy_output_error(t *testing.T) {
	defer cleanTmpDir()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	runnerMock := mockrunner.NewMockRunnerInterface(ctrl)
	runnerMock.EXPECT().CreateIaCFolder(gomock.Any()).DoAndReturn(func(ctx context.Context) error {
		_ = os.MkdirAll("./tmp/opt/runner/tofu", os.ModePerm)
		_, err := os.Create("./tmp/opt/runner/tofu/main.tf")
		return err
	}).Times(1)
	runnerMock.EXPECT().Version(gomock.Any()).Return("1.9.0", nil).Times(1)
	runnerMock.EXPECT().Init(gomock.Any()).Return("init-output", nil).Times(1)
	runnerMock.EXPECT().Plan(gomock.Any()).Return("plan-output", nil, &runner.IaCChanges{}, nil).Times(1)
	runnerMock.EXPECT().Apply(gomock.Any()).Return("apply-output", &runner.IaCChanges{
		Added:  1,
		Change: 2,
		Import: 0,
		Remove: 0,
	}, nil).Times(1)
	runnerMock.EXPECT().OutputMetadata(gomock.Any(), "platform_orchestrator_metadata").Return(`{"node1":{"foo":"bar"}}`, nil).Times(1)
	runnerMock.EXPECT().Output(gomock.Any()).Return(nil, errors.New("output fetch failed")).Times(1)

	cfg := stdCfgConfig
	cfg.Mode = config.Deploy
	cfg.MetadataKey = "platform_orchestrator_metadata"
	cfg.EncryptingKey = "some-key"
	apiUpdate, err := ExecuteStandardMode(t.Context(), cfg, getMockRunnerFactory(runnerMock), &limitedlogsbuffer.LimitedLogsBuffer{}, programLevel)
	require.EqualError(t, err, "[IAC]outputs error: output fetch failed")
	require.Equal(t, platformorchestratorapi.DeploymentResultsUpdateBody{
		Status: platformorchestratorapi.Failure,
		Error:  &platformorchestratorapi.Error{Error: "IAC_COMMAND_ERROR", Message: "output fetch failed"},
	}, apiUpdate)
}
