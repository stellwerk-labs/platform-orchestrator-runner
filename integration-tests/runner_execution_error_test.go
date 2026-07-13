package integrationtests

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-runner/integration-tests/clients/platformorchestratorcp"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/ref"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMissingOutputsError(t *testing.T) {
	ctx := context.Background()
	internalCpClient := MustInternalControlPlaneClient(t)
	orgId := MustCreateOrgId(t, internalCpClient)
	cpClient := MustControlPlaneClient(t)
	projectId := MustCreateProject(t, cpClient, orgId, "my-app").Id
	k8sRunnerId := "k8s-runner"
	MustCreateK8sRunnerWithRule(t, cpClient, orgId, k8sRunnerId, projectId)
	envType := MustCreateEnvType(t, cpClient, orgId, "dev").Id
	env := MustCreateEnv(t, cpClient, orgId, envType, projectId, "my-env")
	envId := env.Id
	moduleId := "dummy-k8s-namespace"

	t.Logf("using org %s", orgId)

	{
		res, err := cpClient.CreateResourceTypeWithResponse(t.Context(), orgId, platformorchestratorcp.CreateResourceTypeJSONRequestBody{Id: "k8s-namespace", Description: ref.Ref("Kubernetes Namespace"), OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}}}})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}
	{
		res, err := cpClient.CreateModuleWithResponse(t.Context(), orgId, platformorchestratorcp.ModuleCreateBody{Id: moduleId, ResourceType: "k8s-namespace",
			ModuleSource: ref.Ref("inline"), ModuleSourceCode: ref.Ref(dummyK8sNamespaceSourceCode), ModuleInputs: map[string]interface{}{"prefix": "${context.project_id}-${context.env_id}", "project": "my-gcp-project"}})
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
						Variables: map[string]string{
							"NAMESPACE": "${resources.ns.outputs.not_existing_output}",
						},
					},
				},
			},
			EncryptedOutputsRecipient: ref.Ref(id.Recipient().String()),
			EncryptedLogsRecipient:    ref.Ref(logsId.Recipient().String()),
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			dep = *res.JSON201
			assert.NotEmpty(t, dep.Id)
		}
	}
	dbConn := MustDatabaseConn(t)
	var rawTofu []byte
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.NoError(t, dbConn.QueryRowContext(t.Context(), "SELECT tofu FROM deployments WHERE id = $1", dep.Id).Scan(&rawTofu))
		require.NotEmpty(t, string(rawTofu))
	}, 30*time.Second, 2*time.Second, "tofu not stored after 30s")

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		res, err := poDpClient.GetDeploymentWithResponse(t.Context(), orgId, dep.Id)
		if assert.NoError(c, err) && assert.Equal(c, http.StatusOK, res.StatusCode()) && assert.NotEmpty(c, res.JSON200.CompletedAt) {
			var parsedError map[string]interface{}
			require.NoError(t, json.Unmarshal([]byte(res.JSON200.StatusMessage), &parsedError))
			assert.Equal(t, "plan", parsedError["action"])
			assert.Equal(t, "Unsupported attribute", parsedError["summary"])
			assert.Equal(t, "This object does not have an attribute named \"not_existing_output\".", parsedError["detail"])
			assert.Equal(t, "output", parsedError["entity_type"])
			assert.Equal(t, "sample", parsedError["entity_id"])
			t.Logf("%v", parsedError)
			assert.True(t, strings.HasPrefix(parsedError["code_hint"].(string), "NAMESPACE = module.k8s-namespace_default_workloadssamplens_"), parsedError["code_hint"])
			assert.Equal(t, "sample", parsedError["workload"])
			assert.Equal(t, "failed", res.JSON200.Status)
		}
	}, 2*time.Minute, 5*time.Second, "deployment not marked as completed after 2m")

	// Check S3 bucket logs
	runnerLogsBucketHandle := MustCreateS3BucketHandle(t)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		logsReader, err := runnerLogsBucketHandle.Object(env.Uuid.String() + "/" + dep.Id.String()).NewReader(ctx)
		if assert.NoError(c, err) {
			defer logsReader.Close()
			var logsBuf bytes.Buffer
			_, err := io.Copy(&logsBuf, logsReader)
			require.NoError(c, err)

			decryptedLogs := decryptBytes(t, logsBuf.Bytes(), &logsId)
			require.Contains(t, decryptedLogs, "\"@message\":\"Error: Unsupported attribute\"")
		}
	}, 30*time.Second, 5*time.Second, "runner logs not found after 30s")
}

