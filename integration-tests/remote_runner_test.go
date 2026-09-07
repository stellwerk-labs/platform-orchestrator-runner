package integrationtests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-runner/integration-tests/clients/platformorchestratorcp"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/ref"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	runnerId         = "remote-runner"
	defaultNamespace = "platform-orchestrator-runner"
)

func TestRemoteRunner_Success(t *testing.T) {
	ctx := context.Background()
	internalCpClient := MustInternalControlPlaneClient(t)
	orgId := MustCreateOrgId(t, internalCpClient)
	cpClient := MustControlPlaneClient(t)
	projectId := MustCreateProject(t, cpClient, orgId, "my-project").Id
	_, privateKey := MustCreateRemoteRunnerWithRule(t, cpClient, orgId, runnerId, projectId, defaultNamespace, "platform-orchestrator-runner")
	envType := MustCreateEnvType(t, cpClient, orgId, "dev").Id
	env := MustCreateEnv(t, cpClient, orgId, envType, projectId, "my-env")
	envId := env.Id
	MustDeployRemoteRunner(t, orgId, privateKey)

	t.Logf("using org %s", orgId)

	{
		res, err := cpClient.CreateResourceTypeWithResponse(t.Context(), orgId, platformorchestratorcp.CreateResourceTypeJSONRequestBody{Id: "k8s-namespace", Description: ref.Ref("Kubernetes Namespace"), OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}}}})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}
	{
		res, err := createManagedModuleWithResponse(t, cpClient, orgId, platformorchestratorcp.ModuleCreateBody{Id: "dummy-k8s-namespace", ResourceType: "k8s-namespace",
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
	var id = getAgeIdentity()

	// TODO: here we are waiting for the remote runner to be ready, this should be done in a more robust way with a health check or similar
	time.Sleep(5 * time.Second)
	t.Run("tofu plan", func(t *testing.T) {
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
							Variables: map[string]string{
								"NAMESPACE": "${resources.ns.outputs.name}",
							},
						},
					},
				},
				EncryptedOutputsRecipient: ref.Ref(id.Recipient().String()),
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

		var outputs []byte
		// check state and outputs have been updated
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			var status string
			var completedAt *time.Time
			var tfResourcesB []byte
			if assert.NoError(c, dbConn.QueryRowContext(t.Context(), "SELECT outputs, status, completed_at, metrics FROM deployments WHERE id = $1", dep.Id).Scan(&outputs, &status, &completedAt, &tfResourcesB)) {
				if assert.NotEmpty(c, completedAt) {
					require.Equal(t, "succeeded", status, fmt.Sprintf("deployment %s not marked as succeeded but %s", dep.Id, status))
					require.NotEmpty(t, outputs)
					require.NotEmpty(t, tfResourcesB)
					var tfResources map[string]interface{}
					require.NoError(t, json.Unmarshal(tfResourcesB, &tfResources))
					require.Equal(t, map[string]interface{}{
						"tf_total":       float64(0),
						"tf_added":       float64(1),
						"tf_changed":     float64(0),
						"tf_removed":     float64(0),
						"resource_nodes": float64(1),
						"workloads":      float64(1),
					}, tfResources)
				}

			}
		}, 30*time.Second, 2*time.Second, fmt.Sprintf("deployment %s results not updated after 30s", dep.Id))
		assert.JSONEq(t, `{"sample":{"NAMESPACE":"(known after apply)"}}`, decryptBytes(t, outputs, &id))
	})

	t.Run("tofu apply", func(t *testing.T) {
		{
			res, err := poDpClient.CreateDeploymentWithResponse(ctx, orgId, &platformorchestratorapi.CreateDeploymentParams{}, platformorchestratorapi.CreateDeploymentJSONRequestBody{
				ProjectId: projectId,
				EnvId:     envId,
				Mode:      platformorchestratorapi.Deploy,
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

		// check state and outputs have been updated
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			res, err := poDpClient.WaitForDeploymentCompleteWithResponse(t.Context(), dep.OrgId, dep.Id, &platformorchestratorapi.WaitForDeploymentCompleteParams{})
			if assert.NoError(t, err) && assert.Equal(c, http.StatusOK, res.StatusCode()) {
				dep = *res.JSON200
				if assert.Equal(c, "succeeded", res.JSON200.Status, "deployment %q created at %s terminated with status %s at %s with message %s", dep.Id, dep.CreatedAt, res.JSON200.Status, dep.CompletedAt, res.JSON200.StatusMessage) {
					assert.Equal(t, platformorchestratorapi.DeploymentMetrics{
						NumTfResourcesRemoved: ref.Ref(0),
						NumResourceNodes:      1,
						NumWorkloads:          1,
						NumTfResources:        ref.Ref(0),
						NumTfResourcesAdded:   ref.Ref(1),
						NumTfResourcesChanged: ref.Ref(0),
					}, res.JSON200.Metrics)
					outRes, err := poDpClient.GetDeploymentEncryptedOutputsWithResponse(t.Context(), orgId, dep.Id)
					if assert.NoError(t, err) && assert.Equal(t, http.StatusOK, outRes.StatusCode()) {
						decryptedOutputs := decryptBytes(t, []byte(outRes.JSON200.Raw), &id)
						match := regexp.MustCompile(`"NAMESPACE":\s*"([^"]*)"`).FindStringSubmatch(decryptedOutputs)
						var outputsMap map[string]interface{}
						require.Greater(t, len(match), 1, "namespace not found in outputs: %s", decryptedOutputs)
						require.NoError(t, json.Unmarshal([]byte(decryptedOutputs), &outputsMap))
						require.Equal(t, map[string]interface{}{"NAMESPACE": match[1]}, outputsMap["sample"])
					}
				}
			}
		}, 2*time.Minute, 10*time.Second, "deployment results not updated after 2 mins")

		require.EventuallyWithT(t, func(c *assert.CollectT) {
			res, err := poDpClient.ListActiveResourceNodesWithResponse(t.Context(), orgId, &platformorchestratorapi.ListActiveResourceNodesParams{ProjectId: &projectId, EnvId: &envId})
			if assert.NoError(c, err) && assert.Equal(c, http.StatusOK, res.StatusCode()) {
				nsIdx := slices.IndexFunc(res.JSON200.Items, func(n platformorchestratorapi.ActiveResourceNode) bool {
					return n.ResourceType == "k8s-namespace"
				})
				if assert.GreaterOrEqual(c, nsIdx, 0) {
					assert.Equal(t, map[string]interface{}{"project": "my-gcp-project"}, res.JSON200.Items[nsIdx].Metadata)
				}
			}
		}, 30*time.Second, 2*time.Second, "active resource nodes not updates after 30s")

		// Logs are transported through the NATS Object Store and served by the data plane.
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			logs, err := poDpClient.GetDeploymentLogsWithResponse(t.Context(), orgId, dep.Id, &platformorchestratorapi.GetDeploymentLogsParams{DecryptKey: ref.Ref(logsId.String())})
			if assert.NoError(c, err) && assert.Equal(c, http.StatusOK, logs.StatusCode(), string(logs.Body)) {
				assert.Contains(c, string(logs.Body), "platform-orchestrator runner completed successfully")
			}
		}, 30*time.Second, 5*time.Second, "runner logs not available through the data plane after 30s")
	})

	t.Run("tofu destroy", func(t *testing.T) {
		{
			res, err := poDpClient.CreateDeploymentWithResponse(ctx, orgId, &platformorchestratorapi.CreateDeploymentParams{}, platformorchestratorapi.CreateDeploymentJSONRequestBody{
				ProjectId: projectId,
				EnvId:     envId,
				Mode:      platformorchestratorapi.Deploy,
				Manifest: &platformorchestratorapi.DeploymentManifest{
					Workloads: map[string]platformorchestratorapi.DeploymentManifestWorkload{},
				},
			})
			if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
				dep = *res.JSON201
				assert.NotEmpty(t, dep.Id)
			}
		}

		// check deployment has been marked as completed
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			res, err := poDpClient.GetDeploymentWithResponse(t.Context(), orgId, dep.Id)
			if assert.NoError(c, err) && assert.Equal(c, http.StatusOK, res.StatusCode()) && assert.NotEmpty(c, res.JSON200.CompletedAt) {
				require.Equal(t, string(platformorchestratorapi.Deploy), res.JSON200.Mode)
				require.Equal(t, "succeeded", res.JSON200.Status)
				require.Equal(t, platformorchestratorapi.DeploymentMetrics{
					NumTfResourcesRemoved: ref.Ref(1),
					NumResourceNodes:      1,
					NumWorkloads:          0,
					NumTfResources:        ref.Ref(0),
					NumTfResourcesAdded:   ref.Ref(0),
					NumTfResourcesChanged: ref.Ref(0),
				}, res.JSON200.Metrics)
			}
		}, 30*time.Second, 2*time.Second, "deployment not marked as completed after 30s")
	})
}

