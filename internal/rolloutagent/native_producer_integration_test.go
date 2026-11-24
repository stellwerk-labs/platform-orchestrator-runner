//go:build integration && (linux || darwin)

package rolloutagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gatewayapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/natstransport"
	api "github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestNativeDPProducerExecutesOpenTofuOnKubernetes(t *testing.T) {
	peerBinary := os.Getenv("D2_DP_API_TEST_BINARY")
	require.True(t, filepath.IsAbs(peerBinary), "explicit qualified DP API test binary is required")
	f := newHAProcessFixture(t)
	f.wire.config.Executor.Capabilities = append(f.wire.config.Executor.Capabilities, rolloutexec.ObservationCapability)
	f.config["capabilities"] = f.wire.config.Executor.Capabilities
	namespace, backend := nativeProducerBackend(t, f.root)
	f.config["kubernetes_backend_config_path"] = backend
	recipient, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	artifact, err := os.MkdirTemp(os.Getenv("NATIVE_COMMAND_EVIDENCE_DIRECTORY"), "producer-")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(artifact, "central-public.key"), []byte(base64.StdEncoding.EncodeToString(f.wire.config.PublicKey)), 0600))
	directory := filepath.Join(f.root, "dp-peer")
	require.NoError(t, os.Mkdir(directory, 0700))
	input := map[string]string{"Org": f.wire.config.OrgID, "Runner": f.wire.config.RunnerID, "RunnerUUID": f.runnerUUID, "Environment": uuid.NewString(), "Namespace": namespace, "ToolsDigest": f.wire.config.Executor.CatalogDigest(), "Recipient": recipient.Recipient().String(), "SigningKey": base64.StdEncoding.EncodeToString(f.wire.key), "Directory": directory}
	raw, err := json.Marshal(input)
	require.NoError(t, err)
	inputFile := filepath.Join(directory, "input.json")
	require.NoError(t, os.WriteFile(inputFile, raw, 0600))
	peer := exec.CommandContext(t.Context(), peerBinary, "-test.run=^TestNativeProducerFixturePeer$", "-test.v")
	peer.Env = append(os.Environ(), "D2_NATIVE_PRODUCER_FIXTURE="+inputFile)
	log, err := os.OpenFile(filepath.Join(directory, "process.log"), os.O_CREATE|os.O_WRONLY, 0600)
	require.NoError(t, err)
	peer.Stdout, peer.Stderr = log, log
	require.NoError(t, peer.Start())
	done := make(chan error, 1)
	go func() { done <- peer.Wait(); _ = log.Close() }()
	peerStopped := false
	t.Cleanup(func() {
		if !peerStopped {
			_ = peer.Process.Kill()
			if err := <-done; err != nil {
				t.Log("Native producer peer stopped during cleanup")
			}
		}
	})
	worker := f.start(t, "worker-0", true)
	publisher, err := natstransport.NewPublisher(f.wire.connection)
	require.NoError(t, err)
	route, err := hmessaging.NativeRunnerToken(f.runnerUUID)
	require.NoError(t, err)
	var beforeObservation map[string]string
	var appliesBeforeObservation, plansBeforeObservation int
	for index := range 3 {
		file := filepath.Join(directory, fmt.Sprintf("command-%d.json", index))
		haWaitFile(t, file)
		raw, err = os.ReadFile(file)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(artifact, fmt.Sprintf("command-%d.json", index)), raw, 0600))
		var envelope hmessaging.CommandEnvelope
		require.NoError(t, json.Unmarshal(raw, &envelope))
		var signed rollout.SignedCommand
		require.NoError(t, json.Unmarshal(envelope.Payload, &signed))
		var command rollout.Command
		require.NoError(t, json.Unmarshal(signed.Payload, &command))
		require.Equal(t, "opentofu", command.Request.Tool)
		require.Equal(t, "kubernetes", command.Request.Backend)
		require.NotNil(t, command.NativeTarget)
		require.Empty(t, command.Request.MetadataKey)
		require.Empty(t, command.Request.DeploymentToken)
		if index == 2 {
			require.Equal(t, "observation", command.Strategy)
			require.Equal(t, "observation", command.Request.Mode)
			require.Empty(t, command.Request.EncryptedOutputsRecipient)
			require.Contains(t, command.RequiredCapabilities, "environment-observation/v1")
			require.Empty(t, command.Steps)
			require.Nil(t, command.Recipe)
		}
		subject, err := hmessaging.RunnerCommandSubject(command.OrgID, route)
		require.NoError(t, err)
		id, err := hmessaging.NativeMessageID(command.OrgID, route, "commands", envelope.Type, envelope.CommandID)
		require.NoError(t, err)
		require.NoError(t, publisher.Publish(t.Context(), hmessaging.Message{ID: id, Subject: subject, Data: raw, CreatedAt: envelope.CreatedAt, ExpiresAt: envelope.ExpiresAt}))
		phase := "completed"
		if index == 1 {
			phase = "failed"
		}
		finished := f.progress(t, command.ID, phase)
		events := make([]hmessaging.Message, 0, 2)
		for _, kind := range []string{"rollout-progress", "deployment-result"} {
			eventSubject, err := hmessaging.RunnerEventSubject(command.OrgID, route, kind)
			require.NoError(t, err)
			var message *nats.RawStreamMsg
			require.Eventually(t, func() bool {
				var err error
				message, err = f.wire.js.GetLastMsg(hmessaging.RunnerEventsStreamName, eventSubject)
				if err != nil {
					return false
				}
				var event hmessaging.EventEnvelope
				return json.Unmarshal(message.Data, &event) == nil && event.DeploymentID == command.ID
			}, 15*time.Second, 25*time.Millisecond)
			events = append(events, hmessaging.Message{ID: message.Header.Get(nats.MsgIdHdr), Subject: eventSubject, Data: message.Data, CreatedAt: message.Time})
		}
		var resultEvent hmessaging.EventEnvelope
		require.NoError(t, json.Unmarshal(events[1].Data, &resultEvent))
		var result api.DeploymentResultsUpdateBody
		require.NoError(t, json.Unmarshal(resultEvent.Payload, &result))
		switch index {
		case 0:
			require.NotNil(t, result.Outputs)
			encrypted, err := base64.StdEncoding.DecodeString(*result.Outputs)
			require.NoError(t, err)
			reader, err := age.Decrypt(bytes.NewReader(encrypted), recipient)
			require.NoError(t, err)
			plain, err := io.ReadAll(reader)
			require.NoError(t, err)
			var outputs map[string]any
			require.NoError(t, json.Unmarshal(plain, &outputs))
			require.Contains(t, outputs, "value")
			require.NotContains(t, outputs, "platform_orchestrator_metadata")
			require.Equal(t, 1, f.count(t, "rollout_effects", command.ID))
		case 1:
			require.NotNil(t, result.NoEffectReceipt)
			require.Zero(t, f.count(t, "rollout_effects", command.ID))
			beforeObservation = nativeProducerStateSnapshot(t, backend, namespace)
			appliesBeforeObservation = f.wire.operations(t, "apply")
			plansBeforeObservation = f.wire.operations(t, "plan")
			require.Eventually(t, func() bool {
				for info := range f.wire.js.ConsumersInfo(hmessaging.RunnerCommandsStreamName) {
					if info.Config.FilterSubject == subject && info.NumAckPending == 0 && info.NumPending == 0 {
						return true
					}
				}
				return false
			}, 5*time.Second, 20*time.Millisecond)
			f.wire.lastAck.Store(gatewayapi.ReceiptRequest{})
			f.wire.dropBefore.Store("ack")
		default:
			require.NotNil(t, result.ObservationResult)
			require.Nil(t, result.Outputs)
			require.Empty(t, result.Metadata)
			require.Nil(t, result.NoEffectReceipt)
			require.False(t, finished.InfrastructureApplied)
			require.NotContains(t, string(resultEvent.Payload), "terraform_data.marker")
			require.NotContains(t, string(resultEvent.Payload), "approved")
			for _, table := range []string{"rollout_artifacts", "rollout_plan_logs", "rollout_execution_logs", "rollout_effects"} {
				require.Zero(t, f.count(t, table, command.ID), "observation retained %s", table)
			}
			require.NotEmpty(t, beforeObservation)
			require.Equal(t, beforeObservation, nativeProducerStateSnapshot(t, backend, namespace), "observation rewrote the backend state or its version")
			require.Equal(t, appliesBeforeObservation, f.wire.operations(t, "apply"))
			require.Equal(t, plansBeforeObservation+1, f.wire.operations(t, "plan"))
			work, err := os.ReadDir(filepath.Join(filepath.Dir(worker.log), "work"))
			require.NoError(t, err)
			require.Empty(t, work, "observation retained private execution files")
			published, err := f.wire.js.GetLastMsg(hmessaging.RunnerCommandsStreamName, subject)
			require.NoError(t, err)
			require.Eventually(t, func() bool { return f.wire.lastAck.Load().(gatewayapi.ReceiptRequest).Receipt != "" }, 5*time.Second, 20*time.Millisecond)
			ack := f.wire.lastAck.Load().(gatewayapi.ReceiptRequest)
			worker.kill(t)
			f.wire.lastAck.Store(gatewayapi.ReceiptRequest{})
			f.wire.dropBefore.Store("ack")
			require.NoError(t, f.wire.client.Retry(t.Context(), envelope.CommandID, ack.Receipt, 0))
			worker = f.start(t, "worker-0", true)
			require.Eventually(t, func() bool {
				receipt := f.wire.lastAck.Load().(gatewayapi.ReceiptRequest).Receipt
				return receipt != "" && receipt != ack.Receipt
			}, 10*time.Second, 20*time.Millisecond, "the replacement Runner did not accept the redelivered observation")
			replayAck := f.wire.lastAck.Load().(gatewayapi.ReceiptRequest)
			replayed, err := f.wire.js.GetLastMsg(hmessaging.RunnerCommandsStreamName, subject)
			require.NoError(t, err)
			require.Equal(t, published.Sequence, replayed.Sequence)
			require.Equal(t, published.Data, replayed.Data)
			require.NoError(t, f.wire.client.Acknowledge(t.Context(), envelope.CommandID, replayAck.Receipt))
			require.Eventually(t, func() bool {
				_, err := f.wire.js.GetLastMsg(hmessaging.RunnerCommandsStreamName, subject)
				return errors.Is(err, nats.ErrMsgNotFound)
			}, 5*time.Second, 20*time.Millisecond, "acknowledged work-queue command was retained")
			require.Equal(t, beforeObservation, nativeProducerStateSnapshot(t, backend, namespace))
			require.Equal(t, finished.Revision, f.progress(t, command.ID, "completed").Revision)
			require.Equal(t, appliesBeforeObservation, f.wire.operations(t, "apply"))
			require.Equal(t, plansBeforeObservation+1, f.wire.operations(t, "plan"), "redelivery repeated the observation")
			work, err = os.ReadDir(filepath.Join(filepath.Dir(worker.log), "work"))
			require.True(t, err == nil || os.IsNotExist(err), "read replacement work directory: %v", err)
			require.Empty(t, work, "replay retained private execution files")
			var commands int
			require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM `+f.table("rollout_commands")+` WHERE id=$1`, command.ID).Scan(&commands))
			require.Equal(t, 1, commands)
		}
		data, err := json.Marshal(events)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(directory, fmt.Sprintf("events-%d.json", index)), data, 0600))
		require.NoError(t, os.WriteFile(filepath.Join(artifact, fmt.Sprintf("events-%d.json", index)), data, 0600))
		haWaitFile(t, filepath.Join(directory, fmt.Sprintf("settled-%d", index)))
	}
	require.NoError(t, <-done, "the private DP fixture peer failed; inspect its owned log")
	peerStopped = true
}

func nativeProducerStateSnapshot(t *testing.T, backend, namespace string) map[string]string {
	t.Helper()
	config, err := clientcmd.BuildConfigFromFlags("", backend)
	require.NoError(t, err)
	client, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)
	secrets, err := client.CoreV1().Secrets(namespace).List(t.Context(), metav1.ListOptions{})
	require.NoError(t, err)
	result := make(map[string]string)
	for _, secret := range secrets.Items {
		data, err := json.Marshal(secret.Data)
		require.NoError(t, err)
		result[secret.Name] = fmt.Sprintf("%s:%s:%x", secret.UID, secret.ResourceVersion, sha256.Sum256(data))
	}
	return result
}

// Use only an explicit admin file to create a uniquely owned namespace and a
// namespace-limited state identity. Never read or change the user's context.
func nativeProducerBackend(t *testing.T, directory string) (string, string) {
	t.Helper()
	adminFile := os.Getenv("NATIVE_BACKEND_ADMIN_CONFIG")
	require.NotEmpty(t, adminFile)
	admin, err := clientcmd.BuildConfigFromFlags("", adminFile)
	require.NoError(t, err)
	client, err := kubernetes.NewForConfig(admin)
	require.NoError(t, err)
	run := uuid.NewString()
	name := "native-d2-" + run[:12]
	evidenceRoot := os.Getenv("NATIVE_COMMAND_EVIDENCE_DIRECTORY")
	require.True(t, filepath.IsAbs(evidenceRoot))
	adminBytes, err := os.ReadFile(adminFile)
	require.NoError(t, err)
	intent, err := json.Marshal(map[string]string{"namespace": name, "run_label": run, "explicit_admin_file": adminFile, "admin_file_sha256": fmt.Sprintf("%x", sha256.Sum256(adminBytes))})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(evidenceRoot, "namespace-"+run+"-intent.json"), intent, 0600))
	namespace, err := client.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"stellwerk.test/run": run}}}, metav1.CreateOptions{})
	require.NoError(t, err)
	ownership, err := json.Marshal(namespace)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(evidenceRoot, "namespace-"+run+"-created.json"), ownership, 0600))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		current, err := client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, namespace.UID, current.UID)
		require.Equal(t, run, current.Labels["stellwerk.test/run"])
		require.NoError(t, client.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{}))
		receipt, _ := json.Marshal(map[string]string{"namespace": name, "uid": string(namespace.UID), "run_label": run, "delete": "requested after exact ownership check"})
		require.NoError(t, os.WriteFile(filepath.Join(evidenceRoot, "namespace-"+run+"-cleanup.json"), receipt, 0600))
	})
	_, err = client.CoreV1().ServiceAccounts(name).Create(t.Context(), &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "state"}}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = client.RbacV1().Roles(name).Create(t.Context(), &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "state"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list", "create", "update", "patch", "delete"}}, {APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "list", "create", "update", "patch", "delete"}}}}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = client.RbacV1().RoleBindings(name).Create(t.Context(), &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "state"}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "state"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "state", Namespace: name}}}, metav1.CreateOptions{})
	require.NoError(t, err)
	token, err := client.CoreV1().ServiceAccounts(name).CreateToken(t.Context(), "state", &authenticationv1.TokenRequest{}, metav1.CreateOptions{})
	require.NoError(t, err)
	config := clientcmdapi.Config{CurrentContext: "state", Contexts: map[string]*clientcmdapi.Context{"state": {Cluster: "state", AuthInfo: "state"}}, Clusters: map[string]*clientcmdapi.Cluster{"state": {Server: admin.Host, CertificateAuthorityData: admin.CAData}}, AuthInfos: map[string]*clientcmdapi.AuthInfo{"state": {Token: token.Status.Token}}}
	data, err := clientcmd.Write(config)
	require.NoError(t, err)
	file := filepath.Join(directory, "native-state.conf")
	require.NoError(t, os.WriteFile(file, data, 0600))
	return name, file
}
