package diode

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
	api "github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/signedcontrol"
	"github.com/stretchr/testify/require"
)

func nativeReverseMessage(t *testing.T, kind string) (hmessaging.Message, ImportConfig) {
	t.Helper()
	if kind == nativelogs.ReadyEventType {
		message, _, _ := nativeLogCarrierFixture(t)
		var event hmessaging.EventEnvelope
		require.NoError(t, json.Unmarshal(message.Data, &event))
		return message, ImportConfig{AuthoritativeOrg: event.OrganizationID, NativeRunnerUUID: strings.TrimPrefix(event.RunnerID, hmessaging.NativeRunnerTokenPrefix), ExpectedAttachment: "log"}
	}
	config := ImportConfig{AuthoritativeOrg: "site-org", NativeRunnerUUID: uuid.NewString(), ExpectedAttachment: "log"}
	route, err := hmessaging.NativeRunnerToken(config.NativeRunnerUUID)
	require.NoError(t, err)
	event := hmessaging.EventEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1, OrganizationID: config.AuthoritativeOrg, RunnerID: route, DeploymentID: uuid.NewString(), Type: kind, CreatedAt: time.Now().UTC(), Payload: json.RawMessage(`{}`)}
	switch kind {
	case "rollout-progress":
		event.EventID = event.DeploymentID + ":1"
		event.Payload = json.RawMessage(`{"progress":{"id":"` + event.DeploymentID + `","revision":1}}`)
	case "deployment-result":
		event.EventID = event.DeploymentID + ":deployment-result"
	case "rollout-control-result":
		result := signedcontrol.Result{Version: signedcontrol.ResultVersion, ControlID: uuid.NewString(), RolloutID: event.DeploymentID, RequestDigest: strings.Repeat("a", 64), ExecutionDigest: strings.Repeat("b", 64), Outcome: "applied", Phase: "cancelled", Revision: 1}
		event.CommandID = "pipeline-control:" + result.ControlID
		event.EventID = event.CommandID + ":result"
		event.Payload, err = json.Marshal(result)
		require.NoError(t, err)
	}
	data, err := json.MarshalIndent(event, "", " ")
	require.NoError(t, err)
	subject, err := hmessaging.RunnerEventSubject(config.AuthoritativeOrg, route, kind)
	require.NoError(t, err)
	id, err := hmessaging.NativeMessageID(config.AuthoritativeOrg, route, "events", kind, event.EventID)
	require.NoError(t, err)
	return hmessaging.Message{ID: id, Subject: subject, Data: data, CreatedAt: event.CreatedAt}, config
}

func TestNativeReverseAcceptedTypesPreserveDataAndDropHeaders(t *testing.T) {
	for _, kind := range []string{"rollout-progress", "deployment-result", "rollout-control-result", nativelogs.ReadyEventType} {
		t.Run(kind, func(t *testing.T) {
			message, config := nativeReverseMessage(t, kind)
			message.Header = hmessaging.Header{"nAtS-mSg-Id": {"foreign"}, "Nats-Expected-Stream": {"foreign"}, "X-Site": {"untrusted"}}
			original := message.Clone()
			actual, err := canonicalImportedMessage(config, message)
			require.NoError(t, err)
			require.Equal(t, original.Data, actual.Data)
			require.Equal(t, original.ID, actual.ID)
			require.Equal(t, original.Subject, actual.Subject)
			require.Equal(t, original.CreatedAt, actual.CreatedAt)
			require.Nil(t, actual.Header)
			require.True(t, actual.ExpiresAt.IsZero())
			require.Equal(t, original, message, "signed frame metadata must remain intact")
		})
	}
}

func TestNativeReverseRejectsScopeSchemaAndIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*hmessaging.Message, *ImportConfig)
	}{
		{"org", func(_ *hmessaging.Message, c *ImportConfig) { c.AuthoritativeOrg = "foreign" }},
		{"runner", func(_ *hmessaging.Message, c *ImportConfig) { c.NativeRunnerUUID = uuid.NewString() }},
		{"subject", func(m *hmessaging.Message, _ *ImportConfig) { m.Subject = "foreign.subject" }},
		{"id", func(m *hmessaging.Message, _ *ImportConfig) { m.ID = "foreign-id" }},
		{"created", func(m *hmessaging.Message, _ *ImportConfig) { m.CreatedAt = m.CreatedAt.Add(time.Second) }},
		{"expiry", func(m *hmessaging.Message, _ *ImportConfig) { m.ExpiresAt = m.CreatedAt.Add(time.Hour) }},
		{"duplicate", func(m *hmessaging.Message, _ *ImportConfig) {
			m.Data = append([]byte(`{"organization_id":"foreign",`), m.Data[1:]...)
		}},
		{"case_collision", func(m *hmessaging.Message, _ *ImportConfig) {
			m.Data = append([]byte(`{"ORGANIZATION_ID":"foreign",`), m.Data[1:]...)
		}},
		{"alias", func(m *hmessaging.Message, _ *ImportConfig) {
			m.Data = []byte(strings.Replace(string(m.Data), `"organization_id"`, `"ORGANIZATION_ID"`, 1))
		}},
		{"null", func(m *hmessaging.Message, _ *ImportConfig) {
			m.Data = []byte(strings.Replace(string(m.Data), `"payload": {}`, `"payload": null`, 1))
		}},
		{"trailing", func(m *hmessaging.Message, _ *ImportConfig) { m.Data = append(m.Data, []byte(` {}`)...) }},
		{"oversize", func(m *hmessaging.Message, _ *ImportConfig) {
			m.Data = []byte(strings.Repeat(" ", nativeReverseEventLimit+1))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			message, config := nativeReverseMessage(t, "deployment-result")
			test.change(&message, &config)
			_, err := canonicalImportedMessage(config, message)
			require.Error(t, err)
		})
	}
	for _, kind := range []string{"rollout-progress", "deployment-result", "rollout-control-result", nativelogs.ReadyEventType} {
		t.Run(kind+"_inner_link", func(t *testing.T) {
			message, config := nativeReverseMessage(t, kind)
			var event hmessaging.EventEnvelope
			require.NoError(t, json.Unmarshal(message.Data, &event))
			event.DeploymentID = uuid.NewString()
			var err error
			message.Data, err = json.Marshal(event)
			require.NoError(t, err)
			_, err = canonicalImportedMessage(config, message)
			require.Error(t, err)
		})
	}
	for _, test := range []struct{ name, value string }{{"type", "unknown"}, {"protocol_version", "2"}, {"deployment_id", "00000000-0000-0000-0000-000000000000"}} {
		t.Run(test.name, func(t *testing.T) {
			message, config := nativeReverseMessage(t, "deployment-result")
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(message.Data, &fields))
			fields[test.name], _ = json.Marshal(test.value)
			message.Data, _ = json.Marshal(fields)
			_, err := canonicalImportedMessage(config, message)
			require.Error(t, err)
		})
	}
}