func TestProviderConfigurationError_AuthIssue(t *testing.T) {
	ctx := context.Background()
	internalCpClient := MustInternalControlPlaneClient(t)
	orgId := MustCreateOrgId(t, internalCpClient)
	cpClient := MustControlPlaneClient(t)
	projectId := MustCreateProject(t, cpClient, orgId, "my-app").Id
	k8sRunnerId := "k8s-runner"
	MustCreateK8sRunnerWithRule(t, cpClient, orgId, k8sRunnerId, projectId)
	envType := MustCreateEnvType(t, cpClient, orgId, "dev").Id
	envId := MustCreateEnv(t, cpClient, orgId, envType, projectId, "my-env").Id
	provider := MustCreateModuleProvider(t, cpClient, orgId, "prv-"+strings.ToLower(rand.Text()))
	const scoreWorkloadResourceType = "score-workload"
	const moduleId = "dummy-lambda"

	t.Logf("using org %s", orgId)

	{
		res, err := cpClient.CreateResourceTypeWithResponse(t.Context(), orgId, platformorchestratorcp.CreateResourceTypeJSONRequestBody{Id: scoreWorkloadResourceType, OutputSchema: map[string]interface{}{}})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}
	{
		res, err := cpClient.CreateModuleWithResponse(t.Context(), orgId, platformorchestratorcp.ModuleCreateBody{
			Id:              moduleId,
			ResourceType:    scoreWorkloadResourceType,
			ModuleSource:    ref.Ref("git::https://github.com/stellwerk-labs/module-definition-library.git//score-workload/aws-lambda"),
			ProviderMapping: map[string]string{"aws": fmt.Sprintf("%s.%s", provider.ProviderType, provider.Id)},
			ModuleInputs: map[string]interface{}{
				"metadata": map[string]interface{}{"name": "my-lambda-function", "runtime": "nodejs14.x", "handler": "index.handler"},
				"containers": map[string]interface{}{
					"main": map[string]interface{}{
						"image": "platform-orchestrator/example-lambda-nodejs14.x:latest",
						"resources": map[string]interface{}{
							"limits": map[string]interface{}{
								"memory": "128M",
							},
						},
					},
				},
			},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}

	{
		res, err := cpClient.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, platformorchestratorcp.RuleCreateBody{ModuleId: moduleId})
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
							"lmbd": {Type: scoreWorkloadResourceType},
						},
					},
				},
			},
			EncryptedOutputsRecipient: ref.Ref(id.Recipient().String()),
			EncryptedLogsRecipient:    ref.Ref(logsId.Recipient().String()),
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			dep = *res.JSON201
			assert.NotEmpty(t, dep.Id)
		}
	}

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		res, err := poDpClient.GetDeploymentWithResponse(t.Context(), orgId, dep.Id)
		if assert.NoError(c, err) && assert.Equal(c, http.StatusOK, res.StatusCode()) && assert.NotEmpty(c, res.JSON200.CompletedAt) {
			var parsedError map[string]interface{}
			require.NoError(t, json.Unmarshal([]byte(res.JSON200.StatusMessage), &parsedError))
			assert.Contains(t, parsedError["summary"], "Retrieving AWS account details: validating provider credentials: retrieving caller identity from STS: operation error STS: GetCallerIdentity, https response error StatusCode: 403,")
			assert.Equal(t, "plan", parsedError["action"])
			assert.Equal(t, "provider", parsedError["entity_type"])
			assert.Contains(t, parsedError["entity_id"], fmt.Sprintf("%s-%s", provider.ProviderType, provider.Id))
			assert.Equal(t, provider.Id, parsedError["provider_id"])
			assert.Equal(t, provider.ProviderType, parsedError["provider_type"])
			assert.Equal(t, "failed", res.JSON200.Status)
		}
	}, 2*time.Minute, 5*time.Second, "deployment not marked as completed after 2m")
}

