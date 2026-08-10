package integrationtests

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/golib/hnats"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	kindE2EOrganizationID = "nats-e2e-org"
	kindE2ERunnerID       = "nats-e2e-runner"
	kindE2ENamespace      = "po-nats-runner-e2e"
	kindE2EBundleBucket   = "PO_RUNNER_BUNDLES"
	kindE2ELogsBucket     = "PO_RUNNER_LOGS"
)

// TestKindNATSRunnerDeploysKubernetesResource is intentionally opt-in. It is a
// destructive integration test within one dedicated namespace on an explicitly
// named Kind context, never a generic current-context test.
func TestKindNATSRunnerDeploysKubernetesResource(t *testing.T) {
	if os.Getenv("PO_NATS_KIND_E2E") != "1" {
		t.Skip("set PO_NATS_KIND_E2E=1 to run the isolated Kind/NATS deployment test")
	}
	contextName := os.Getenv("PO_NATS_KIND_CONTEXT")
	if !strings.HasPrefix(contextName, "kind-") {
		t.Fatalf("PO_NATS_KIND_CONTEXT must name an explicit Kind context, got %q", contextName)
	}
	runnerImage := requiredEnvironment(t, "PO_NATS_KIND_RUNNER_IMAGE")
	natsURL := requiredEnvironment(t, "PO_NATS_KIND_NATS_URL")
	inClusterNATSURL := requiredEnvironment(t, "PO_NATS_KIND_NATS_IN_CLUSTER_URL")
	kubeconfigPath := requiredEnvironment(t, "PO_NATS_KIND_KUBECONFIG")

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	kube := kindClient(t, kubeconfigPath, contextName)
	recreateKindE2ENamespace(t, ctx, kube)
	t.Cleanup(func() {
		if t.Failed() {
			dumpKindE2EPodLogs(t, kube)
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = kube.CoreV1().Namespaces().Delete(cleanupCtx, kindE2ENamespace, metav1.DeleteOptions{})
	})

	installKindE2ERBAC(t, ctx, kube)

	connection, err := hnats.Connect(hnats.ConnectionConfig{
		URLs: []string{natsURL}, Name: "platform-orchestrator-kind-e2e",
		ConnectTimeout: 10 * time.Second, ReconnectWait: time.Second, MaxReconnects: -1,
	}, nil)
	require.NoError(t, err)
	defer connection.Close()
	js, err := connection.JetStream()
	require.NoError(t, err)
	modern, err := hnats.NewJetStream(connection)
	require.NoError(t, err)
	require.NoError(t, hnats.EnsureStandardStreams(ctx, modern, 1))
	ensureKindE2EObjectStore(t, js, kindE2EBundleBucket)
	ensureKindE2EObjectStore(t, js, kindE2ELogsBucket)
	installRemoteRunner(t, ctx, kube, runnerImage, inClusterNATSURL)

	deploymentID := uuid.New()
	environmentID := uuid.New()
	proofName := "nats-runner-proof-" + strings.Split(deploymentID.String(), "-")[0]
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	bundle := kindE2EBundle(t, proofName)
	bundles, err := js.ObjectStore(kindE2EBundleBucket)
	require.NoError(t, err)
	bundleKey := kindE2EOrganizationID + "/" + deploymentID.String()
	_, err = bundles.PutBytes(bundleKey, bundle)
	require.NoError(t, err)

	eventSubject := fmt.Sprintf("po.v1.orgs.%s.runners.%s.events.>", kindE2EOrganizationID, kindE2ERunnerID)
	eventConsumer, err := js.PullSubscribe(eventSubject, "kind-e2e-"+strings.ReplaceAll(deploymentID.String(), "-", ""),
		nats.BindStream(hmessaging.RunnerEventsStreamName), nats.ManualAck(), nats.AckExplicit())
	require.NoError(t, err)

	publishKindE2ECommand(t, ctx, connection, runnerImage, deploymentID, environmentID, bundleKey, proofName, identity.Recipient().String())

	require.Eventually(t, func() bool {
		_, getErr := kube.CoreV1().ConfigMaps(kindE2ENamespace).Get(ctx, proofName, metav1.GetOptions{})
		return getErr == nil
	}, 6*time.Minute, 2*time.Second, "runner Job did not deploy the proof ConfigMap")

	events := collectKindE2EEvents(t, ctx, eventConsumer, deploymentID.String())
	result, found := events["deployment-result"]
	require.True(t, found, "deployment-result event was not received")
	var resultPayload struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(result.Payload, &resultPayload))
	require.Equal(t, "success", resultPayload.Status)
	_, found = events["log-object-ready"]
	require.True(t, found, "log-object-ready event was not received")

	logs, err := js.ObjectStore(kindE2ELogsBucket)
	require.NoError(t, err)
	encryptedLogs, err := logs.GetBytes(environmentID.String() + "/" + deploymentID.String())
	require.NoError(t, err)
	agePayload, err := base64.StdEncoding.DecodeString(string(encryptedLogs))
	require.NoError(t, err)
	reader, err := age.Decrypt(bytes.NewReader(agePayload), identity)
	require.NoError(t, err)
	plainLogs, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Contains(t, string(plainLogs), "Apply complete")
}