func TestNativeReverseConfigurationIsCompleteBeforeStartupEffects(t *testing.T) {
	_, complete := nativeReverseMessage(t, "deployment-result")
	require.NoError(t, validateNativeReverseConfig(ImportConfig{}))
	require.NoError(t, validateNativeReverseConfig(ImportConfig{ExpectedAttachment: "log"}))
	require.NoError(t, validateNativeReverseConfig(complete))
	for _, change := range []func(*ImportConfig){
		func(c *ImportConfig) { c.AuthoritativeOrg = "" },
		func(c *ImportConfig) { c.NativeRunnerUUID = "" },
		func(c *ImportConfig) { c.NativeRunnerUUID = "00000000-0000-0000-0000-000000000000" },
		func(c *ImportConfig) { c.NativeRunnerUUID = "33333333-3333-4333-8333-33333333333A" },
		func(c *ImportConfig) { c.NativeRunnerUUID = "invalid" },
		func(c *ImportConfig) { c.AuthoritativeOrg = "bad.org" },
		func(c *ImportConfig) { c.AuthoritativeOrg = strings.Repeat("a", 129) },
		func(c *ImportConfig) { c.ExpectedAttachment = "" },
		func(c *ImportConfig) { c.ExpectedAttachment = "bundle" },
	} {
		config := complete
		change(&config)
		require.Error(t, validateNativeReverseConfig(config))
		// Invalid paths/key distinguish policy refusal from any later startup I/O.
		require.Equal(t, validateNativeReverseConfig(config).Error(), RunImporter(t.Context(), config).Error())
	}
}

func TestNativeReverseUnboundPreservesGenericMessages(t *testing.T) {
	message := hmessaging.Message{ID: "legacy", Subject: "legacy.subject", Data: []byte("opaque"), Header: hmessaging.Header{"X-Legacy": {"preserved"}}, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour)}
	actual, err := canonicalImportedMessage(ImportConfig{}, message)
	require.NoError(t, err)
	require.Equal(t, message, actual)
}

func TestNativeReversePreservesCaseSensitivePrivateResultMetadata(t *testing.T) {
	message, config := nativeReverseMessage(t, "deployment-result")
	var event hmessaging.EventEnvelope
	require.NoError(t, json.Unmarshal(message.Data, &event))
	// Use the actual result metadata type emitted by executionMetadata.
	body := api.DeploymentResultsUpdateBody{Status: api.Success, Metadata: api.DeploymentResultMetadata{{NodeId: "customer-node", Metadata: map[string]interface{}{"Name": "upper", "name": "lower"}}}}
	var err error
	event.Payload, err = json.Marshal(body)
	require.NoError(t, err)
	message.Data, err = json.MarshalIndent(event, "", "  ")
	require.NoError(t, err)
	actual, err := canonicalImportedMessage(config, message)
	require.NoError(t, err)
	require.Equal(t, message.Data, actual.Data)
	var accepted hmessaging.EventEnvelope
	require.NoError(t, json.Unmarshal(actual.Data, &accepted))
	var acceptedBody api.DeploymentResultsUpdateBody
	require.NoError(t, json.Unmarshal(accepted.Payload, &acceptedBody))
	require.Equal(t, body.Metadata, acceptedBody.Metadata)
}

func TestNativeReverseControlEventRawSizeBoundary(t *testing.T) {
	message, config := nativeReverseMessage(t, "rollout-control-result")
	message.Data = append(message.Data, []byte(strings.Repeat(" ", signedcontrol.MaxBytes-len(message.Data)))...)
	actual, err := canonicalImportedMessage(config, message)
	require.NoError(t, err)
	require.Len(t, actual.Data, signedcontrol.MaxBytes)
	require.Equal(t, message.Data, actual.Data)
	message.Data = append(message.Data, ' ')
	_, err = canonicalImportedMessage(config, message)
	require.Error(t, err)
	// Ordinary result events retain the existing larger event limit.
	result, config := nativeReverseMessage(t, "deployment-result")
	result.Data = append(result.Data, []byte(strings.Repeat(" ", signedcontrol.MaxBytes+1-len(result.Data)))...)
	_, err = canonicalImportedMessage(config, result)
	require.NoError(t, err)
}
