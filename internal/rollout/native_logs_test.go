package rollout

import (
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stretchr/testify/require"
)

func nativeLogFixture(t *testing.T) (*Engine, nativelogs.Authority, *age.X25519Identity) {
	t.Helper()
	site, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	managed, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	engine := &Engine{config: Config{OrgID: "org", RunnerID: "site", RunnerUUID: "11111111-1111-4111-8111-111111111111"}, executor: &rolloutexec.Executor{Identity: site}}
	authority := nativelogs.Authority{OrganizationID: "org", EnvironmentID: "env", DeploymentID: "deployment", CommandDigest: strings.Repeat("a", 64), Target: NativeExecutionTarget{RunnerID: "site", RunnerUUID: engine.config.RunnerUUID, RuntimeVersion: NativeRuntimeVersion, PipelineVersion: NativePipelineVersion, StateBackendType: "local", StateConfigurationDigest: strings.Repeat("b", 64), ToolsDigest: strings.Repeat("c", 64)}, Phase: "plan", ExecutionID: "deployment", CaptureRevision: 1, Subject: strings.Repeat("d", 64)}
	return engine, authority, managed
}

func TestNativeLogPreparationKeepsLocalCaptureOnExportFailure(t *testing.T) {
	engine, authority, managed := nativeLogFixture(t)
	for _, test := range []struct {
		name, recipient, status string
		logs                    []byte
		large                   bool
	}{
		{"managed", managed.Recipient().String(), nativelogs.PendingTransfer, []byte("failed diagnostic"), false},
		{"empty", managed.Recipient().String(), nativelogs.PendingTransfer, []byte{}, false},
		{"absent", "", nativelogs.NotManaged, []byte("site-only"), false},
		{"invalid", "not-an-age-recipient", nativelogs.Unavailable, []byte("keep all diagnostics"), false},
		{"too_large", managed.Recipient().String(), nativelogs.TooLarge, make([]byte, nativelogs.RawLimit+1), false},
		{"executor_too_large", managed.Recipient().String(), nativelogs.TooLarge, nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			local, descriptor, attachment, err := engine.prepareNativeLogs(authority, test.recipient, test.logs, test.large)
			require.NoError(t, err)
			require.Equal(t, test.status, descriptor.Status)
			require.NoError(t, descriptor.Validate())
			decoded, err := nativelogs.Decode(local, engine.executor.Identity, authority)
			require.NoError(t, err)
			if test.status == nativelogs.TooLarge {
				require.Empty(t, decoded.Logs)
			} else {
				require.Equal(t, test.logs, decoded.Logs)
			}
			if test.status == nativelogs.PendingTransfer {
				require.Equal(t, local, attachment)
				require.NoError(t, descriptor.VerifyCiphertext(attachment))
				_, err = nativelogs.Decode(attachment, managed, authority)
				require.NoError(t, err)
			} else {
				require.Empty(t, attachment)
				_, err = nativelogs.Decode(local, managed, authority)
				require.Error(t, err)
			}
		})
	}
}

func TestNativeLogDeliveryHasSeparateLocalDecoderBound(t *testing.T) {
	engine, authority, managed := nativeLogFixture(t)
	logs := make([]byte, nativelogs.RawLimit)
	_, err := rand.Read(logs)
	require.NoError(t, err)
	_, descriptor, ciphertext, err := engine.prepareNativeLogs(authority, managed.Recipient().String(), logs, false)
	require.NoError(t, err)
	event := Event{ID: 1, Progress: Progress{ID: authority.DeploymentID, EnvironmentID: authority.EnvironmentID, Revision: authority.CaptureRevision, LogCapture: &descriptor}}
	body, err := json.Marshal(event)
	require.NoError(t, err)
	progress, err := engine.eventEnvelope(authority.DeploymentID, "capture", "rollout-progress", event.Progress.CreatedAt, body)
	require.NoError(t, err)
	delivery := EventDelivery{Version: 1, EventID: 1, Progress: progress, Log: &LogAttachment{Descriptor: descriptor, Ciphertext: ciphertext}}
	plain, err := json.Marshal(delivery)
	require.NoError(t, err)
	require.Greater(t, len(plain), 4<<20)
	require.LessOrEqual(t, len(plain), nativelogs.DeliveryLimit)
	encrypted, err := engine.encrypt(plain)
	require.NoError(t, err)
	_, err = engine.decrypt(encrypted)
	requireCode(t, err, "artifact_too_large")
	actual, err := engine.readDelivery(1, encrypted)
	require.NoError(t, err)
	require.Equal(t, delivery, actual)
	delivery.Log.Ciphertext[0] ^= 1
	plain, err = json.Marshal(delivery)
	require.NoError(t, err)
	encrypted, err = engine.encrypt(plain)
	require.NoError(t, err)
	_, err = engine.readDelivery(1, encrypted)
	requireCode(t, err, "delivery_unavailable")
	encrypted, err = engine.encrypt(make([]byte, nativelogs.DeliveryLimit+1))
	require.NoError(t, err)
	_, err = engine.readDelivery(1, encrypted)
	requireCode(t, err, "delivery_unavailable")
	// A legacy delivery keeps its original 4 MiB ceiling.
	plain, err = json.Marshal(EventDelivery{Version: 1, EventID: 1, Progress: json.RawMessage(`{"large":"` + strings.Repeat("x", 4<<20) + `"}`)})
	require.NoError(t, err)
	encrypted, err = engine.encrypt(plain)
	require.NoError(t, err)
	_, err = engine.readDelivery(1, encrypted)
	requireCode(t, err, "delivery_unavailable")
}
