package rolloutagent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stretchr/testify/require"
)

func TestNativeLogReadyUsesFrozenProgressTimeAndAttachment(t *testing.T) {
	key, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	a := nativelogs.Authority{OrganizationID: "org", EnvironmentID: "11111111-1111-4111-8111-111111111111", DeploymentID: "22222222-2222-4222-8222-222222222222", CommandDigest: strings.Repeat("a", 64), Target: nativelogs.Target{RunnerID: "site", RunnerUUID: "33333333-3333-4333-8333-333333333333", PipelineVersion: "pipeline/v2", RuntimeVersion: "native/v1", StateBackendType: "local", ToolsDigest: strings.Repeat("b", 64), StateConfigurationDigest: strings.Repeat("c", 64)}, Phase: "apply", ExecutionID: "execution", CaptureRevision: 9}
	cipher, err := nativelogs.Encode(nativelogs.Envelope{Version: 1, Authority: a, Status: nativelogs.PendingTransfer, Logs: []byte("private")}, key.Recipient())
	require.NoError(t, err)
	descriptor := nativelogs.Descriptor{Version: 1, Authority: a, Status: nativelogs.PendingTransfer, CiphertextSHA256: nativelogs.Hash(cipher), CiphertextBytes: len(cipher)}
	descriptor.ObjectKey = a.ObjectKey(descriptor.CiphertextSHA256)
	route, err := hmessaging.NativeRunnerToken(a.Target.RunnerUUID)
	require.NoError(t, err)
	frozenTime := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	progress, err := json.Marshal(hmessaging.EventEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1, OrganizationID: a.OrganizationID, RunnerID: route, DeploymentID: a.DeploymentID, CreatedAt: frozenTime})
	require.NoError(t, err)
	delivery := rollout.EventDelivery{Version: 1, EventID: 42, Progress: progress, Log: &rollout.LogAttachment{Descriptor: descriptor, Ciphertext: cipher}}
	first, err := logReadyBytes(delivery)
	require.NoError(t, err)
	repeated, err := logReadyBytes(delivery)
	require.NoError(t, err)
	require.Equal(t, first, repeated)
	event, actual, err := nativelogs.ParseReady(first)
	require.NoError(t, err)
	require.Equal(t, frozenTime, event.CreatedAt)
	require.Equal(t, descriptor, actual)
	delivery.Log.Ciphertext[0] ^= 1
	_, err = logReadyBytes(delivery)
	require.Error(t, err)
}