func TestModuleError_MissingVariable(t *testing.T) {
	ctx := context.Background()
	internalCpClient := MustInternalControlPlaneClient(t)
	orgId := MustCreateOrgId(t, internalCpClient)
	cpClient := MustControlPlaneClient(t)
	projectId := MustCreateProject(t, cpClient, orgId, "my-app").Id
	k8sRunnerId := "k8s-runner"
	MustCreateK8sRunnerWithRule(t, cpClient, orgId, k8sRunnerId, projectId)
	envType := MustCreateEnvType(t, cpClient, orgId, "dev").Id
	env := MustCreateEnv(t, cpClient, orgId, envType, projectId, "my-env")
	envId := env.Id
	moduleId := "inline-k8s-namespace"
	var moduleVersionId string

	t.Logf("using org %s", orgId)

	{
		res, err := cpClient.CreateResourceTypeWithResponse(t.Context(), orgId, platformorchestratorcp.CreateResourceTypeJSONRequestBody{Id: "k8s-namespace", Description: ref.Ref("Kubernetes Namespace"), OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}}}})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}
	{
		res, err := cpClient.CreateModuleWithResponse(t.Context(), orgId, platformorchestratorcp.ModuleCreateBody{
			Id:           moduleId,
			ResourceType: "k8s-namespace",
			ModuleSource: ref.Ref("inline"),
			ModuleSourceCode: ref.Ref(`
variable "prefix" {
  type = string
}
  
output "name" {
  value = "${var.prefix}-namespace"
}
`)})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
		moduleVersionId = res.JSON201.VersionId
	}

	{
		res, err := cpClient.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, platformorchestratorcp.RuleCreateBody{ModuleId: "inline-k8s-namespace"})
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
						Variables: map[string]string{
							"NAMESPACE": "${resources.ns.outputs.name}",
						},
					},
				},
			},
			EncryptedOutputsRecipient: ref.Ref(id.Recipient().String()),
			EncryptedLogsRecipient:    ref.Ref(logsId.Recipient().String()),
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			dep = *res.JSON201
			assert.NotEmpty(t, dep.Id)
		}
	}
	dbConn := MustDatabaseConn(t)
	var rawTofu []byte
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.NoError(t, dbConn.QueryRowContext(t.Context(), "SELECT tofu FROM deployments WHERE id = $1", dep.Id).Scan(&rawTofu))
		require.NotEmpty(t, string(rawTofu))
	}, 30*time.Second, 2*time.Second, "tofu not stored after 30s")

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		res, err := poDpClient.GetDeploymentWithResponse(t.Context(), orgId, dep.Id)
		if assert.NoError(c, err) && assert.Equal(c, http.StatusOK, res.StatusCode()) && assert.NotEmpty(c, res.JSON200.CompletedAt) {
			var parsedError map[string]interface{}
			require.NoError(t, json.Unmarshal([]byte(res.JSON200.StatusMessage), &parsedError))
			assert.Equal(t, "plan", parsedError["action"])
			assert.Equal(t, "Missing required argument", parsedError["summary"])
			assert.Equal(t, "The argument \"prefix\" is required, but no definition was found.", parsedError["detail"])
			assert.Equal(t, moduleId, parsedError["module_id"])
			assert.Equal(t, moduleVersionId, parsedError["module_version"])
			assert.Equal(t, "failed", res.JSON200.Status)
		}
	}, 2*time.Minute, 5*time.Second, "deployment not marked as completed after 2m")

	// Check S3 bucket logs
	runnerLogsBucketHandle := MustCreateS3BucketHandle(t)
	logsReader, err := runnerLogsBucketHandle.Object(env.Uuid.String() + "/" + dep.Id.String()).NewReader(ctx)
	if assert.NoError(t, err) {
		defer logsReader.Close()
		var logsBuf bytes.Buffer
		_, err := io.Copy(&logsBuf, logsReader)
		require.NoError(t, err)

		decryptedLogs := decryptBytes(t, logsBuf.Bytes(), &logsId)
		require.Contains(t, decryptedLogs, "\"@message\":\"Error: Missing required argument\"")
	}
}

