package rollout

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stretchr/testify/require"
)

func TestNativeCommandExactAuthorityAtFreshAndRetainedDecode(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	id := "11111111-1111-4111-8111-111111111111"
	engine := &Engine{config: Config{Postgres: &PostgresConfig{}, OrgID: "org", RunnerID: "site", RunnerUUID: id, PublicKey: public}}
	command := Command{ID: "deployment", OrgID: "org", RunnerID: "site", RunnerUUID: id, EnvironmentID: "environment", Version: NativePipelineVersion, Strategy: "plan", IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), ToolsDigest: strings.Repeat("a", 64), Request: rolloutexec.Request{ID: "deployment", OrgID: "org", RunnerID: "site", RunnerUUID: id, EnvironmentID: "environment", Backend: "local", Tool: "opentofu"}}
	command.ExecutionLimits = &ExecutionLimits{PlanSeconds: 1800, ApplySeconds: 7200, TaskSeconds: 1800}
	// Local is standalone Engine authority, never a Core-registered backend.
	command.NativeTarget = &NativeExecutionTarget{RunnerID: "site", RunnerUUID: id, RuntimeVersion: NativeRuntimeVersion, PipelineVersion: NativePipelineVersion, ToolsDigest: command.ToolsDigest, StateBackendType: "local", StateConfigurationDigest: strings.Repeat("b", 64)}
	valid, err := json.Marshal(command)
	require.NoError(t, err)
	for _, bad := range []string{
		strings.Replace(string(valid), `"execution_limits":{"plan_seconds":1800,"apply_seconds":7200,"task_seconds":1800},`, ``, 1),
		strings.Replace(string(valid), `"execution_limits":`, `"EXECUTION_LIMITS":`, 1),
		strings.Replace(string(valid), `"execution_limits":{"plan_seconds":1800,"apply_seconds":7200,"task_seconds":1800}`, `"execution_limits":null`, 1),
		strings.TrimSuffix(string(valid), "}") + `,"execution_limits":{"plan_seconds":1,"apply_seconds":1,"task_seconds":1}}`,
		strings.Replace(string(valid), `"request":{`, `"request":{"deployment_token":"",`, 1),
		strings.Replace(string(valid), `"request":{`, `"request":{"DEPLOYMENT_TOKEN":null,`, 1),
		strings.Replace(string(valid), `"request":{`, `"request":{"deployment_to\u006ben":false,`, 1),
		strings.Replace(string(valid), `"request":`, `"request":{"deployment_token":null},"request":`, 1),
		strings.TrimSuffix(string(valid), "}") + `,"request":{}}`,
		strings.Replace(string(valid), `"request":`, `"Request":`, 1),
		strings.Replace(string(valid), `"request":`, `"reque\u0073t":{"deployment_token":null},"request":`, 1),
		strings.Replace(string(valid), `"request":{`, `"request":{"encrypted_logs_recipient":null,`, 1),
		strings.Replace(string(valid), `"request":{`, `"request":{"encrypted_logs_recipient":true,`, 1),
		strings.Replace(string(valid), `"request":{`, `"request":{"encrypted_logs_recipient":"a","encrypted_logs_recipient":"b",`, 1),
		strings.Replace(string(valid), `"request":{`, `"request":{"ENCRYPTED_LOGS_RECIPIENT":"a",`, 1),
	} {
		payload := []byte(bad)
		_, err = engine.verify(SignedCommand{Payload: payload, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload))})
		requireCode(t, err, "invalid_command")
		_, err = engine.decodeStoredCommand(command.ID, digest(payload), payload)
		requireCode(t, err, "stored_command_unavailable")
	}
	for _, payload := range [][]byte{valid, []byte(strings.Replace(string(valid), `"request":`, `"reque\u0073t":`, 1))} {
		_, err = engine.verify(SignedCommand{Payload: payload, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload))})
		require.NoError(t, err)
		_, err = engine.decodeStoredCommand(command.ID, digest(payload), payload)
		require.NoError(t, err)
	}
	for _, recipient := range []string{"", "invalid-export-recipient"} {
		command.Request.EncryptedLogsRecipient = recipient
		payload, marshalErr := json.Marshal(command)
		require.NoError(t, marshalErr)
		_, verifyErr := engine.verify(SignedCommand{Payload: payload, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload))})
		require.NoError(t, verifyErr, "recipient parsing belongs to finite export preparation")
	}
	command.Request.EncryptedLogsRecipient = ""
	// Adding the Native field must not make even null Native authority known to v1.
	engine.config.Postgres = nil
	engine.config.RunnerUUID = ""
	command.NativeTarget = nil
	command.ExecutionLimits = nil
	command.RunnerUUID = ""
	command.Request.RunnerUUID = ""
	for _, version := range []string{PipelineVersion, Version} {
		command.Version = version
		legacy, marshalErr := json.Marshal(command)
		require.NoError(t, marshalErr)
		alias := []byte(strings.Replace(string(legacy), `"request":`, `"Request":`, 1))
		_, err = engine.verify(SignedCommand{Payload: alias, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, alias))})
		require.NoError(t, err)
		for _, field := range []string{`"native_target":null`, `"execution_limits":null`} {
			extra := []byte(strings.TrimSuffix(string(legacy), "}") + "," + field + "}")
			_, err = engine.verify(SignedCommand{Payload: extra, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, extra))})
			requireCode(t, err, "invalid_command")
		}
		for _, value := range []string{`null`, `"recipient"`} {
			payload := []byte(strings.Replace(string(legacy), `"request":{`, `"request":{"encrypted_logs_recipient":`+value+`,`, 1))
			_, err = engine.verify(SignedCommand{Payload: payload, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload))})
			requireCode(t, err, "invalid_command")
		}
	}
}
