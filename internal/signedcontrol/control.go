// Package signedcontrol defines the bounded immutable Native control wire contract.
package signedcontrol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

const Version = "stellwerk.pipeline.control/v1"
const ResultVersion = "stellwerk.pipeline.control-result/v1"
const MaxBytes = 8 << 10

var ErrInvalid = errors.New("invalid_control")
var ErrSignature = errors.New("control_signature_refused")

type Target struct {
	PipelineVersion          string `json:"pipeline_version"`
	RunnerID                 string `json:"runner_id"`
	RunnerUUID               string `json:"runner_uuid"`
	RuntimeVersion           string `json:"runtime_version"`
	StateBackendType         string `json:"state_backend_type"`
	StateConfigurationDigest string `json:"state_configuration_digest"`
	ToolsDigest              string `json:"tools_digest"`
}

type Request struct {
	Version         string    `json:"version"`
	ID              string    `json:"id"`
	RolloutID       string    `json:"rollout_id"`
	OrgID           string    `json:"org_id"`
	EnvironmentID   string    `json:"environment_id"`
	Target          Target    `json:"target"`
	ExecutionDigest string    `json:"execution_digest"`
	ActorID         string    `json:"actor_id"`
	Action          string    `json:"action"`
	Subject         string    `json:"subject"`
	FailedStep      string    `json:"failed_step"`
	Revision        int64     `json:"revision"`
	IssuedAt        time.Time `json:"issued_at"`
	ExpiresAt       time.Time `json:"expires_at"`
}

type Signed struct {
	Payload   json.RawMessage `json:"payload"`
	Signature string          `json:"signature"`
}

// Result is metadata only. It never embeds progress, plans, logs or tool errors.
type Result struct {
	Version         string `json:"version"`
	ControlID       string `json:"control_id"`
	RolloutID       string `json:"rollout_id"`
	RequestDigest   string `json:"request_digest"`
	ExecutionDigest string `json:"execution_digest"`
	Outcome         string `json:"outcome"`
	Code            string `json:"code"`
	Phase           string `json:"phase"`
	Revision        int64  `json:"revision"`
}

// Receipt retains the exact signed payload and frozen result bytes across replay.
type Receipt struct {
	Request       string `json:"request"`
	RequestDigest string `json:"request_digest"`
	Result        string `json:"result"`
	ResultDigest  string `json:"result_digest"`
}

func Digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func UUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value && parsed != uuid.Nil
}
func Hash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && strings.ToLower(value) == value
}

func Decode(data []byte, key ed25519.PublicKey) (Request, []byte, error) {
	if len(data) == 0 || len(data) > MaxBytes {
		return Request{}, nil, ErrInvalid
	}
	if _, err := object(data, "payload signature"); err != nil {
		return Request{}, nil, err
	}
	var signed Signed
	if json.Unmarshal(data, &signed) != nil {
		return Request{}, nil, ErrInvalid
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(signed.Signature)
	if err != nil || base64.StdEncoding.EncodeToString(signature) != signed.Signature || len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, signed.Payload, signature) {
		return Request{}, nil, ErrSignature
	}
	request, err := DecodeRequest(signed.Payload)
	return request, signed.Payload, err
}

// DecodeRequest validates immutable shape, including time relationships, without
// consulting a clock. Authenticated retained replay may happen after expiry.
func DecodeRequest(data []byte) (Request, error) {
	var request Request
	if len(data) == 0 || len(data) > MaxBytes {
		return request, ErrInvalid
	}
	fields, err := object(data, "version id rollout_id org_id environment_id target execution_digest actor_id action subject failed_step revision issued_at expires_at")
	if err != nil {
		return request, err
	}
	if _, err = object(fields["target"], "pipeline_version runner_id runner_uuid runtime_version state_backend_type state_configuration_digest tools_digest"); err != nil {
		return request, err
	}
	if json.Unmarshal(data, &request) != nil || !request.valid() {
		return Request{}, ErrInvalid
	}
	return request, nil
}

func (r Request) valid() bool {
	for _, value := range []string{r.ID, r.RolloutID, r.ActorID, r.EnvironmentID} {
		if !UUID(value) {
			return false
		}
	}
	if r.Version != Version || r.OrgID == "" || len(r.OrgID) > 128 || !Hash(r.ExecutionDigest) || !r.Target.valid() || r.IssuedAt.IsZero() || !r.ExpiresAt.After(r.IssuedAt) || r.ExpiresAt.Sub(r.IssuedAt) > 7*24*time.Hour {
		return false
	}
	return r.validAuthority()
}