func ensureKindE2EObjectStore(t *testing.T, js nats.JetStreamContext, bucket string) {
	t.Helper()
	if _, err := js.ObjectStore(bucket); err == nil {
		return
	} else if !errors.Is(err, nats.ErrBucketNotFound) && !errors.Is(err, nats.ErrStreamNotFound) {
		require.NoError(t, err)
	}
	_, err := js.CreateObjectStore(&nats.ObjectStoreConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 1})
	require.NoError(t, err)
}

func requiredEnvironment(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is required when PO_NATS_KIND_E2E=1", name)
	}
	return value
}

func kindClient(t *testing.T, kubeconfigPath, contextName string) kubernetes.Interface {
	t.Helper()
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: contextName}).ClientConfig()
	require.NoError(t, err)
	client, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)
	return client
}

func recreateKindE2ENamespace(t *testing.T, ctx context.Context, kube kubernetes.Interface) {
	t.Helper()
	err := kube.CoreV1().Namespaces().Delete(ctx, kindE2ENamespace, metav1.DeleteOptions{})
	require.True(t, err == nil || apierrors.IsNotFound(err), "delete stale namespace: %v", err)
	require.Eventually(t, func() bool {
		_, getErr := kube.CoreV1().Namespaces().Get(ctx, kindE2ENamespace, metav1.GetOptions{})
		return apierrors.IsNotFound(getErr)
	}, 30*time.Second, 500*time.Millisecond)
	_, err = kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: kindE2ENamespace}}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func installKindE2ERBAC(t *testing.T, ctx context.Context, kube kubernetes.Interface) {
	t.Helper()
	for _, name := range []string{"remote-runner", "deployment-job"} {
		_, err := kube.CoreV1().ServiceAccounts(kindE2ENamespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: kindE2ENamespace}}, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	_, err := kube.RbacV1().Roles(kindE2ENamespace).Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "runner-e2e", Namespace: kindE2ENamespace},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"create", "get", "list", "watch"}},
			{APIGroups: []string{""}, Resources: []string{"pods", "pods/log", "events", "configmaps"}, Verbs: []string{"create", "get", "list", "watch", "update", "patch"}},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = kube.RbacV1().RoleBindings(kindE2ENamespace).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "runner-e2e", Namespace: kindE2ENamespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "runner-e2e"},
		Subjects: []rbacv1.Subject{
			{Kind: "ServiceAccount", Name: "remote-runner", Namespace: kindE2ENamespace},
			{Kind: "ServiceAccount", Name: "deployment-job", Namespace: kindE2ENamespace},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func installRemoteRunner(t *testing.T, ctx context.Context, kube kubernetes.Interface, image, natsURL string) {
	t.Helper()
	replicas := int32(1)
	_, err := kube.AppsV1().Deployments(kindE2ENamespace).Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "remote-runner", Namespace: kindE2ENamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "remote-runner"}},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "remote-runner"}},
				Spec: corev1.PodSpec{
					ServiceAccountName: "remote-runner",
					Containers: []corev1.Container{{
						Name: "runner", Image: image, ImagePullPolicy: corev1.PullNever, Args: []string{"remote"},
						Env: []corev1.EnvVar{
							{Name: "ORG_ID", Value: kindE2EOrganizationID}, {Name: "RUNNER_ID", Value: kindE2ERunnerID},
							{Name: "NATS_URL", Value: natsURL}, {Name: "NATS_OUTBOX_DIR", Value: "/tmp/outbox"},
						},
						ReadinessProbe: &corev1.Probe{InitialDelaySeconds: 1, PeriodSeconds: 1, ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", "test -d /proc/1"}}}},
					}},
				},
			},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		deployment, getErr := kube.AppsV1().Deployments(kindE2ENamespace).Get(ctx, "remote-runner", metav1.GetOptions{})
		return getErr == nil && deployment.Status.ReadyReplicas == 1
	}, 90*time.Second, time.Second)
}