func TestModuleError_BadIndex(t *testing.T) {
	ctx := context.Background()
	internalCpClient := MustInternalControlPlaneClient(t)
	orgId := MustCreateOrgId(t, internalCpClient)
	cpClient := MustControlPlaneClient(t)
	projectId := MustCreateProject(t, cpClient, orgId, "my-app").Id
	k8sRunnerId := "k8s-runner"
	MustCreateK8sRunnerWithRule(t, cpClient, orgId, k8sRunnerId, projectId)
	envType := MustCreateEnvType(t, cpClient, orgId, "dev").Id
	envId := MustCreateEnv(t, cpClient, orgId, envType, projectId, "my-env").Id

	t.Logf("using org %s", orgId)

	{
		res, err := cpClient.CreateResourceTypeWithResponse(t.Context(), orgId, platformorchestratorcp.CreateResourceTypeJSONRequestBody{Id: "k8s-namespace", Description: ref.Ref("Kubernetes Namespace"), OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}}}})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}
	{
		res, err := cpClient.CreateModuleWithResponse(t.Context(), orgId, platformorchestratorcp.ModuleCreateBody{
			Id:           "inline-k8s-namespace",
			ResourceType: "k8s-namespace",
			ModuleSource: ref.Ref("inline"),
			ModuleSourceCode: ref.Ref(`
variable "metadata" {
  type = any
}
  
output "workload_type" {
  value = lookup(var.metadata["annotations"], "score.platform-orchestrator.com/workload-type", "Deployment")
}
`),
			ModuleInputs: map[string]interface{}{"metadata": map[string]interface{}{
				"name": "score-ns",
			}}})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}

	{
		res, err := cpClient.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, platformorchestratorcp.RuleCreateBody{ModuleId: "inline-k8s-namespace"})
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
						Variables: map[string]string{
							"WORKLOAD_TYPE": "${resources.ns.outputs.workload_type}",
						},
					},
				},
			},
			EncryptedOutputsRecipient: ref.Ref(id.Recipient().String()),
			EncryptedLogsRecipient:    ref.Ref(logsId.Recipient().String()),
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			dep = *res.JSON201
			assert.NotEmpty(t, dep.Id)
		}
	}
	dbConn := MustDatabaseConn(t)
	var rawTofu []byte
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.NoError(t, dbConn.QueryRowContext(t.Context(), "SELECT tofu FROM deployments WHERE id = $1", dep.Id).Scan(&rawTofu))
		require.NotEmpty(t, string(rawTofu))
	}, 30*time.Second, 2*time.Second, "tofu not stored after 30s")

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		res, err := poDpClient.GetDeploymentWithResponse(t.Context(), orgId, dep.Id)
		if assert.NoError(c, err) && assert.Equal(c, http.StatusOK, res.StatusCode()) && assert.NotEmpty(c, res.JSON200.CompletedAt) {
			t.Logf("status_message: %s", res.JSON200.StatusMessage)
			var parsedError map[string]interface{}
			require.NoError(t, json.Unmarshal([]byte(res.JSON200.StatusMessage), &parsedError))
			require.Equal(t, "plan", parsedError["action"])
			require.Equal(t, "Invalid index", parsedError["summary"])
			require.Equal(t, "The given key does not identify an element in this collection value.", parsedError["detail"])
			require.Equal(t, string(runner.CategoryOutput), parsedError["entity_type"])
			require.Equal(t, "workload_type", parsedError["workload"])
			require.Equal(t, "value = lookup(var.metadata[\"annotations\"], \"score.platform-orchestrator.com/workload-type\", \"Deployment\")", parsedError["code_hint"])

			require.Equal(t, "failed", res.JSON200.Status)
		}
	}, 2*time.Minute, 5*time.Second, "deployment not marked as completed after 2m")
}

