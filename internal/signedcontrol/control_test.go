package signedcontrol

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func controlRequest() Request {
	return Request{Version: Version, ID: uuid.NewString(), RolloutID: uuid.NewString(), OrgID: "org", EnvironmentID: uuid.NewString(), ActorID: uuid.NewString(), Target: Target{PipelineVersion: "stellwerk.pipeline/v2", RunnerID: "site", RunnerUUID: uuid.NewString(), RuntimeVersion: "stellwerk.runner.rollout/v2", StateBackendType: "local", StateConfigurationDigest: strings.Repeat("a", 64), ToolsDigest: strings.Repeat("b", 64)}, ExecutionDigest: strings.Repeat("c", 64), Action: "approve", Subject: strings.Repeat("d", 64), IssuedAt: time.Now().Add(-time.Hour).UTC(), ExpiresAt: time.Now().Add(-time.Minute).UTC()}
}

func TestDecodeAuthenticatesExpiredImmutableControl(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	request := controlRequest()
	payload, err := json.Marshal(request)
	require.NoError(t, err)
	signed, err := json.Marshal(Signed{Payload: payload, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload))})
	require.NoError(t, err)
	decoded, retained, err := Decode(signed, public)
	require.NoError(t, err)
	require.Equal(t, request, decoded)
	require.Equal(t, payload, retained)
	signed[len(signed)-5] ^= 1
	_, _, err = Decode(signed, public)
	require.Error(t, err)
}

func TestDecodeRejectsAmbiguousOrIncompleteControl(t *testing.T) {
	payload, err := json.Marshal(controlRequest())
	require.NoError(t, err)
	original := string(payload)
	cases := map[string]string{
		"duplicate":        strings.Replace(original, `"version":`, `"id":"ignored","version":`, 1),
		"alias":            strings.Replace(original, `"id":`, `"ID":`, 1),
		"unknown":          strings.Replace(original, `"id":`, `"extra":1,"id":`, 1),
		"nested duplicate": strings.Replace(original, `"runner_id":`, `"runner_id":"other","runner_id":`, 1),
		"nested alias":     strings.Replace(original, `"runner_id":`, `"Runner_ID":`, 1),
		"missing":          strings.Replace(original, `"revision":0,`, ``, 1),
		"null":             strings.Replace(original, `"revision":0`, `"revision":null`, 1),
		"trailing":         original + `{}`,
		"oversize":         original + strings.Repeat(" ", MaxBytes),
		"wrong authority":  strings.Replace(original, `"revision":0`, `"revision":7`, 1),
		"bad time":         strings.Replace(original, `"issued_at":`, `"issued_at":null,"ignored":`, 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) { _, err := DecodeRequest([]byte(body)); require.ErrorIs(t, err, ErrInvalid) })
	}
}

func TestSignedEnvelopeRejectsAmbiguousNamesAndCompleteInputLimit(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	payload, err := json.Marshal(controlRequest())
	require.NoError(t, err)
	encoded, err := json.Marshal(Signed{Payload: payload, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload))})
	require.NoError(t, err)
	for _, body := range []string{strings.Replace(string(encoded), `"signature":`, `"signature":"x","signature":`, 1), strings.Replace(string(encoded), `"payload":`, `"Payload":`, 1), string(encoded) + `true`, string(encoded) + strings.Repeat(" ", MaxBytes)} {
		_, _, err := Decode([]byte(body), public)
		require.Error(t, err)
	}
}

func TestFrozenReceiptPreservesExactRequestAndResult(t *testing.T) {
	request := controlRequest()
	payload, err := json.Marshal(request)
	require.NoError(t, err)
	receipt, err := Freeze(request, payload, "refused", "approval_subject_conflict", "awaiting_approval", 7)
	require.NoError(t, err)
	encoded, err := json.Marshal(receipt)
	require.NoError(t, err)
	retained, decoded, result, err := DecodeReceipt(encoded)
	require.NoError(t, err)
	require.Equal(t, receipt, retained)
	require.Equal(t, request, decoded)
	require.Equal(t, "refused", result.Outcome)
	receipt.Result += " "
	encoded, err = json.Marshal(receipt)
	require.NoError(t, err)
	_, _, _, err = DecodeReceipt(encoded)
	require.ErrorIs(t, err, ErrInvalid)
}

func TestDecodeRejectsCompoundFieldNames(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	for _, trial := range []struct {
		action      string
		member      string
		replacement string
	}{
		{"approve", `"revision":0`, `"revision issued_at":7`},
		{"deny", `"revision":0`, `"revision issued_at":7`},
		{"cancel", `"revision":0`, `"revision issued_at":7`},
		{"cancel", `"subject":""`, `"subject failed_step":"ignored"`},
		{"cancel", `"failed_step":""`, `"failed_step revision":"ignored"`},
	} {
		for _, escaped := range []bool{false, true} {
			replacement := trial.replacement
			if escaped {
				replacement = strings.ReplaceAll(replacement, " ", `\u0020`)
			}
			t.Run(trial.action+"/"+replacement, func(t *testing.T) {
				request := controlRequest()
				request.Action = trial.action
				if trial.action == "cancel" {
					request.Subject = ""
				}
				payload, err := json.Marshal(request)
				require.NoError(t, err)
				_, err = DecodeRequest(payload)
				require.NoError(t, err)
				payload = []byte(strings.Replace(string(payload), trial.member, replacement, 1))
				signed, err := json.Marshal(Signed{Payload: payload, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload))})
				require.NoError(t, err)
				_, _, err = Decode(signed, public)
				require.ErrorIs(t, err, ErrInvalid)
				_, err = DecodeRequest(payload)
				require.ErrorIs(t, err, ErrInvalid)
			})
		}
	}
}
