package nativelogs

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/stretchr/testify/require"
)

func authorityFixture() Authority {
	return Authority{OrganizationID: "org", EnvironmentID: "environment", DeploymentID: "deployment", CommandDigest: strings.Repeat("a", 64), Target: Target{RunnerID: "site", RunnerUUID: "11111111-1111-4111-8111-111111111111", RuntimeVersion: "native/v1", PipelineVersion: "native-pipeline/v1", StateBackendType: "local", StateConfigurationDigest: strings.Repeat("b", 64), ToolsDigest: strings.Repeat("c", 64)}, Phase: "plan", ExecutionID: "deployment", CaptureRevision: 3, Subject: strings.Repeat("d", 64)}
}

func encryptedJSON(t *testing.T, data []byte, recipient age.Recipient) []byte {
	t.Helper()
	var output bytes.Buffer
	writer, err := age.Encrypt(&output, recipient)
	require.NoError(t, err)
	_, err = writer.Write(data)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return output.Bytes()
}

func TestArtifactBothRecipientsAndEveryAuthorityField(t *testing.T) {
	site, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	managed, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	foreign, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	authority := authorityFixture()
	authority.SourcePlanID, authority.SourceSubject = "source", strings.Repeat("e", 64)
	original := Envelope{Version: 1, Authority: authority, Status: PendingTransfer, Logs: []byte("PRIVATE_DIAGNOSTIC\x00\xff")}
	ciphertext, err := Encode(original, site.Recipient(), managed.Recipient())
	require.NoError(t, err)
	require.NotContains(t, string(ciphertext), "PRIVATE_DIAGNOSTIC")
	for _, identity := range []age.Identity{site, managed} {
		actual, err := Decode(ciphertext, identity, authority)
		require.NoError(t, err)
		require.Equal(t, original, actual)
	}
	empty, err := Decode(ciphertext, foreign, authority)
	require.Error(t, err)
	require.Empty(t, empty.Logs)
	changes := map[string]func(*Authority){
		"org":            func(a *Authority) { a.OrganizationID = "foreign" },
		"environment":    func(a *Authority) { a.EnvironmentID = "foreign" },
		"deployment":     func(a *Authority) { a.DeploymentID = "foreign" },
		"command":        func(a *Authority) { a.CommandDigest = strings.Repeat("f", 64) },
		"runner_id":      func(a *Authority) { a.Target.RunnerID = "foreign" },
		"runner_uuid":    func(a *Authority) { a.Target.RunnerUUID = "22222222-2222-4222-8222-222222222222" },
		"runtime":        func(a *Authority) { a.Target.RuntimeVersion = "foreign" },
		"pipeline":       func(a *Authority) { a.Target.PipelineVersion = "foreign" },
		"tools":          func(a *Authority) { a.Target.ToolsDigest = strings.Repeat("f", 64) },
		"backend":        func(a *Authority) { a.Target.StateBackendType = "s3" },
		"state":          func(a *Authority) { a.Target.StateConfigurationDigest = strings.Repeat("f", 64) },
		"phase":          func(a *Authority) { a.Phase = "apply"; a.Subject = "" },
		"step":           func(a *Authority) { a.StepID = "foreign" },
		"execution":      func(a *Authority) { a.ExecutionID = "foreign" },
		"revision":       func(a *Authority) { a.CaptureRevision++ },
		"subject":        func(a *Authority) { a.Subject = strings.Repeat("f", 64) },
		"source":         func(a *Authority) { a.SourcePlanID = "foreign" },
		"source_subject": func(a *Authority) { a.SourceSubject = strings.Repeat("f", 64) },
		"inherited":      func(a *Authority) { a.Inherited = true; a.ExecutionID = a.SourcePlanID },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			expected := authority
			change(&expected)
			actual, err := Decode(ciphertext, managed, expected)
			require.Error(t, err)
			require.Empty(t, actual.Logs)
		})
	}
}