func TestRemoteRunner_Failure(t *testing.T) {
	ctx := context.Background()
	internalCpClient := MustInternalControlPlaneClient(t)
	orgId := MustCreateOrgId(t, internalCpClient)
	cpClient := MustControlPlaneClient(t)
	projectId := MustCreateProject(t, cpClient, orgId, "my-project").Id
	_, privateKey := MustCreateRemoteRunnerWithRule(t, cpClient, orgId, runnerId, projectId, defaultNamespace, "platform-orchestrator-runner")
	envType := MustCreateEnvType(t, cpClient, orgId, "dev").Id
	envId := MustCreateEnv(t, cpClient, orgId, envType, projectId, "my-env").Id
	MustDeployRemoteRunner(t, orgId, privateKey)

	t.Logf("using org %s", orgId)

	{
		res, err := cpClient.CreateResourceTypeWithResponse(t.Context(), orgId, platformorchestratorcp.CreateResourceTypeJSONRequestBody{Id: "k8s-namespace", Description: ref.Ref("Kubernetes Namespace"), OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}}}})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}
	{
		res, err := createManagedModuleWithResponse(t, cpClient, orgId, platformorchestratorcp.ModuleCreateBody{Id: "dummy-k8s-namespace", ResourceType: "k8s-namespace",
			ModuleSource: ref.Ref("inline"), ModuleSourceCode: ref.Ref(dummyK8sNamespaceSourceCode), ModuleInputs: map[string]interface{}{"prefix": "${context.project_id}-${context.env_id}", "project": "my-gcp-project"}})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}

	{
		res, err := cpClient.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, platformorchestratorcp.RuleCreateBody{ModuleId: "dummy-k8s-namespace"})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}

	// TODO: here we are waiting for the remote runner to be ready, this should be done in a more robust way with a health check or similar
	time.Sleep(5 * time.Second)

	poDpClient := MustDataPlaneClient(t)
	var dep platformorchestratorapi.Deployment

	{
		res, err := poDpClient.CreateDeploymentWithResponse(ctx, orgId, &platformorchestratorapi.CreateDeploymentParams{}, platformorchestratorapi.CreateDeploymentJSONRequestBody{
			ProjectId: projectId,
			EnvId:     envId,
			Mode:      platformorchestratorapi.Deploy,
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
			require.Contains(t, res.JSON200.StatusMessage, `Unsupported attribute`, fmt.Sprintf("deployment %s did not fail with the expected error", dep.Id))
			require.Equal(t, "failed", res.JSON200.Status)
		}
	}, 30*time.Second, 2*time.Second, "deployment not marked as completed after 30s")
}

