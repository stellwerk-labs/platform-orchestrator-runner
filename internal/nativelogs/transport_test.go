package nativelogs

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stretchr/testify/require"
)

func readyFixture(t *testing.T) (Descriptor, []byte, hmessaging.Message) {
	t.Helper()
	a := authorityFixture()
	a.EnvironmentID = "22222222-2222-4222-8222-222222222222"
	a.DeploymentID = "33333333-3333-4333-8333-333333333333"
	key, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	cipher, err := Encode(Envelope{Version: 1, Authority: a, Status: PendingTransfer, Logs: []byte("private log canary")}, key.Recipient())
	require.NoError(t, err)
	d := Descriptor{Version: 1, Authority: a, Status: PendingTransfer, CiphertextSHA256: Hash(cipher), CiphertextBytes: len(cipher)}
	d.ObjectKey = a.ObjectKey(d.CiphertextSHA256)
	payload, err := json.Marshal(d)
	require.NoError(t, err)
	route, err := hmessaging.NativeRunnerToken(a.Target.RunnerUUID)
	require.NoError(t, err)
	event := hmessaging.EventEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1, EventID: d.EventID(), OrganizationID: a.OrganizationID, RunnerID: route, DeploymentID: a.DeploymentID, Type: ReadyEventType, CreatedAt: time.Date(2026, 11, 21, 1, 0, 0, 0, time.UTC), Payload: payload}
	data, err := json.Marshal(event)
	require.NoError(t, err)
	subject, err := hmessaging.RunnerEventSubject(a.OrganizationID, route, event.Type)
	require.NoError(t, err)
	id, err := hmessaging.NativeMessageID(a.OrganizationID, route, "events", event.Type, event.EventID)
	require.NoError(t, err)
	return d, cipher, hmessaging.Message{ID: id, Subject: subject, Data: data, CreatedAt: event.CreatedAt}
}

func TestReadyRequiresExactBoundedDescriptorAndEnvelope(t *testing.T) {
	d, cipher, msg := readyFixture(t)
	actual, err := VerifyReadyMessage(msg)
	require.NoError(t, err)
	require.Equal(t, d, actual)
	require.NoError(t, actual.VerifyCiphertext(cipher))
	require.NotContains(t, string(msg.Data), "private log canary")
	for _, pair := range [][2]string{{`"authority":`, `"status authority":`}, {`"authority":`, `"Authority":`}, {`"target":`, `"Target":`}, {`"runner_uuid":`, `"Runner_UUID":`}, {`"phase":`, `"phase":"apply","phase":`}, {`"created_at":`, `"Created_At":`}, {`"status":`, `"plaintext":"secret","status":`}, {`"ciphertext_bytes":`, `"ciphertext_bytes":null,"unused":`}} {
		candidate := msg
		candidate.Data = bytes.Replace(msg.Data, []byte(pair[0]), []byte(pair[1]), 1)
		_, err := VerifyReadyMessage(candidate)
		require.Error(t, err, string(candidate.Data))
	}
	for _, data := range [][]byte{append(bytes.Clone(msg.Data), []byte(` {}`)...), append(bytes.Clone(msg.Data), bytes.Repeat([]byte(" "), ReadyEventLimit)...)} {
		candidate := msg
		candidate.Data = data
		_, err := VerifyReadyMessage(candidate)
		require.Error(t, err)
	}
	for _, change := range []func(*hmessaging.Message){func(m *hmessaging.Message) { m.ID = "other" }, func(m *hmessaging.Message) { m.Subject += ".other" }, func(m *hmessaging.Message) { m.CreatedAt = m.CreatedAt.Add(time.Second) }, func(m *hmessaging.Message) { m.ExpiresAt = m.CreatedAt.Add(time.Hour) }} {
		candidate := msg
		change(&candidate)
		_, err := VerifyReadyMessage(candidate)
		require.Error(t, err)
	}
}
