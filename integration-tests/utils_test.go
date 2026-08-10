package integrationtests

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/stellwerk-labs/platform-orchestrator-runner/integration-tests/clients/platformorchestratorcp"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/ref"
)

const dummyK8sNamespaceSourceCode = `
variable "prefix" {
  type    = string
  default = "ns"
}

variable "project" {
  type    = string
  default = "test-project"
}

resource "random_string" "dummy" {
  length  = 8
  upper   = false
  special = false
}

output "name" {
  value = "${var.prefix}-${random_string.dummy.id}"
}

output "secret_name" {
  value     = "secret-${var.prefix}-${random_string.dummy.id}"
  sensitive = true
}

output "platform_orchestrator_metadata" {
  value = {
    "project" = var.project
  }
}
`

var testHttpClient = &http.Client{
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			if strings.HasSuffix(host, ".localhost") {
				address = net.JoinHostPort("127.0.0.1", port)
			}
			dialer := &net.Dialer{
				Timeout: 30 * time.Second,
			}
			return dialer.DialContext(ctx, network, address)
		},
	},
}

func MustDataPlaneClient(t *testing.T) platformorchestratorapi.ClientWithResponsesInterface {
	client, err := platformorchestratorapi.NewClientWithResponses(os.Getenv("SERVER_URL"), platformorchestratorapi.WithRequestEditorFn(func(ctx context.Context, req *http.Request) error {
		req.Header.Set("From", "ffffffff-ffff-ffff-ffff-ffffffffffff")
		if strings.HasPrefix(req.URL.Path, "/internal") {
			return fmt.Errorf("path %s is internal - MustInternalDataPlaneClient client required", req.URL.Path)
		}
		return nil
	}), platformorchestratorapi.WithHTTPClient(testHttpClient))
	require.NoError(t, err)
	return client
}

func MustControlPlaneClient(t *testing.T) platformorchestratorcp.ClientWithResponsesInterface {
	client, err := platformorchestratorcp.NewClientWithResponses(os.Getenv("SERVER_URL"), platformorchestratorcp.WithRequestEditorFn(func(ctx context.Context, req *http.Request) error {
		req.Header.Set("From", "ffffffff-ffff-ffff-ffff-ffffffffffff")
		if strings.HasPrefix(req.URL.Path, "/internal") {
			return fmt.Errorf("path %s is internal - MustInternalControlPlaneClient client required", req.URL.Path)
		}
		return nil
	}), platformorchestratorcp.WithHTTPClient(testHttpClient))
	require.NoError(t, err)
	return client
}

func MustInternalControlPlaneClient(t *testing.T) platformorchestratorcp.ClientWithResponsesInterface {
	client, err := platformorchestratorcp.NewClientWithResponses(os.Getenv("INTERNAL_CP_URL"), platformorchestratorcp.WithRequestEditorFn(func(ctx context.Context, req *http.Request) error {
		req.Header.Set("From", "ffffffff-ffff-ffff-ffff-ffffffffffff")
		if !strings.HasPrefix(req.URL.Path, "/internal") {
			return fmt.Errorf("path %s is not internal - MustControlPlaneClient required", req.URL.Path)
		}
		return nil
	}))
	require.NoError(t, err)
	return client
}

// MustDatabaseConn provides access to a raw database connection for the integration test.
func MustDatabaseConn(t *testing.T) *sql.DB {
	db, err := sql.Open("postgres", os.Getenv("DB_CONNECTION_STRING"))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, db.Close())
	})
	return db
}

func MustCreateOrgId(t *testing.T, cpClient platformorchestratorcp.ClientWithResponsesInterface) string {
	res, err := cpClient.CreateInternalOrganizationWithResponse(t.Context(), platformorchestratorcp.CreateInternalOrganizationJSONRequestBody{Id: fmt.Sprintf("org-%s", strings.ToLower(rand.Text()))})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, res.StatusCode())
	return res.JSON201.Id
}

func MustCreateProject(t *testing.T, cpClient platformorchestratorcp.ClientWithResponsesInterface, orgId string, projectId string) *platformorchestratorcp.Project {
	res, err := cpClient.CreateProjectWithResponse(t.Context(), orgId, platformorchestratorcp.CreateProjectJSONRequestBody{Id: projectId})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, res.StatusCode())
	return res.JSON201
}

func MustCreateEnvType(t *testing.T, cpClient platformorchestratorcp.ClientWithResponsesInterface, orgId string, et string) *platformorchestratorcp.EnvironmentType {
	res, err := cpClient.CreateEnvironmentTypeWithResponse(t.Context(), orgId, platformorchestratorcp.CreateEnvironmentTypeJSONRequestBody{Id: et})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, res.StatusCode())
	return res.JSON201
}