func TestArtifactStrictJSONAndNoPartialPlaintext(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	authority := authorityFixture()
	valid, err := json.Marshal(Envelope{Version: 1, Authority: authority, Status: PendingTransfer, Logs: []byte("private")})
	require.NoError(t, err)
	for _, bad := range []string{
		strings.Replace(string(valid), `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(string(valid), `"authority":{`, `"authority":{"organization_id":"foreign",`, 1),
		strings.Replace(string(valid), `"runner_id":"site"`, `"runner_id":"foreign","runner_id":"site"`, 1),
		strings.Replace(string(valid), `"phase":"plan"`, `"PHASE":"apply","phase":"plan"`, 1),
		strings.Replace(string(valid), `"status":`, `"unknown":true,"status":`, 1),
		string(valid) + ` {}`, string(valid) + ` trailing`,
	} {
		actual, err := Decode(encryptedJSON(t, []byte(bad), identity.Recipient()), identity, authority)
		require.Error(t, err)
		require.Empty(t, actual.Logs)
	}
	oversized, err := json.Marshal(Envelope{Version: 1, Authority: authority, Status: PendingTransfer, Logs: make([]byte, RawLimit+1)})
	require.NoError(t, err)
	actual, err := Decode(encryptedJSON(t, oversized, identity.Recipient()), identity, authority)
	require.Error(t, err)
	require.Empty(t, actual.Logs)
	_, err = Decode(encryptedJSON(t, make([]byte, EnvelopeLimit+1), identity.Recipient()), identity, authority)
	require.Error(t, err)
	_, err = Decode(make([]byte, CiphertextLimit+1), identity, authority)
	require.Error(t, err)
}

func TestArtifactHighEntropyBoundaryAndDescriptor(t *testing.T) {
	site, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	managed, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	logs := make([]byte, RawLimit)
	_, err = rand.Read(logs)
	require.NoError(t, err)
	authority := authorityFixture()
	ciphertext, err := Encode(Envelope{Version: 1, Authority: authority, Status: PendingTransfer, Logs: logs}, site.Recipient(), managed.Recipient())
	require.NoError(t, err)
	require.LessOrEqual(t, len(ciphertext), CiphertextLimit)
	descriptor := Descriptor{Version: 1, Authority: authority, Status: PendingTransfer, CiphertextSHA256: Hash(ciphertext), CiphertextBytes: len(ciphertext)}
	descriptor.ObjectKey = authority.ObjectKey(descriptor.CiphertextSHA256)
	require.NoError(t, descriptor.VerifyCiphertext(ciphertext))
	metadata, err := json.Marshal(descriptor)
	require.NoError(t, err)
	require.LessOrEqual(t, len(metadata), MetadataLimit)
	require.NotContains(t, string(metadata), "logs")
	actual, err := Decode(ciphertext, managed, authority)
	require.NoError(t, err)
	require.Equal(t, logs, actual.Logs)
	ciphertext[len(ciphertext)-1] ^= 1
	require.Error(t, descriptor.VerifyCiphertext(ciphertext))
	for _, change := range []func(*Descriptor){
		func(d *Descriptor) { d.CiphertextBytes = CiphertextLimit + 1 },
		func(d *Descriptor) { d.CiphertextBytes = 0 },
		func(d *Descriptor) { d.ObjectKey = "environment/native/foreign" },
		func(d *Descriptor) { d.CiphertextSHA256 = strings.Repeat("f", 64) },
		func(d *Descriptor) { d.Authority.ExecutionID = "../foreign" },
		func(d *Descriptor) { d.Authority.ExecutionID = strings.Repeat("x", 129) },
		func(d *Descriptor) { d.Authority.CaptureRevision = 0 },
	} {
		bad := descriptor
		change(&bad)
		require.Error(t, bad.Validate())
	}
	_, err = Encode(Envelope{Version: 1, Authority: authority, Status: PendingTransfer, Logs: make([]byte, RawLimit+1)}, site.Recipient())
	require.Error(t, err)
}

func TestArtifactEmptyFailureAndTooLargeStatuses(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	for _, test := range []struct {
		status string
		logs   []byte
	}{
		{PendingTransfer, []byte{}}, {PendingTransfer, []byte("failed plan diagnostic")},
		{TooLarge, nil}, {NotManaged, []byte("site only")}, {Unavailable, []byte("export failed")},
	} {
		original := Envelope{Version: 1, Authority: authorityFixture(), Status: test.status, Logs: test.logs}
		ciphertext, err := Encode(original, identity.Recipient())
		require.NoError(t, err)
		actual, err := Decode(ciphertext, identity, original.Authority)
		require.NoError(t, err)
		require.Equal(t, original, actual)
	}
	_, err = Encode(Envelope{Version: 1, Authority: authorityFixture(), Status: TooLarge, Logs: []byte("partial")}, identity.Recipient())
	require.Error(t, err)
}

func TestStrictJSONBoundsNestingBeforeTypedDecode(t *testing.T) {
	// RawMessage accepts either shape, isolating the framing walk from typed
	// field/type refusal. These small inputs also support the old/new probe.
	for _, test := range []struct {
		name, open, close string
		depth             int
		accepted          bool
	}{
		{"array boundary", "[", "]", 16, true},
		{"array over boundary", "[", "]", 17, false},
		{"array excessive", "[", "]", 64, false},
		{"object boundary", `{"child":`, "}", 16, true},
		{"object over boundary", `{"child":`, "}", 17, false},
		{"object excessive", `{"child":`, "}", 64, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := []byte(strings.Repeat(test.open, test.depth) + "0" + strings.Repeat(test.close, test.depth))
			var raw json.RawMessage
			err := StrictJSON(data, &raw, len(data))
			if test.accepted {
				require.NoError(t, err)
				require.Equal(t, string(data), string(raw))
			} else {
				require.ErrorIs(t, err, ErrArtifact)
				require.Nil(t, raw, "framing refuses before typed decoding")
			}
		})
	}
}
