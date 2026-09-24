package integrationtests

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-runner/integration-tests/clients/platformorchestratorcp"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/ref"

	"filippo.io/age"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	id     = getAgeIdentity()
	logsId = getAgeIdentity()
)

func TestK8sRunner_Success(t *testing.T) {
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

	t.Logf("using org %s", orgId)

	{
		res, err := cpClient.CreateResourceTypeWithResponse(t.Context(), orgId, platformorchestratorcp.CreateResourceTypeJSONRequestBody{Id: "k8s-namespace", Description: ref.Ref("Kubernetes Namespace"), OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}}}})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}
	{
		res, err := createManagedModuleWithResponse(t, cpClient, orgId, platformorchestratorcp.ModuleCreateBody{Id: "dummy-k8s-namespace", ResourceType: "k8s-namespace",
			ModuleSource: ref.Ref("inline"), ModuleSourceCode: ref.Ref(dummyK8sNamespaceSourceCode), ModuleInputs: map[string]interface{}{"prefix": "${context.project_id}-${context.env_id}", "project": "my-gcp-project"}}, "name")
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}
	{
		res, err := cpClient.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, platformorchestratorcp.RuleCreateBody{ModuleId: "dummy-k8s-namespace"})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}
	{
		res, err := cpClient.CreateResourceTypeWithResponse(t.Context(), orgId, platformorchestratorcp.CreateResourceTypeJSONRequestBody{Id: "thing", OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}})
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	}
	// {
	// 	res, err := createManagedModuleWithResponse(t, cpClient, orgId, platformorchestratorcp.ModuleCreateBody{
	// 		Id: "thing", ResourceType: "thing",
	// 		ModuleSource: ref.Ref("git::ssh://git@github.com/example-org/example-private-module.git"),
	// 	})
	// 	require.NoError(t, err)
	// 	require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	// }
	// {
	// 	res, err := cpClient.CreateModuleRuleInOrgWithResponse(t.Context(), orgId, platformorchestratorcp.RuleCreateBody{ModuleId: "thing"})
	// 	require.NoError(t, err)
	// 	require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	// }

	poDpClient := MustDataPlaneClient(t)
	var dep platformorchestratorapi.Deployment

	t.Run("tofu plan", func(t *testing.T) {
		logsKey, _ := age.GenerateX25519Identity()
		{
			res, err := poDpClient.CreateDeploymentWithResponse(ctx, orgId, &platformorchestratorapi.CreateDeploymentParams{}, platformorchestratorapi.CreateDeploymentJSONRequestBody{
				ProjectId:              projectId,
				EnvId:                  envId,
				Mode:                   platformorchestratorapi.PlanOnly,
				EncryptedLogsRecipient: ref.Ref(logsKey.Recipient().String()),
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
			var statusMessage string
			var completedAt *time.Time
			if assert.NoError(c, dbConn.QueryRowContext(t.Context(), "SELECT outputs, status, status_message, completed_at FROM deployments WHERE id = $1", dep.Id).Scan(&outputs, &status, &statusMessage, &completedAt)) {
				if assert.NotEmpty(c, completedAt) {
					if assert.Equal(c, "succeeded", status, statusMessage) {
						require.NotEmpty(c, outputs)
					} else {
						MustPrintLogs(t, poDpClient, dep.OrgId, dep.Id, logsKey)
					}
				}

			}
		}, 30*time.Second, 2*time.Second, "deployment results not updated after 30s")
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

		var outputs []byte
		// check state and outputs have been updated
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			var status string
			var completedAt *time.Time
			var tfResourcesB []byte
			if assert.NoError(c, dbConn.QueryRowContext(t.Context(), "SELECT outputs, status, completed_at, metrics FROM deployments WHERE id = $1", dep.Id).Scan(&outputs, &status, &completedAt, &tfResourcesB)) {
				if assert.NotEmpty(c, completedAt) {
					require.Equal(t, "succeeded", status)
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
		}, 30*time.Second, 2*time.Second, "deployment results not updated after 30s")
		decryptedOutputs := decryptBytes(t, outputs, &id)
		match := regexp.MustCompile(`"NAMESPACE":\s*"([^"]*)"`).FindStringSubmatch(decryptedOutputs)
		var outputsMap map[string]interface{}
		if assert.Greater(t, len(match), 1) && assert.NoError(t, json.Unmarshal([]byte(decryptedOutputs), &outputsMap)) {
			require.Equal(t, map[string]interface{}{"NAMESPACE": match[1]}, outputsMap["sample"])
		}

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