func MustCreateEnv(t *testing.T, cpClient platformorchestratorcp.ClientWithResponsesInterface, orgId, et, projectId, env string) *platformorchestratorcp.Environment {
	res, err := cpClient.CreateEnvironmentWithResponse(t.Context(), orgId, projectId, platformorchestratorcp.CreateEnvironmentJSONRequestBody{EnvTypeId: et, Id: env})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, res.StatusCode())
	return res.JSON201
}

func MustCreateModuleProvider(t *testing.T, cpClient platformorchestratorcp.ClientWithResponsesInterface, orgId, providerId string) *platformorchestratorcp.ModuleProvider {
	res, err := cpClient.CreateModuleProviderWithResponse(t.Context(), orgId, platformorchestratorcp.CreateModuleProviderJSONRequestBody{
		Id:                providerId,
		ProviderType:      "aws",
		Configuration:     map[string]interface{}{},
		Source:            "hashicorp/aws",
		VersionConstraint: ">= 3.0",
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	return res.JSON201
}

func MustCreateK8sRunnerWithRule(t *testing.T, cpClient platformorchestratorcp.ClientWithResponsesInterface, orgId, runnerId, projectId string) *platformorchestratorcp.Runner {
	cfgJson, err := os.ReadFile("runner_config.json")
	require.NoError(t, err)
	var runnerK8sClusterCfg platformorchestratorcp.K8sRunnerK8sCluster
	require.NoError(t, json.Unmarshal(cfgJson, &runnerK8sClusterCfg))
	var runnerCfg = new(platformorchestratorcp.RunnerConfiguration)
	require.NoError(t, runnerCfg.FromK8sRunnerConfiguration(platformorchestratorcp.K8sRunnerConfiguration{
		Cluster: runnerK8sClusterCfg,
		Job: platformorchestratorcp.K8sRunnerJobConfig{
			Namespace:      "platform-orchestrator-runner",
			ServiceAccount: "platform-orchestrator-runner",
			PodTemplate: &map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []map[string]interface{}{
						{
							"name": "main",
							"env": []interface{}{
								map[string]interface{}{
									"name": "RUNNER_GIT_SSH_KEYS",
									"valueFrom": map[string]interface{}{
										"secretKeyRef": map[string]interface{}{
											"name": "runner-git-ssh-keys",
											"key":  "key",
										},
									},
								},
								map[string]interface{}{
									"name": "AWS_ACCESS_KEY_ID",
									"valueFrom": map[string]interface{}{
										"secretKeyRef": map[string]interface{}{
											"name": "aws-creds",
											"key":  "access_key_id",
										},
									},
								},
								map[string]interface{}{
									"name": "AWS_SECRET_ACCESS_KEY",
									"valueFrom": map[string]interface{}{
										"secretKeyRef": map[string]interface{}{
											"name": "aws-creds",
											"key":  "secret_access_key",
										},
									},
								},
								map[string]interface{}{
									"name":  "AWS_REGION",
									"value": "us-east-1",
								},
							},
						},
					},
				},
			},
		},
		Type: platformorchestratorcp.RunnerTypeKubernetes,
	}))
	var ssc = new(platformorchestratorcp.StateStorageConfiguration)
	_ = ssc.FromK8sStorageConfiguration(platformorchestratorcp.K8sStorageConfiguration{
		Namespace: "platform-orchestrator-runner",
	})
	res, err := cpClient.CreateRunnerWithResponse(t.Context(), orgId, platformorchestratorcp.CreateRunnerJSONRequestBody{
		Id:                        runnerId,
		Description:               ref.Ref("default runner"),
		RunnerConfiguration:       *runnerCfg,
		StateStorageConfiguration: *ssc,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	runner := res.JSON201

	_, err = cpClient.CreateRunnerRuleInOrgWithResponse(t.Context(), orgId, platformorchestratorcp.CreateRunnerRuleInOrgJSONRequestBody{
		ProjectId: ref.Ref(projectId),
		RunnerId:  runner.Id,
	})
	require.NoError(t, err)

	return runner
}

// MustCreateK8sRunnerWithIaCBackend creates a K8s runner whose pod template includes
// IAC_BACKEND=iacBackend in the main container's env, then attaches a runner rule
// scoped to projectId.
func MustCreateK8sRunnerWithIaCBackend(t *testing.T, cpClient platformorchestratorcp.ClientWithResponsesInterface, orgId, runnerId, projectId, iacBackend string) *platformorchestratorcp.Runner {
	cfgJson, err := os.ReadFile("runner_config.json")
	require.NoError(t, err)
	var runnerK8sClusterCfg platformorchestratorcp.K8sRunnerK8sCluster
	require.NoError(t, json.Unmarshal(cfgJson, &runnerK8sClusterCfg))
	var runnerCfg = new(platformorchestratorcp.RunnerConfiguration)
	require.NoError(t, runnerCfg.FromK8sRunnerConfiguration(platformorchestratorcp.K8sRunnerConfiguration{
		Cluster: runnerK8sClusterCfg,
		Job: platformorchestratorcp.K8sRunnerJobConfig{
			Namespace:      "platform-orchestrator-runner",
			ServiceAccount: "platform-orchestrator-runner",
			PodTemplate: &map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []map[string]interface{}{
						{
							"name": "main",
							"env": []interface{}{
								map[string]interface{}{
									"name": "RUNNER_GIT_SSH_KEYS",
									"valueFrom": map[string]interface{}{
										"secretKeyRef": map[string]interface{}{
											"name": "runner-git-ssh-keys",
											"key":  "key",
										},
									},
								},
								map[string]interface{}{
									"name": "AWS_ACCESS_KEY_ID",
									"valueFrom": map[string]interface{}{
										"secretKeyRef": map[string]interface{}{
											"name": "aws-creds",
											"key":  "access_key_id",
										},
									},
								},
								map[string]interface{}{
									"name": "AWS_SECRET_ACCESS_KEY",
									"valueFrom": map[string]interface{}{
										"secretKeyRef": map[string]interface{}{
											"name": "aws-creds",
											"key":  "secret_access_key",
										},
									},
								},
								map[string]interface{}{
									"name":  "AWS_REGION",
									"value": "us-east-1",
								},
								map[string]interface{}{
									"name":  "IAC_BACKEND",
									"value": iacBackend,
								},
							},
						},
					},
				},
			},
		},
		Type: platformorchestratorcp.RunnerTypeKubernetes,
	}))
	var ssc = new(platformorchestratorcp.StateStorageConfiguration)
	_ = ssc.FromK8sStorageConfiguration(platformorchestratorcp.K8sStorageConfiguration{
		Namespace: "platform-orchestrator-runner",
	})
	res, err := cpClient.CreateRunnerWithResponse(t.Context(), orgId, platformorchestratorcp.CreateRunnerJSONRequestBody{
		Id:                        runnerId,
		Description:               ref.Ref("runner with iac backend: " + iacBackend),
		RunnerConfiguration:       *runnerCfg,
		StateStorageConfiguration: *ssc,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	runner := res.JSON201

	_, err = cpClient.CreateRunnerRuleInOrgWithResponse(t.Context(), orgId, platformorchestratorcp.CreateRunnerRuleInOrgJSONRequestBody{
		ProjectId: ref.Ref(projectId),
		RunnerId:  runner.Id,
	})
	require.NoError(t, err)

	return runner
}

func MustCreateRemoteRunnerWithRule(t *testing.T, cpClient platformorchestratorcp.ClientWithResponsesInterface, orgId, runnerId, projectId, namespace, serviceaccount string) *platformorchestratorcp.Runner {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	publicKeyDER, err := x509.MarshalPKIXPublicKey(publicKey)
	require.NoError(t, err)
	publicKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicKeyDER})
	require.NotEmpty(t, publicKeyPEM)
	cfg := new(platformorchestratorcp.RunnerConfiguration)
	require.NoError(t, cfg.FromK8sAgentRunnerConfiguration(platformorchestratorcp.K8sAgentRunnerConfiguration{
		Job: platformorchestratorcp.K8sRunnerJobConfig{
			Namespace:      namespace,
			ServiceAccount: serviceaccount,
		},
		Key:  string(publicKeyPEM),
		Type: platformorchestratorcp.RunnerTypeKubernetesAgent,
	}))
	var ssc = new(platformorchestratorcp.StateStorageConfiguration)
	_ = ssc.FromK8sStorageConfiguration(platformorchestratorcp.K8sStorageConfiguration{
		Namespace: "platform-orchestrator-runner",
	})
	res, err := cpClient.CreateRunnerWithResponse(t.Context(), orgId, platformorchestratorcp.CreateRunnerJSONRequestBody{
		Id:                        runnerId,
		Description:               ref.Ref("default runner"),
		RunnerConfiguration:       *cfg,
		StateStorageConfiguration: *ssc,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, res.StatusCode(), string(res.Body))
	runner := res.JSON201

	_, err = cpClient.CreateRunnerRuleInOrgWithResponse(t.Context(), orgId, platformorchestratorcp.CreateRunnerRuleInOrgJSONRequestBody{
		ProjectId: ref.Ref(projectId),
		RunnerId:  runner.Id,
	})
	require.NoError(t, err)

	return runner
}

type K8sClient struct {
	client          *dynamic.DynamicClient
	discoveryMapper *restmapper.DeferredDiscoveryRESTMapper
}

func MustDeployRemoteRunner(t *testing.T, orgId string) {
	templateFilePath := "./runner-cluster/remote_runner.yaml"
	manifestBytes, err := os.ReadFile(templateFilePath)
	require.NoError(t, err)
	modifiedManifest := strings.ReplaceAll(string(manifestBytes), "${ORG_ID}", orgId)

	config, err := clientcmd.BuildConfigFromFlags("", "./runner-cluster/kubeconfig.yaml")
	require.NoError(t, err)

	client, err := dynamic.NewForConfig(config)
	require.NoError(t, err)

	clientset, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)

	discoveryClient := memory.NewMemCacheClient(clientset.Discovery())
	discoveryMapper := restmapper.NewDeferredDiscoveryRESTMapper(discoveryClient)

	require.NoError(t, (&K8sClient{
		client:          client,
		discoveryMapper: discoveryMapper,
	}).Apply(t, bytes.NewBufferString(modifiedManifest)))
}

