package diode

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stretchr/testify/require"
)

func TestFrameSignatureAndPayloadIntegrity(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	message := hmessaging.Message{
		ID: "command-1", Subject: "po.v1.orgs.test.runners.runner.commands",
		Data: []byte(`{"command":"create-job"}`), CreatedAt: time.Now().UTC(),
	}
	frame, err := signFrame(42, message, privateKey)
	require.NoError(t, err)
	require.NoError(t, verifyFrame(frame, publicKey))

	frame.Message.Data[0] ^= 1
	require.ErrorContains(t, verifyFrame(frame, publicKey), "checksum")
}

func TestImportQuarantinesInvalidFrameAndContinues(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	root := t.TempDir()
	input := filepath.Join(root, "input")
	processed := filepath.Join(root, "processed")
	quarantineDir := filepath.Join(root, "quarantine")
	require.NoError(t, os.MkdirAll(input, 0o700))
	require.NoError(t, os.MkdirAll(processed, 0o700))
	require.NoError(t, os.MkdirAll(quarantineDir, 0o700))
	invalidPath := filepath.Join(input, "0001-invalid.json")
	require.NoError(t, os.WriteFile(invalidPath, []byte(`{"not":"a frame"}`), 0o600))
	config := ImportConfig{
		InputDir: input, ProcessedDir: processed, QuarantineDir: quarantineDir,
		LedgerPath: filepath.Join(root, "processed.ledger"),
	}
	require.NoError(t, importAvailable(t.Context(), config, publicKey, nil, nil, map[string]struct{}{}))
	_, err = os.Stat(filepath.Join(quarantineDir, filepath.Base(invalidPath)))
	require.NoError(t, err)
	errorsLog, err := os.ReadFile(config.LedgerPath + ".errors")
	require.NoError(t, err)
	require.Contains(t, string(errorsLog), "verify frame")
}

func TestImportQuarantinesExpiredFrame(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	root := t.TempDir()
	input := filepath.Join(root, "input")
	processed := filepath.Join(root, "processed")
	quarantineDir := filepath.Join(root, "quarantine")
	require.NoError(t, os.MkdirAll(input, 0o700))
	require.NoError(t, os.MkdirAll(processed, 0o700))
	require.NoError(t, os.MkdirAll(quarantineDir, 0o700))
	createdAt := time.Now().Add(-2 * time.Hour).UTC()
	frame, err := signFrame(1, hmessaging.Message{
		ID: "expired-command", Subject: "po.v1.orgs.test.runners.runner.commands",
		Data: []byte(`{"protocol_version":"1"}`), CreatedAt: createdAt, ExpiresAt: createdAt.Add(time.Hour),
	}, privateKey)
	require.NoError(t, err)
	data, err := json.Marshal(frame)
	require.NoError(t, err)
	name := "0001-expired.json"
	require.NoError(t, os.WriteFile(filepath.Join(input, name), data, 0o600))
	config := ImportConfig{
		InputDir: input, ProcessedDir: processed, QuarantineDir: quarantineDir,
		LedgerPath: filepath.Join(root, "processed.ledger"),
	}
	require.NoError(t, importAvailable(t.Context(), config, publicKey, nil, nil, map[string]struct{}{}))
	_, err = os.Stat(filepath.Join(quarantineDir, name))
	require.NoError(t, err)
}

func TestImportQuarantinesOversizedFrameBeforeReadingIt(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	root := t.TempDir()
	input := filepath.Join(root, "input")
	processed := filepath.Join(root, "processed")
	quarantineDir := filepath.Join(root, "quarantine")
	require.NoError(t, os.MkdirAll(input, 0o700))
	require.NoError(t, os.MkdirAll(processed, 0o700))
	require.NoError(t, os.MkdirAll(quarantineDir, 0o700))
	name := "0001-oversized.json"
	limit := int64(1)
	file, err := os.Create(filepath.Join(input, name))
	require.NoError(t, err)
	require.NoError(t, file.Truncate(maxFrameBytes(limit)+1))
	require.NoError(t, file.Close())
	config := ImportConfig{
		InputDir: input, ProcessedDir: processed, QuarantineDir: quarantineDir,
		LedgerPath: filepath.Join(root, "processed.ledger"), MaxAttachmentBytes: limit,
	}
	require.NoError(t, importAvailable(t.Context(), config, publicKey, nil, nil, map[string]struct{}{}))
	_, err = os.Stat(filepath.Join(quarantineDir, name))
	require.NoError(t, err)
}