func TestModuleError_MissingProvider(t *testing.T) {
	ctx := context.Background()
	internalCpClient := MustInternalControlPlaneClient(t)
	orgId := MustCreateOrgId(t, internalCpClient)
	cpClient := MustControlPlaneClient(t)
	projectId := MustCreateProject(t, cpClient, orgId, "my-app").Id
	k8sRunnerId := "k8s-runner"
	MustCreateK8sRunnerWithRule(t, cpClient, orgId, k8sRunnerId, projectId)
	envType := MustCreateEnvType(t, cpClient, orgId, "dev").Id
	envId := MustCreateEnv(t, cpClient, orgId, envType, projectId, "my-env").Id
	const scoreWorkloadResourceType = "score-workload"
	const moduleId = "dummy-lambda"

	t.Logf("using org %s", orgId)

	{
		res, err := cpClient.CreateResourceTypeWithResponse(t.Context(), orgId, platformorchestratorcp.CreateResourceTypeJSONRequestBody{Id: scoreWorkloadResourceType, OutputSchema: map[string]interface{}{}})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}
	{
		res, err := cpClient.CreateModuleWithResponse(t.Context(), orgId, platformorchestratorcp.ModuleCreateBody{
			Id:           moduleId,
			ResourceType: scoreWorkloadResourceType,
			ModuleSource: ref.Ref("git::https://github.com/stellwerk-labs/module-definition-library.git//score-workload/aws-lambda"),
			ModuleInputs: map[string]interface{}{
				"metadata": map[string]interface{}{"name": "my-lambda-function", "runtime": "nodejs14.x", "handler": "index.handler"},
				"containers": map[string]interface{}{
					"main": map[string]interface{}{
						"image": "platform-orchestrator/example-lambda-nodejs14.x:latest",
						"resources": map[string]interface{}{
							"limits": map[string]interface{}{
								"memory": "128M",
							},
						},
					},
				},
			},
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}

	{
		res, err := cpClient.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, platformorchestratorcp.RuleCreateBody{ModuleId: moduleId})
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
							"lmbd": {Type: scoreWorkloadResourceType},
						},
					},
				},
			},
			EncryptedOutputsRecipient: ref.Ref(id.Recipient().String()),
			EncryptedLogsRecipient:    ref.Ref(logsId.Recipient().String()),
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			dep = *res.JSON201
			assert.NotEmpty(t, dep.Id)
		}
	}

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		res, err := poDpClient.GetDeploymentWithResponse(t.Context(), orgId, dep.Id)
		if assert.NoError(c, err) && assert.Equal(c, http.StatusOK, res.StatusCode()) && assert.NotEmpty(c, res.JSON200.CompletedAt) {
			t.Logf("status error message: %s", res.JSON200.StatusMessage)
			var parsedError map[string]interface{}
			require.NoError(t, json.Unmarshal([]byte(res.JSON200.StatusMessage), &parsedError))
			assert.Equal(t, "Invalid provider configuration", parsedError["summary"])
			assert.Contains(t, parsedError["detail"], "Provider \"registry.opentofu.org/hashicorp/aws\" requires explicit configuration.")
			assert.Equal(t, "plan", parsedError["action"])
			assert.Equal(t, "failed", res.JSON200.Status)
		}
	}, 2*time.Minute, 5*time.Second, "deployment not marked as completed after 2m")
}