func (k *K8sClient) Apply(t *testing.T, r io.Reader) error {
	dec := yaml.NewDecoder(r)
	for {
		obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
		err := dec.Decode(obj.Object)
		if errors.Is(err, io.EOF) {
			break
		} else {
			require.NoError(t, err, "failed to decode YAML")
		}

		require.NotNil(t, obj.Object, "decoded object should not be nil")
		gvk := obj.GroupVersionKind()
		restMapping, err := k.discoveryMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		require.NoError(t, err, "failed to get REST mapping for %s", gvk)
		gvr := restMapping.Resource
		namespace := obj.GetNamespace()

		_ = k.client.Resource(gvr).Namespace(namespace).Delete(t.Context(), obj.GetName(), metav1.DeleteOptions{})

		_, err = k.client.Resource(gvr).Namespace(namespace).Apply(t.Context(), obj.GetName(), obj, metav1.ApplyOptions{FieldManager: "kube-apply", Force: true})
		require.NoError(t, err, "failed to apply object %s", obj.GetName())
	}
	return nil
}

type s3BucketHandle struct {
	client *s3.Client
	bucket string
}

type s3ObjectHandle struct {
	handle *s3BucketHandle
	key    string
}

func MustCreateS3BucketHandle(t *testing.T) *s3BucketHandle {
	cfg, err := awsconfig.LoadDefaultConfig(
		context.Background(),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "testtest", "")),
		awsconfig.WithRegion("us-east-1"),
	)
	require.NoError(t, err)
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("http://localhost:8333")
		o.UsePathStyle = true
	})
	return &s3BucketHandle{client: client, bucket: "runner-logs"}
}