func TestRemoteRunner_Failure_job_creation(t *testing.T) {
	ctx := context.Background()
	internalCpClient := MustInternalControlPlaneClient(t)
	orgId := MustCreateOrgId(t, internalCpClient)
	cpClient := MustControlPlaneClient(t)
	projectId := MustCreateProject(t, cpClient, orgId, "my-project").Id
	_, privateKey := MustCreateRemoteRunnerWithRule(t, cpClient, orgId, runnerId, projectId, "not-existing-namespace", "platform-orchestrator-runner")
	envType := MustCreateEnvType(t, cpClient, orgId, "dev").Id
	envId := MustCreateEnv(t, cpClient, orgId, envType, projectId, "my-env").Id
	MustDeployRemoteRunner(t, orgId, privateKey)

	t.Logf("using org %s", orgId)

	{
		res, err := cpClient.CreateResourceTypeWithResponse(t.Context(), orgId, platformorchestratorcp.CreateResourceTypeJSONRequestBody{Id: "k8s-namespace", Description: ref.Ref("Kubernetes Namespace"), OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}}}})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}
	{
		res, err := createManagedModuleWithResponse(t, cpClient, orgId, platformorchestratorcp.ModuleCreateBody{Id: "dummy-k8s-namespace", ResourceType: "k8s-namespace",
			ModuleSource: ref.Ref("inline"), ModuleSourceCode: ref.Ref(dummyK8sNamespaceSourceCode), ModuleInputs: map[string]interface{}{"prefix": "${context.project_id}-${context.env_id}", "project": "my-gcp-project"}})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}

	{
		res, err := cpClient.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, platformorchestratorcp.RuleCreateBody{ModuleId: "dummy-k8s-namespace"})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}

	// TODO: here we are waiting for the remote runner to be ready, this should be done in a more robust way with a health check or similar
	time.Sleep(5 * time.Second)

	poDpClient := MustDataPlaneClient(t)
	var dep platformorchestratorapi.Deployment

	{
		res, err := poDpClient.CreateDeploymentWithResponse(ctx, orgId, &platformorchestratorapi.CreateDeploymentParams{}, platformorchestratorapi.CreateDeploymentJSONRequestBody{
			ProjectId: projectId,
			EnvId:     envId,
			Mode:      platformorchestratorapi.Deploy,
			Manifest: &platformorchestratorapi.DeploymentManifest{
				Workloads: map[string]platformorchestratorapi.DeploymentManifestWorkload{},
			},
			EncryptedOutputsRecipient: ref.Ref(id.Recipient().String()),
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			dep = *res.JSON201
			assert.NotEmpty(t, dep.Id)
		}
	}

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		res, err := poDpClient.GetDeploymentWithResponse(t.Context(), orgId, dep.Id)
		if assert.NoError(c, err) && assert.Equal(c, http.StatusOK, res.StatusCode()) && assert.NotEmpty(c, res.JSON200.CompletedAt) {
			require.Contains(t, res.JSON200.StatusMessage, "failed to create job: jobs.batch is forbidden: User \"system:serviceaccount:platform-orchestrator-runner:platform-orchestrator-runner-remote\" cannot create resource \"jobs\" in API group \"batch\" in the namespace \"not-existing-namespace\"", fmt.Sprintf("deployment %s did not fail with the expected error", dep.Id))
			require.Equal(t, "failed", res.JSON200.Status)
		}
	}, 30*time.Second, 2*time.Second, "deployment not marked as completed after 30s")
}