func TestModuleError_WrongSourceCode(t *testing.T) {
	ctx := context.Background()
	internalCpClient := MustInternalControlPlaneClient(t)
	orgId := MustCreateOrgId(t, internalCpClient)
	cpClient := MustControlPlaneClient(t)
	projectId := MustCreateProject(t, cpClient, orgId, "my-app").Id
	k8sRunnerId := "k8s-runner"
	MustCreateK8sRunnerWithRule(t, cpClient, orgId, k8sRunnerId, projectId)
	envType := MustCreateEnvType(t, cpClient, orgId, "dev").Id
	envId := MustCreateEnv(t, cpClient, orgId, envType, projectId, "my-env").Id

	t.Logf("using org %s", orgId)

	{
		res, err := cpClient.CreateResourceTypeWithResponse(t.Context(), orgId, platformorchestratorcp.CreateResourceTypeJSONRequestBody{Id: "k8s-namespace", Description: ref.Ref("Kubernetes Namespace"), OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}}}})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}
	{
		res, err := cpClient.CreateModuleWithResponse(t.Context(), orgId, platformorchestratorcp.ModuleCreateBody{
			Id:           "inline-k8s-namespace",
			ResourceType: "k8s-namespace",
			ModuleSource: ref.Ref("git::https://github.com/stellwerk-labs/module-definition-library.git//score-workload/aws-lambdas"),
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}

	{
		res, err := cpClient.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, platformorchestratorcp.RuleCreateBody{ModuleId: "inline-k8s-namespace"})
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
						Outputs: map[string]string{
							"NAMESPACE": "${resources.ns.outputs.name}",
						},
					},
				},
			},
			EncryptedOutputsRecipient: ref.Ref(id.Recipient().String()),
			EncryptedLogsRecipient:    ref.Ref(logsId.Recipient().String()),
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			dep = *res.JSON201
			assert.NotEmpty(t, dep.Id)
		}
	}
	dbConn := MustDatabaseConn(t)
	var rawTofu []byte
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.NoError(t, dbConn.QueryRowContext(t.Context(), "SELECT tofu FROM deployments WHERE id = $1", dep.Id).Scan(&rawTofu))
		require.NotEmpty(t, string(rawTofu))
	}, 30*time.Second, 2*time.Second, "tofu not stored after 30s")

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		res, err := poDpClient.GetDeploymentWithResponse(t.Context(), orgId, dep.Id)
		if assert.NoError(c, err) && assert.Equal(c, http.StatusOK, res.StatusCode()) && assert.NotEmpty(c, res.JSON200.CompletedAt) {
			var parsedError map[string]interface{}
			t.Logf("status message: %s", res.JSON200.StatusMessage)
			require.NoError(t, json.Unmarshal([]byte(res.JSON200.StatusMessage), &parsedError))
			assert.Equal(t, "init", parsedError["action"])
			assert.Equal(t, "Failed to expand subdir globs", parsedError["summary"])
			assert.Equal(t, "subdir \"score-workload/aws-lambdas\" not found", parsedError["detail"])
			assert.Equal(t, "failed", res.JSON200.Status)
		}
	}, 2*time.Minute, 5*time.Second, "deployment not marked as completed after 2m")
}

