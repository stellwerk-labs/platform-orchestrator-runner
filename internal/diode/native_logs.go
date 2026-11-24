package diode

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
)

func nativeLogFrame(message hmessaging.Message, attachment *Attachment) bool {
	var event hmessaging.EventEnvelope
	_ = json.Unmarshal(message.Data, &event)
	return strings.HasPrefix(event.RunnerID, hmessaging.NativeRunnerTokenPrefix) ||
		strings.HasPrefix(message.ID, hmessaging.NativeMessageIDPrefix) ||
		attachment != nil && strings.Contains(attachment.Key, "/native/")
}

// Only attachment-bearing Native logs receive this additional policy. Existing
// trusted non-artifact frames retain their contract; no all-event policy is added.
func validateNativeLogAttachment(expected string, message hmessaging.Message, attachment *Attachment) error {
	if !nativeLogFrame(message, attachment) {
		return nil
	}
	var event hmessaging.EventEnvelope
	_ = json.Unmarshal(message.Data, &event)
	if attachment == nil && event.Type != nativelogs.ReadyEventType {
		return nil
	}
	if expected != "log" || attachment == nil {
		return nativelogs.ErrArtifact
	}
	d, err := nativelogs.VerifyReadyMessage(message)
	if err != nil || attachment.Bucket != nativelogs.Bucket || attachment.Key != d.ObjectKey ||
		attachment.PayloadSHA256 != d.CiphertextSHA256 || d.VerifyCiphertext(attachment.Data) != nil {
		return nativelogs.ErrArtifact
	}
	return nil
}

func storeNativeAttachment(connection *nats.Conn, attachment Attachment) error {
	js, err := connection.JetStream()
	if err != nil {
		return err
	}
	store, err := js.ObjectStore(nativelogs.Bucket)
	if err != nil {
		return err
	}
	reader, err := store.Get(attachment.Key)
	if err == nil {
		defer func() { _ = reader.Close() }()
		existing, readErr := io.ReadAll(io.LimitReader(reader, int64(nativelogs.CiphertextLimit)+1))
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(existing, attachment.Data) {
			return nativelogs.ErrArtifact
		}
		return nil
	}
	if err != nats.ErrObjectNotFound {
		return err
	}
	_, err = store.PutBytes(attachment.Key, attachment.Data)
	return err
}
