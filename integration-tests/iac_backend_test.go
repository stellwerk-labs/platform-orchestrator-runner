package integrationtests

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-runner/integration-tests/clients/platformorchestratorcp"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/ref"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIaCBackend_TerraformBinaryNotFound verifies that when IAC_BACKEND=terraform is
// configured on the K8s runner but the `terraform` binary is not present in the runner
// image, the deployment fails with an IAC_COMMAND_ERROR (returned when the `terraform
// version` check fails) rather than hanging or reporting a misleading status.
func TestIaCBackend_TerraformBinaryNotFound(t *testing.T) {
	ctx := context.Background()
	internalCpClient := MustInternalControlPlaneClient(t)
	orgId := MustCreateOrgId(t, internalCpClient)
	cpClient := MustControlPlaneClient(t)
	projectId := MustCreateProject(t, cpClient, orgId, "my-app").Id
	k8sRunnerId := "k8s-runner-terraform"
	// Create a runner that passes IAC_BACKEND=terraform to the job container.
	// The runner image ships only the `tofu` binary, so `terraform version` will
	// fail and the deployment must be rejected with IAC_COMMAND_ERROR.
	MustCreateK8sRunnerWithIaCBackend(t, cpClient, orgId, k8sRunnerId, projectId, "terraform")
	envType := MustCreateEnvType(t, cpClient, orgId, "dev").Id
	envId := MustCreateEnv(t, cpClient, orgId, envType, projectId, "my-env").Id

	{
		res, err := cpClient.CreateResourceTypeWithResponse(t.Context(), orgId, platformorchestratorcp.CreateResourceTypeJSONRequestBody{
			Id: "k8s-namespace",
			OutputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{"type": "string"},
				},
			},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}
	{
		res, err := createManagedModuleWithResponse(t, cpClient, orgId, platformorchestratorcp.ModuleCreateBody{
			Id:           "dummy-k8s-namespace",
			ResourceType: "k8s-namespace",
			ModuleSource: ref.Ref("inline"), ModuleSourceCode: ref.Ref(dummyK8sNamespaceSourceCode),
			ModuleInputs: map[string]interface{}{
				"prefix":  "${context.project_id}-${context.env_id}",
				"project": "my-gcp-project",
			},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}
	{
		res, err := cpClient.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, platformorchestratorcp.RuleCreateBody{ModuleId: "dummy-k8s-namespace"})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}

	poDpClient := MustDataPlaneClient(t)
	var dep platformorchestratorapi.Deployment

	{
		res, err := poDpClient.CreateDeploymentWithResponse(ctx, orgId, &platformorchestratorapi.CreateDeploymentParams{}, platformorchestratorapi.CreateDeploymentJSONRequestBody{
			ProjectId:      projectId,
			EnvId:          envId,
			Mode:           platformorchestratorapi.Deploy,
			RunnerLogLevel: ref.Ref(platformorchestratorapi.DeploymentCreateBodyRunnerLogLevelDebug),
			Manifest: &platformorchestratorapi.DeploymentManifest{
				Workloads: map[string]platformorchestratorapi.DeploymentManifestWorkload{
					"sample": {
						Resources: map[string]platformorchestratorapi.DeploymentManifestResource{
							"ns": {Type: "k8s-namespace"},
						},
					},
				},
			},
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			dep = *res.JSON201
			assert.NotEmpty(t, dep.Id)
		}
	}

	// The `terraform version` check fails because the binary is absent, which causes
	// ExecuteStandardMode to return an IAC_COMMAND_ERROR that is sent back via the API.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		res, err := poDpClient.GetDeploymentWithResponse(t.Context(), orgId, dep.Id)
		if assert.NoError(c, err) && assert.Equal(c, http.StatusOK, res.StatusCode()) && assert.NotEmpty(c, res.JSON200.CompletedAt) {
			assert.Equal(c, "failed", res.JSON200.Status)
			// The error code emitted when the version check exec fails.
			assert.Contains(c, res.JSON200.StatusMessage, "IAC_COMMAND_ERROR")
			// Confirms the terraform binary (not tofu) was the one attempted.
			assert.Contains(c, res.JSON200.StatusMessage, "terraform")
		}
	}, 2*time.Minute, 5*time.Second, "deployment not marked as complete after 2m")
}

// TestIaCBackend_OpenTofuExplicitlySet verifies that setting IAC_BACKEND=opentofu
// explicitly produces the same successful outcome as the default (unset) backend.
// This guards against regressions where the explicit value breaks something that
// works when the env var is absent.
func TestIaCBackend_OpenTofuExplicitlySet(t *testing.T) {
	ctx := context.Background()
	internalCpClient := MustInternalControlPlaneClient(t)
	orgId := MustCreateOrgId(t, internalCpClient)
	cpClient := MustControlPlaneClient(t)
	projectId := MustCreateProject(t, cpClient, orgId, "my-app").Id
	k8sRunnerId := "k8s-runner-opentofu"
	MustCreateK8sRunnerWithIaCBackend(t, cpClient, orgId, k8sRunnerId, projectId, "opentofu")
	envType := MustCreateEnvType(t, cpClient, orgId, "dev").Id
	envId := MustCreateEnv(t, cpClient, orgId, envType, projectId, "my-env").Id

	{
		res, err := cpClient.CreateResourceTypeWithResponse(t.Context(), orgId, platformorchestratorcp.CreateResourceTypeJSONRequestBody{
			Id: "k8s-namespace",
			OutputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{"type": "string"},
				},
			},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}
	{
		res, err := createManagedModuleWithResponse(t, cpClient, orgId, platformorchestratorcp.ModuleCreateBody{
			Id:           "dummy-k8s-namespace",
			ResourceType: "k8s-namespace",
			ModuleSource: ref.Ref("inline"), ModuleSourceCode: ref.Ref(dummyK8sNamespaceSourceCode),
			ModuleInputs: map[string]interface{}{
				"prefix":  "${context.project_id}-${context.env_id}",
				"project": "my-gcp-project",
			},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}
	{
		res, err := cpClient.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, platformorchestratorcp.RuleCreateBody{ModuleId: "dummy-k8s-namespace"})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}

	poDpClient := MustDataPlaneClient(t)
	var dep platformorchestratorapi.Deployment

	{
		res, err := poDpClient.CreateDeploymentWithResponse(ctx, orgId, &platformorchestratorapi.CreateDeploymentParams{}, platformorchestratorapi.CreateDeploymentJSONRequestBody{
			ProjectId: projectId,
			EnvId:     envId,
			Mode:      platformorchestratorapi.PlanOnly,
			Manifest: &platformorchestratorapi.DeploymentManifest{
				Workloads: map[string]platformorchestratorapi.DeploymentManifestWorkload{
					"sample": {
						Resources: map[string]platformorchestratorapi.DeploymentManifestResource{
							"ns": {Type: "k8s-namespace"},
						},
					},
				},
			},
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			dep = *res.JSON201
			assert.NotEmpty(t, dep.Id)
		}
	}

	// Expect a successful plan — the opentofu backend is available in the runner image.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		res, err := poDpClient.GetDeploymentWithResponse(t.Context(), orgId, dep.Id)
		if assert.NoError(c, err) && assert.Equal(c, http.StatusOK, res.StatusCode()) && assert.NotEmpty(c, res.JSON200.CompletedAt) {
			assert.Equal(c, "succeeded", res.JSON200.Status)
		}
	}, 2*time.Minute, 5*time.Second, "deployment not marked as complete after 2m")
}