func publishKindE2ECommand(t *testing.T, ctx context.Context, connection *nats.Conn, image string, deploymentID, environmentID uuid.UUID, bundleKey, proofName, logRecipient string) {
	t.Helper()
	zero := int32(0)
	jobSpec := batchv1.JobSpec{
		BackoffLimit: &zero,
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "nats-runner-job"}},
			Spec: corev1.PodSpec{
				ServiceAccountName: "deployment-job", RestartPolicy: corev1.RestartPolicyNever,
				SecurityContext: &corev1.PodSecurityContext{RunAsUser: int64Pointer(65534), RunAsGroup: int64Pointer(65534), FSGroup: int64Pointer(65534)},
				Volumes:         []corev1.Volume{{Name: "tofu", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
				Containers: []corev1.Container{{
					Name: "runner", Image: image, ImagePullPolicy: corev1.PullNever, Args: []string{"standard"},
					VolumeMounts: []corev1.VolumeMount{{Name: "tofu", MountPath: "/opt/runner/tofu"}},
					Env: []corev1.EnvVar{
						{Name: "DEPLOYMENT_ID", Value: deploymentID.String()}, {Name: "DEPLOYMENT_ENV_UUID", Value: environmentID.String()},
						{Name: "MODE", Value: "deploy"}, {Name: "IAC_BACKEND", Value: "opentofu"},
						{Name: "IAC_CODE_DIR", Value: "/opt/runner/tofu"}, {Name: "NATS_BUNDLE_BUCKET", Value: kindE2EBundleBucket},
						{Name: "NATS_BUNDLE_KEY", Value: bundleKey}, {Name: "ENCRYPTING_LOGS_KEY", Value: logRecipient},
					},
				}},
			},
		},
	}
	jobJSON, err := json.Marshal(jobSpec)
	require.NoError(t, err)
	jobMap := map[string]interface{}{}
	require.NoError(t, json.Unmarshal(jobJSON, &jobMap))
	payload, err := json.Marshal(hmessaging.CreateJobCommand{JobID: deploymentID.String(), Namespace: kindE2ENamespace, Configuration: jobMap})
	require.NoError(t, err)
	now := time.Now().UTC()
	command := hmessaging.CommandEnvelope{
		ProtocolVersion: hmessaging.ProtocolVersionV1, CommandID: "kind-e2e:" + deploymentID.String(),
		OrganizationID: kindE2EOrganizationID, RunnerID: kindE2ERunnerID, DeploymentID: deploymentID.String(),
		Type: hmessaging.CommandTypeCreateJob, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute), Payload: payload,
	}
	data, err := json.Marshal(command)
	require.NoError(t, err)
	subject, err := hmessaging.RunnerCommandSubject(kindE2EOrganizationID, kindE2ERunnerID)
	require.NoError(t, err)
	modern, err := hnats.NewJetStream(connection)
	require.NoError(t, err)
	publisher := hnats.NewPublisher(modern, "", nil)
	require.NoError(t, publisher.Publish(ctx, hmessaging.Message{
		ID: command.CommandID, Subject: subject, Data: data, CreatedAt: now, ExpiresAt: command.ExpiresAt,
	}))
	t.Logf("published %s for proof resource %s", command.CommandID, proofName)
}

func collectKindE2EEvents(t *testing.T, ctx context.Context, subscription *nats.Subscription, deploymentID string) map[string]hmessaging.EventEnvelope {
	t.Helper()
	events := map[string]hmessaging.EventEnvelope{}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) && (events["deployment-result"].EventID == "" || events["log-object-ready"].EventID == "") {
		messages, err := subscription.Fetch(1, nats.MaxWait(2*time.Second))
		if err == nats.ErrTimeout {
			continue
		}
		require.NoError(t, err)
		for _, message := range messages {
			var event hmessaging.EventEnvelope
			require.NoError(t, json.Unmarshal(message.Data, &event))
			require.NoError(t, message.Ack())
			if event.DeploymentID == deploymentID {
				events[event.Type] = event
			}
		}
		if ctx.Err() != nil {
			t.Fatalf("event collection context ended: %v", ctx.Err())
		}
	}
	return events
}

func kindE2EBundle(t *testing.T, proofName string) []byte {
	t.Helper()
	content := fmt.Sprintf(`terraform {
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "2.38.0"
    }
  }
}

provider "kubernetes" {
  host                   = "https://kubernetes.default.svc"
  token                  = file("/var/run/secrets/kubernetes.io/serviceaccount/token")
  cluster_ca_certificate = file("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
}

resource "kubernetes_config_map_v1" "proof" {
  metadata {
    name      = %q
    namespace = %q
  }
  data = { transport = "nats-jetstream" }
}

output "platform_orchestrator_metadata" {
  value = {
    proof = {
      name      = kubernetes_config_map_v1.proof.metadata[0].name
      namespace = kubernetes_config_map_v1.proof.metadata[0].namespace
    }
  }
}
`, proofName, kindE2ENamespace)
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	tarWriter := tar.NewWriter(gzipWriter)
	require.NoError(t, tarWriter.WriteHeader(&tar.Header{Name: "main.tf", Mode: 0o600, Size: int64(len(content)), ModTime: time.Now()}))
	_, err := tarWriter.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, tarWriter.Close())
	require.NoError(t, gzipWriter.Close())
	return compressed.Bytes()
}

func dumpKindE2EPodLogs(t *testing.T, kube kubernetes.Interface) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pods, err := kube.CoreV1().Pods(kindE2ENamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Logf("list E2E pods after failure: %v", err)
		return
	}
	for _, pod := range pods.Items {
		data, logErr := kube.CoreV1().Pods(kindE2ENamespace).GetLogs(pod.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
		if logErr != nil {
			t.Logf("read logs for %s: %v", pod.Name, logErr)
			continue
		}
		t.Logf("logs for %s:\n%s", pod.Name, data)
	}
}

func int64Pointer(value int64) *int64 { return &value }