func (h *s3BucketHandle) Object(key string) *s3ObjectHandle {
	return &s3ObjectHandle{handle: h, key: key}
}

func (o *s3ObjectHandle) NewReader(ctx context.Context) (io.ReadCloser, error) {
	output, err := o.handle.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(o.handle.bucket),
		Key:    aws.String(o.key),
	})
	if err != nil {
		return nil, err
	}
	return output.Body, nil
}

func decryptBytes(t *testing.T, base64Outputs []byte, id age.Identity) string {
	if encryptedOutputs, err := base64.StdEncoding.DecodeString(string(base64Outputs)); assert.NoError(t, err) {
		if r, err := age.Decrypt(bytes.NewReader(encryptedOutputs), id); assert.NoError(t, err) {
			out := &bytes.Buffer{}
			if _, err = io.Copy(out, r); assert.NoError(t, err) {
				return out.String()
			}
		}
	}
	return ""
}

func getAgeIdentity() age.X25519Identity {
	id, _ := age.GenerateX25519Identity()
	return *id
}

func MustPrintLogs(t *testing.T, dpClient platformorchestratorapi.ClientWithResponsesInterface, orgId string, deploymentId uuid.UUID, ageKey *age.X25519Identity) {
	r, err := dpClient.GetDeploymentLogsWithResponse(t.Context(), orgId, deploymentId, &platformorchestratorapi.GetDeploymentLogsParams{DecryptKey: ref.Ref(ageKey.String())})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, r.StatusCode(), string(r.Body))
	t.Log(string(r.Body))
}