func (t Target) valid() bool {
	return UUID(t.RunnerUUID) && t.RunnerID != "" && len(t.RunnerID) <= 128 && t.PipelineVersion == "stellwerk.pipeline/v2" && t.RuntimeVersion == "stellwerk.runner.rollout/v2" && Hash(t.ToolsDigest) && Hash(t.StateConfigurationDigest) && slices.Contains([]string{"kubernetes", "s3", "gcs", "azurerm", "local"}, t.StateBackendType)
}

func (r Request) validAuthority() bool {
	switch r.Action {
	case "approve", "deny":
		return Hash(r.Subject) && r.FailedStep == "" && r.Revision == 0
	case "cancel":
		return r.Subject == "" && r.FailedStep == "" && r.Revision == 0
	case "repair":
		return r.Subject == "" && r.FailedStep != "" && len(r.FailedStep) <= 128 && r.Revision > 0
	default:
		return false
	}
}

func Freeze(request Request, payload []byte, outcome, code, phase string, revision int64) (Receipt, error) {
	result := Result{Version: ResultVersion, ControlID: request.ID, RolloutID: request.RolloutID, RequestDigest: Digest(payload), ExecutionDigest: request.ExecutionDigest, Outcome: outcome, Code: code, Phase: phase, Revision: revision}
	encoded, err := json.Marshal(result)
	if err != nil {
		return Receipt{}, err
	}
	return Receipt{Request: string(payload), RequestDigest: result.RequestDigest, Result: string(encoded), ResultDigest: Digest(encoded)}, nil
}

func DecodeReceipt(data []byte) (Receipt, Request, Result, error) {
	var receipt Receipt
	var result Result
	if len(data) > 2*MaxBytes {
		return receipt, Request{}, result, ErrInvalid
	}
	if _, err := object(data, "request request_digest result result_digest"); err != nil {
		return receipt, Request{}, result, err
	}
	if json.Unmarshal(data, &receipt) != nil || Digest([]byte(receipt.Request)) != receipt.RequestDigest || Digest([]byte(receipt.Result)) != receipt.ResultDigest {
		return Receipt{}, Request{}, result, ErrInvalid
	}
	request, err := DecodeRequest([]byte(receipt.Request))
	if err != nil {
		return Receipt{}, request, result, err
	}
	if _, err = object([]byte(receipt.Result), "version control_id rollout_id request_digest execution_digest outcome code phase revision"); err != nil {
		return Receipt{}, request, result, err
	}
	if json.Unmarshal([]byte(receipt.Result), &result) != nil || !result.matches(request, receipt.RequestDigest) || !result.validOutcome() {
		return Receipt{}, request, Result{}, ErrInvalid
	}

	return receipt, request, result, nil
}

func (r Result) matches(request Request, requestDigest string) bool {
	return r.Version == ResultVersion && r.ControlID == request.ID && r.RolloutID == request.RolloutID && r.RequestDigest == requestDigest && r.ExecutionDigest == request.ExecutionDigest
}

func (r Result) validOutcome() bool {
	return (r.Outcome == "applied" || r.Outcome == "refused") && (r.Outcome == "refused") == (r.Code != "") && r.Phase != "" && len(r.Phase) <= 64 && len(r.Code) <= 128 && r.Revision > 0
}

// Read member names before projecting values, so duplicate and case-aliased
// members cannot be hidden by encoding/json's last-member behavior.
func object(data []byte, names string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrInvalid
	}
	fields := make(map[string]json.RawMessage)
	allowed := strings.Fields(names)
	for decoder.More() {
		token, err = decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || !slices.Contains(allowed, name) {
			return nil, ErrInvalid
		}
		if _, duplicate := fields[name]; duplicate {
			return nil, ErrInvalid
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(value, []byte("null")) {
			return nil, ErrInvalid
		}
		fields[name] = value
	}
	if _, err = decoder.Token(); err != nil || len(fields) != len(allowed) || decoder.Decode(new(any)) != io.EOF {
		return nil, ErrInvalid
	}
	return fields, nil
}
