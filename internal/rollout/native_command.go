package rollout

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
)

// NativeExecutionTarget carries accepted central authority. The state digest is
// opaque here; it is not the private backend identity retained by this Engine.
type NativeExecutionTarget = nativelogs.Target

func (e *Engine) nativeCommandScope(command Command) error {
	target := command.NativeTarget
	if e.config.Postgres == nil {
		if target != nil || command.ExecutionLimits != nil {
			return refused("command_scope_refused")
		}
		return nil
	}
	if command.ExecutionLimits == nil || !command.ExecutionLimits.valid() {
		return refused("execution_limits_refused")
	}
	if target == nil || target.RunnerID != command.RunnerID || target.RunnerUUID != command.RunnerUUID || target.RuntimeVersion != NativeRuntimeVersion || target.PipelineVersion != NativePipelineVersion || target.ToolsDigest != command.ToolsDigest || !canonicalDigest(target.ToolsDigest) || !canonicalDigest(target.StateConfigurationDigest) || target.StateBackendType != command.Request.Backend || command.Request.MetadataKey != "" {
		return refused("command_scope_refused")
	}
	switch target.StateBackendType {
	case "kubernetes", "s3", "gcs", "azurerm", "local":
		return nil
	default:
		return refused("command_scope_refused")
	}
}

func canonicalDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Read names in order: projecting a map first would hide an earlier request.
func nativeCommandFields(payload []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	found, limits := false, false
	for decoder.More() {
		token, err = decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return false
		}
		if strings.EqualFold(name, "request") {
			if found || name != "request" || !readNativeRequest(decoder) {
				return false
			}
			found = true
		} else if strings.EqualFold(name, "execution_limits") {
			var value ExecutionLimits
			if limits || name != "execution_limits" || decoder.Decode(&value) != nil {
				return false
			}
			limits = true
		} else {
			var value json.RawMessage
			if decoder.Decode(&value) != nil {
				return false
			}
		}
	}
	_, err = decoder.Token()
	return err == nil && found && limits && decoder.Decode(new(any)) == io.EOF
}

func readNativeRequest(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err = decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[strings.ToLower(name)] || strings.EqualFold(name, "deployment_token") || strings.EqualFold(name, "encrypted_logs_recipient") && name != "encrypted_logs_recipient" {
			return false
		}
		seen[strings.ToLower(name)] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil || name == "encrypted_logs_recipient" && (len(value) == 0 || value[0] != '"') {
			return false
		}
	}
	_, err = decoder.Token()
	return err == nil
}

func (e *Engine) decodeCommandPayload(payload []byte) (Command, error) {
	var command Command
	if e.config.Postgres != nil {
		if !nativeCommandFields(payload) {
			return command, refused("invalid_command")
		}
	} else {
		// This new field remains unknown to legacy payloads, even when explicitly null.
		var extra struct {
			Target  json.RawMessage `json:"native_target"`
			Limits  json.RawMessage `json:"execution_limits"`
			Request struct {
				Logs json.RawMessage `json:"encrypted_logs_recipient"`
			} `json:"request"`
		}
		if json.Unmarshal(payload, &extra) != nil || len(extra.Target) != 0 || len(extra.Limits) != 0 || len(extra.Request.Logs) != 0 {
			return command, refused("invalid_command")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&command) != nil || decoder.Decode(new(any)) != io.EOF {
		return command, refused("invalid_command")
	}
	return command, e.scope(command)
}
