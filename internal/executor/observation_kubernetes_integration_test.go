//go:build integration

package executor

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/golib/hnats"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gateway"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gatewayapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/kubernetesjob"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// Real JetStream, HTTP gateway, remote command executor and Kubernetes API.
// Suspended Jobs verify the wire environment without starting or pulling images.
func TestObservationRemoteCommandPreservesSafeWireOnKubernetes(t *testing.T) {
	kubeconfig, contextName, natsURL := os.Getenv("OBSERVATION_KUBE_CONFIG"), os.Getenv("OBSERVATION_KUBE_CONTEXT"), os.Getenv("OBSERVATION_NATS_URL")
	if kubeconfig == "" || contextName == "" || natsURL == "" {
		t.Skip("set explicit OBSERVATION_KUBE_CONFIG, OBSERVATION_KUBE_CONTEXT and OBSERVATION_NATS_URL")
	}
	require.True(t, strings.HasPrefix(contextName, "kind-"), "fixture requires an explicit isolated Kind context")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	rest, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig}, &clientcmd.ConfigOverrides{CurrentContext: contextName}).ClientConfig()
	require.NoError(t, err)
	kube, err := kubernetes.NewForConfig(rest)
	require.NoError(t, err)
	namespace := "obs-wire-" + uuid.NewString()[:8]
	_, err = kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{})
	require.NoError(t, err)
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		require.NoError(t, kube.CoreV1().Namespaces().Delete(cleanup, namespace, metav1.DeleteOptions{}))
	}()
	connection, err := hnats.Connect(hnats.ConnectionConfig{URLs: []string{natsURL}, Name: "observation-wire-fixture", ConnectTimeout: 5 * time.Second}, nil)
	require.NoError(t, err)
	defer connection.Close()
	modern, err := hnats.NewJetStream(connection)
	require.NoError(t, err)
	require.NoError(t, hnats.EnsureStandardStreams(ctx, modern, 1))
	js, err := connection.JetStream()
	require.NoError(t, err)
	for _, bucket := range []string{"PO_RUNNER_BUNDLES", "PO_RUNNER_LOGS"} {
		if _, err := js.ObjectStore(bucket); err != nil {
			_, err = js.CreateObjectStore(&nats.ObjectStoreConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 1})
			require.NoError(t, err)
		}
	}
	backend, err := gateway.NewNATSBackend(connection)
	require.NoError(t, err)
	org, runnerID := namespace, "runner"
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	publicDER, err := x509.MarshalPKIXPublicKey(public)
	require.NoError(t, err)
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	require.NoError(t, err)
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	server, err := gateway.NewServer(gateway.ServerConfig{Backend: backend, PublicKeys: gateway.StaticPublicKeyResolver{OrganizationID: org, RunnerID: runnerID, PublicKey: publicPEM}, ReceiptKey: make([]byte, 32), RunnerTokenSalt: "isolated-observation-wire-fixture", FetchWait: 3 * time.Second})
	require.NoError(t, err)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client, err := gatewayapi.NewClient(gatewayapi.ClientConfig{BaseURL: httpServer.URL, OrganizationID: org, RunnerID: runnerID, PrivateKey: privatePEM})
	require.NoError(t, err)
	cfg := &config.RemoteModeConfiguration{OrgID: org, RunnerId: runnerID, PrivateKey: string(privatePEM), Gateway: config.GatewayClientConfiguration{URL: httpServer.URL}}
	for _, protocol := range []string{config.ObservationProtocolV1, ""} {
		jobID := uuid.NewString()
		suspend := true
		env := []corev1.EnvVar{{Name: "MODE", Value: "plan_only"}}
		if protocol != "" {
			env = append(env, corev1.EnvVar{Name: "OBSERVATION_PROTOCOL", Value: protocol})
		}
		spec := batchv1.JobSpec{Suspend: &suspend, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "runner", Image: "observation-fixture:never-pull", ImagePullPolicy: corev1.PullNever, Env: env}}}}}
		encoded, err := json.Marshal(spec)
		require.NoError(t, err)
		var configuration map[string]interface{}
		require.NoError(t, json.Unmarshal(encoded, &configuration))
		payload, err := json.Marshal(hmessaging.CreateJobCommand{JobID: jobID, Namespace: namespace, Configuration: configuration})
		require.NoError(t, err)
		now := time.Now().UTC()
		envelope := hmessaging.CommandEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1, CommandID: "observation-wire:" + jobID, OrganizationID: org, RunnerID: runnerID, DeploymentID: jobID, Type: hmessaging.CommandTypeCreateJob, CreatedAt: now, ExpiresAt: now.Add(time.Minute), Payload: payload}
		data, err := json.Marshal(envelope)
		require.NoError(t, err)
		subject, err := hmessaging.RunnerCommandSubject(org, runnerID)
		require.NoError(t, err)
		require.NoError(t, hnats.NewPublisher(modern, "", nil).Publish(ctx, hmessaging.Message{ID: envelope.CommandID, Subject: subject, Data: data, CreatedAt: now, ExpiresAt: envelope.ExpiresAt}))
		delivery, err := client.NextCommand(ctx)
		require.NoError(t, err)
		require.NoError(t, executeCommand(ctx, cfg, client, kubernetesjob.NewK8sJobsClient(kube), delivery))
		job, err := kube.BatchV1().Jobs(namespace).Get(ctx, jobID, metav1.GetOptions{})
		require.NoError(t, err)
		actual := map[string]string{}
		for _, entry := range job.Spec.Template.Spec.Containers[0].Env {
			actual[entry.Name] = entry.Value
		}
		require.Equal(t, "plan_only", actual["MODE"])
		require.Equal(t, protocol, actual["OBSERVATION_PROTOCOL"])
		if protocol == "" {
			_, present := actual["OBSERVATION_PROTOCOL"]
			require.False(t, present)
		}
		require.Equal(t, httpServer.URL, actual["RUNNER_GATEWAY_URL"])
	}
}