func TestModuleResourceError(t *testing.T) {
	ctx := context.Background()
	internalCpClient := MustInternalControlPlaneClient(t)
	orgId := MustCreateOrgId(t, internalCpClient)
	cpClient := MustControlPlaneClient(t)
	projectId := MustCreateProject(t, cpClient, orgId, "my-app").Id
	k8sRunnerId := "k8s-runner"
	MustCreateK8sRunnerWithRule(t, cpClient, orgId, k8sRunnerId, projectId)
	envType := MustCreateEnvType(t, cpClient, orgId, "dev").Id
	envId := MustCreateEnv(t, cpClient, orgId, envType, projectId, "my-env").Id
	var provider platformorchestratorcp.ModuleProvider
	var moduleVersionId string

	t.Logf("using org %s", orgId)

	{
		res, err := cpClient.CreateModuleProviderWithResponse(t.Context(), orgId, platformorchestratorcp.CreateModuleProviderJSONRequestBody{
			Id:                "kubernetes",
			ProviderType:      "kubernetes",
			Configuration:     map[string]interface{}{},
			Source:            "hashicorp/kubernetes",
			VersionConstraint: ">= 2.0",
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
		provider = *res.JSON201
	}

	{
		res, err := cpClient.CreateResourceTypeWithResponse(t.Context(), orgId, platformorchestratorcp.CreateResourceTypeJSONRequestBody{Id: "k8s-namespace", Description: ref.Ref("Kubernetes Namespace"), OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}}}})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}
	{
		res, err := cpClient.CreateModuleWithResponse(t.Context(), orgId, platformorchestratorcp.ModuleCreateBody{
			Id:           "inline-k8s-namespace",
			ResourceType: "k8s-namespace",
			ModuleSource: ref.Ref("inline"),
			ModuleSourceCode: ref.Ref(`
variable "trigger-error" {
  type = bool
}

locals {
  ns_name = var.trigger-error ? null : "example-namespace"
}

resource "kubernetes_manifest" "namespace" {
  manifest = {
    apiVersion = "v1"
    kind       = "Namespace"
    metadata = {
      name = "example-namespace"
      annotations = {
        "name-prefix-part-0" = split("-", local.ns_name)[0]
      }
    }
  }
}

output "name" {
  value = kubernetes_manifest.namespace.object.metadata.name
}

`),
			ModuleInputs:    map[string]interface{}{"trigger-error": true},
			ProviderMapping: map[string]string{"kubernetes": fmt.Sprintf("%s.%s", provider.ProviderType, provider.Id)},
		})

		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
		moduleVersionId = res.JSON201.VersionId
	}

	{
		res, err := cpClient.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, platformorchestratorcp.RuleCreateBody{ModuleId: "inline-k8s-namespace"})
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
						Outputs: map[string]string{
							"namespace_name": "${resources.ns.outputs.name}",
						},
					},
				},
			},
			EncryptedOutputsRecipient: ref.Ref(id.Recipient().String()),
			EncryptedLogsRecipient:    ref.Ref(logsId.Recipient().String()),
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			dep = *res.JSON201
			assert.NotEmpty(t, dep.Id)
		}
	}
	dbConn := MustDatabaseConn(t)
	var rawTofu []byte
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.NoError(t, dbConn.QueryRowContext(t.Context(), "SELECT tofu FROM deployments WHERE id = $1", dep.Id).Scan(&rawTofu))
		require.NotEmpty(t, string(rawTofu))
	}, 30*time.Second, 2*time.Second, "tofu not stored after 30s")

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		res, err := poDpClient.GetDeploymentWithResponse(t.Context(), orgId, dep.Id)
		if assert.NoError(c, err) && assert.Equal(c, http.StatusOK, res.StatusCode()) && assert.NotEmpty(c, res.JSON200.CompletedAt) {
			statusMessageParsedError := strings.TrimPrefix(res.JSON200.StatusMessage, `runner failed with code TF_DIAGNOSTIC_ERROR: `)
			t.Logf("parsed error: %s", statusMessageParsedError)
			var parsedError map[string]interface{}
			require.NoError(t, json.Unmarshal([]byte(statusMessageParsedError), &parsedError))
			require.Equal(t, "plan", parsedError["action"])
			require.Equal(t, "Invalid function argument", parsedError["summary"])
			require.Equal(t, "Invalid value for \"str\" parameter: argument must not be null.", parsedError["detail"])
			require.Equal(t, "inline-k8s-namespace", parsedError["module_id"])
			require.Equal(t, moduleVersionId, parsedError["module_version"])
			require.Equal(t, "module", parsedError["entity_type"])
			require.Equal(t, "failed", res.JSON200.Status)
		}
	}, 2*time.Minute, 5*time.Second, "deployment not marked as completed after 2m")
}