func TestRemoteRunner_ServiceAccount_NotExisting(t *testing.T) {
	ctx := context.Background()
	internalCpClient := MustInternalControlPlaneClient(t)
	orgId := MustCreateOrgId(t, internalCpClient)
	cpClient := MustControlPlaneClient(t)
	projectId := MustCreateProject(t, cpClient, orgId, "my-project").Id
	_, privateKey := MustCreateRemoteRunnerWithRule(t, cpClient, orgId, runnerId, projectId, defaultNamespace, "not-existing-service-account")
	envType := MustCreateEnvType(t, cpClient, orgId, "dev").Id
	envId := MustCreateEnv(t, cpClient, orgId, envType, projectId, "my-env").Id
	MustDeployRemoteRunner(t, orgId, privateKey)

	t.Logf("using org %s", orgId)

	{
		res, err := cpClient.CreateResourceTypeWithResponse(t.Context(), orgId, platformorchestratorcp.CreateResourceTypeJSONRequestBody{Id: "k8s-namespace", Description: ref.Ref("Kubernetes Namespace"), OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}}}})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}
	{
		res, err := createManagedModuleWithResponse(t, cpClient, orgId, platformorchestratorcp.ModuleCreateBody{Id: "dummy-k8s-namespace", ResourceType: "k8s-namespace",
			ModuleSource: ref.Ref("inline"), ModuleSourceCode: ref.Ref(dummyK8sNamespaceSourceCode), ModuleInputs: map[string]interface{}{"prefix": "${context.project_id}-${context.env_id}", "project": "my-gcp-project"}})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}

	{
		res, err := cpClient.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, platformorchestratorcp.RuleCreateBody{ModuleId: "dummy-k8s-namespace"})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}

	time.Sleep(5 * time.Second)

	poDpClient := MustDataPlaneClient(t)
	var dep platformorchestratorapi.Deployment

	{
		res, err := poDpClient.CreateDeploymentWithResponse(ctx, orgId, &platformorchestratorapi.CreateDeploymentParams{}, platformorchestratorapi.CreateDeploymentJSONRequestBody{
			ProjectId: projectId,
			EnvId:     envId,
			Mode:      platformorchestratorapi.Deploy,
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
		})
		if assert.NoError(t, err) && assert.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body)) {
			dep = *res.JSON201
			assert.NotEmpty(t, dep.Id)
		}
	}

	var statusMessage string
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		res, err := poDpClient.GetDeploymentWithResponse(t.Context(), orgId, dep.Id)
		if assert.NoError(c, err) && assert.Equal(c, http.StatusOK, res.StatusCode()) && assert.NotEmpty(c, res.JSON200.CompletedAt) {
			require.Equal(t, "failed", res.JSON200.Status)
			statusMessage = res.JSON200.StatusMessage
			require.Contains(t, statusMessage, "serviceaccount \"not-existing-service-account\" not found", fmt.Sprintf("deployment created at %s failed at %s with error %q", dep.CreatedAt, res.JSON200.CompletedAt, statusMessage))
		}
	}, 3*time.Minute, 30*time.Second, "deployment not completed with the expected error message after 3 mins: %s", statusMessage)
}