func TestFrameRejectsWrongSigningKey(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	otherPublicKey, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	frame, err := signFrame(1, hmessaging.Message{
		ID: "event-1", Subject: "po.v1.orgs.test.runners.runner.events.job-created",
		Data: []byte(`{}`), CreatedAt: time.Now().UTC(),
	}, privateKey)
	require.NoError(t, err)
	require.ErrorContains(t, verifyFrame(frame, otherPublicKey), "signature")
}

func TestFrameRejectsTamperedAttachment(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	attachment := &Attachment{Bucket: "PO_RUNNER_BUNDLES", Key: "org/deployment", Data: []byte("bundle")}
	attachment.PayloadSHA256 = payloadHash(attachment.Data)
	frame, err := signFrameWithAttachment(1, hmessaging.Message{
		ID: "command-with-bundle", Subject: "po.v1.orgs.org.runners.runner.commands",
		Data: []byte(`{"command":"create-job"}`), CreatedAt: time.Now().UTC(),
	}, attachment, privateKey)
	require.NoError(t, err)
	require.NoError(t, verifyFrame(frame, publicKey))
	frame.Attachment.Data[0] ^= 1
	require.ErrorContains(t, verifyFrame(frame, publicKey), "attachment payload checksum")
}

func TestBundleAttachmentReferenceIsIsolatedToCommandDeployment(t *testing.T) {
	command := hmessaging.CommandEnvelope{
		ProtocolVersion: hmessaging.ProtocolVersionV1, CommandID: "create-job:deployment-a:1",
		OrganizationID: "org-a", RunnerID: "runner-a", DeploymentID: "deployment-a",
		Type: "create-job", CreatedAt: time.Now().UTC(), Payload: json.RawMessage(`{}`),
	}
	data, err := json.Marshal(command)
	require.NoError(t, err)
	bucket, key, required, err := attachmentReference(ExportConfig{AttachObject: "bundle", BundleBucket: "PO_RUNNER_BUNDLES"}, hmessaging.Message{
		ID: command.CommandID, Subject: "po.v1.orgs.org-a.runners.runner-a.commands", Data: data, CreatedAt: command.CreatedAt,
	})
	require.NoError(t, err)
	require.True(t, required)
	require.Equal(t, "PO_RUNNER_BUNDLES", bucket)
	require.Equal(t, "org-a/deployment-a", key)
	require.NotEqual(t, "org-b/deployment-b", key)
}

func TestAttachmentInvariantRejectsDifferentDeployment(t *testing.T) {
	command := hmessaging.CommandEnvelope{
		ProtocolVersion: hmessaging.ProtocolVersionV1, CommandID: "create-job:deployment-a:1",
		OrganizationID: "org-a", RunnerID: "runner-a", DeploymentID: "deployment-a",
		Type: "create-job", CreatedAt: time.Now().UTC(), Payload: json.RawMessage(`{}`),
	}
	data, err := json.Marshal(command)
	require.NoError(t, err)
	message := hmessaging.Message{ID: command.CommandID, Subject: "po.v1.orgs.org-a.runners.runner-a.commands", Data: data, CreatedAt: command.CreatedAt}
	attachment := &Attachment{Bucket: "PO_RUNNER_BUNDLES", Key: "org-b/deployment-b", Data: []byte("other tenant")}
	attachment.PayloadSHA256 = payloadHash(attachment.Data)
	require.ErrorContains(t, validateAttachmentInvariant("bundle", message, attachment), "does not match message reference")
}
