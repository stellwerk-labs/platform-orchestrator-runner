package diode

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
	"github.com/stretchr/testify/require"
)

func nativeLogCarrierFixture(t *testing.T) (hmessaging.Message, *Attachment, ed25519.PrivateKey) {
	t.Helper()
	a := nativelogs.Authority{OrganizationID: "carrier-" + uuid.NewString(), EnvironmentID: uuid.NewString(), DeploymentID: uuid.NewString(), CommandDigest: strings.Repeat("a", 64), Target: nativelogs.Target{RunnerID: "site", RunnerUUID: uuid.NewString(), PipelineVersion: "pipeline/v2", RuntimeVersion: "native/v1", StateBackendType: "local", ToolsDigest: strings.Repeat("b", 64), StateConfigurationDigest: strings.Repeat("c", 64)}, Phase: "plan", ExecutionID: "execution", CaptureRevision: 1}
	key, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	cipher, err := nativelogs.Encode(nativelogs.Envelope{Version: 1, Authority: a, Status: nativelogs.PendingTransfer, Logs: []byte("private log canary")}, key.Recipient())
	require.NoError(t, err)
	d := nativelogs.Descriptor{Version: 1, Authority: a, Status: nativelogs.PendingTransfer, CiphertextSHA256: nativelogs.Hash(cipher), CiphertextBytes: len(cipher)}
	d.ObjectKey = a.ObjectKey(d.CiphertextSHA256)
	payload, err := json.Marshal(d)
	require.NoError(t, err)
	route, err := hmessaging.NativeRunnerToken(a.Target.RunnerUUID)
	require.NoError(t, err)
	created := time.Now().UTC()
	event := hmessaging.EventEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1, EventID: d.EventID(), OrganizationID: a.OrganizationID, RunnerID: route, DeploymentID: a.DeploymentID, Type: nativelogs.ReadyEventType, CreatedAt: created, Payload: payload}
	wire, err := json.Marshal(event)
	require.NoError(t, err)
	subject, err := hmessaging.RunnerEventSubject(a.OrganizationID, route, event.Type)
	require.NoError(t, err)
	id, err := hmessaging.NativeMessageID(a.OrganizationID, route, "events", event.Type, event.EventID)
	require.NoError(t, err)
	_, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return hmessaging.Message{ID: id, Subject: subject, Data: wire, CreatedAt: created}, &Attachment{Bucket: nativelogs.Bucket, Key: d.ObjectKey, Data: cipher, PayloadSHA256: d.CiphertextSHA256}, private
}

func TestNativeSignedAttachmentRequiresExactLogAuthority(t *testing.T) {
	msg, attachment, private := nativeLogCarrierFixture(t)
	frame, err := signFrameWithAttachment(1, msg, attachment, private)
	require.NoError(t, err)
	require.NoError(t, verifyFrame(frame, private.Public().(ed25519.PublicKey)))
	require.NoError(t, validateAttachmentInvariant("log", msg, attachment))
	for _, direction := range []string{"", "bundle"} {
		require.Error(t, validateAttachmentInvariant(direction, msg, attachment))
	}
	for _, change := range []func(*hmessaging.Message, *Attachment){
		func(m *hmessaging.Message, a *Attachment) { m.ID = "foreign" }, func(m *hmessaging.Message, a *Attachment) { m.Subject = "foreign" },
		func(m *hmessaging.Message, a *Attachment) { a.Bucket = "foreign" }, func(m *hmessaging.Message, a *Attachment) { a.Key = "foreign" },
		func(m *hmessaging.Message, a *Attachment) { a.Data = []byte("wrong") },
		func(m *hmessaging.Message, a *Attachment) {
			var e hmessaging.EventEnvelope
			require.NoError(t, json.Unmarshal(m.Data, &e))
			e.Type = "rollout-progress"
			m.Data, _ = json.Marshal(e)
		},
		func(m *hmessaging.Message, a *Attachment) {
			var e hmessaging.EventEnvelope
			require.NoError(t, json.Unmarshal(m.Data, &e))
			e.OrganizationID = "foreign"
			m.Data, _ = json.Marshal(e)
		},
	} {
		candidate := msg
		copyAttachment := *attachment
		change(&candidate, &copyAttachment)
		copyAttachment.PayloadSHA256 = payloadHash(copyAttachment.Data)
		signed, err := signFrameWithAttachment(2, candidate, &copyAttachment, private)
		require.NoError(t, err)
		require.NoError(t, verifyFrame(signed, private.Public().(ed25519.PublicKey)), "a trusted signature alone is insufficient")
		require.Error(t, validateAttachmentInvariant("log", candidate, &copyAttachment))
	}
	require.Error(t, validateAttachmentInvariant("log", msg, nil))
}
