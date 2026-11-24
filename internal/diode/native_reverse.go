package diode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gateway"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/signedcontrol"
)

// Match the connected Gateway's event body limit.
const nativeReverseEventLimit = 4 << 20

func validateNativeReverseConfig(config ImportConfig) error {
	if config.AuthoritativeOrg == "" && config.NativeRunnerUUID == "" {
		return nil
	}
	route, err := hmessaging.NativeRunnerToken(config.NativeRunnerUUID)
	if err != nil || config.AuthoritativeOrg == "" || len(config.AuthoritativeOrg) > 128 {
		return errors.New("native reverse import requires an organization and canonical nonzero runner UUID")
	}
	if _, err := hmessaging.RunnerEventSubject(config.AuthoritativeOrg, route, "deployment-result"); err != nil {
		return fmt.Errorf("native reverse organization: %w", err)
	}
	if config.ExpectedAttachment != "log" {
		return errors.New("native reverse import requires ExpectedAttachment=log")
	}
	return nil
}

// The signed frame remains intact for original attachment checks and custody.
// Only the fresh publication uses centrally derived routing and metadata.
// The importer validates configuration before calling this function.
func canonicalImportedMessage(config ImportConfig, message hmessaging.Message) (hmessaging.Message, error) {
	if config.NativeRunnerUUID == "" {
		return message, nil
	}
	route := hmessaging.NativeRunnerTokenPrefix + config.NativeRunnerUUID
	event, err := nativeEventEnvelope(message.Data)
	if err != nil || event.OrganizationID != config.AuthoritativeOrg || event.RunnerID != route ||
		!gateway.ValidNativeEventIdentity(event) {
		return hmessaging.Message{}, errors.New("native reverse event authority or identity is invalid")
	}
	subject, subjectErr := hmessaging.RunnerEventSubject(config.AuthoritativeOrg, route, event.Type)
	id, idErr := hmessaging.NativeMessageID(config.AuthoritativeOrg, route, "events", event.Type, event.EventID)
	if subjectErr != nil || idErr != nil || message.Subject != subject || message.ID != id ||
		!message.CreatedAt.Equal(event.CreatedAt) || !message.ExpiresAt.IsZero() {
		return hmessaging.Message{}, errors.New("native reverse transport identity is invalid")
	}
	return hmessaging.Message{ID: id, Subject: subject, Data: message.Data, CreatedAt: event.CreatedAt}, nil
}

func nativeEventEnvelope(data []byte) (hmessaging.EventEnvelope, error) {
	var event hmessaging.EventEnvelope
	if len(data) > nativeReverseEventLimit || !exactNativeEventMembers(data) ||
		json.Unmarshal(data, &event) != nil || len(event.Payload) == 0 ||
		event.ProtocolVersion != hmessaging.ProtocolVersionV1 || event.CreatedAt.IsZero() ||
		(event.Type == "rollout-control-result" && len(data) > signedcontrol.MaxBytes) {
		return event, errors.New("invalid Native reverse event envelope")
	}
	return event, nil
}

// Check only the envelope. Private result/progress payloads retain their existing
// case-sensitive map keys; the Gateway's per-type validators own their identity.
func exactNativeEventMembers(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err = decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return false
		}
		switch name {
		case "protocol_version", "event_id", "command_id", "organization_id", "runner_id", "deployment_id", "type", "created_at", "payload":
		default:
			return false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return false
		}
		seen[name] = true
	}
	token, err = decoder.Token()
	return err == nil && token == json.Delim('}') && decoder.Decode(new(any)) == io.EOF
}
