package diode

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/natstransport"
)

const FrameVersion = "1"

type ExportConfig struct {
	NATS               natstransport.Config
	Stream             string
	Subject            string
	Durable            string
	OutputDir          string
	LedgerPath         string
	SigningKey         string
	AttachObject       string
	BundleBucket       string
	MaxAttachmentBytes int64
}

type ImportConfig struct {
	NATS               natstransport.Config
	InputDir           string
	ProcessedDir       string
	QuarantineDir      string
	LedgerPath         string
	VerifyKey          string
	PollInterval       time.Duration
	MaxAttachmentBytes int64
	ExpectedAttachment string
}

type Attachment struct {
	Bucket        string `json:"bucket"`
	Key           string `json:"key"`
	Data          []byte `json:"data"`
	PayloadSHA256 string `json:"payload_sha256"`
}

type unsignedFrame struct {
	Version       string             `json:"version"`
	Sequence      uint64             `json:"sequence"`
	Message       hmessaging.Message `json:"message"`
	PayloadSHA256 string             `json:"payload_sha256"`
	Attachment    *Attachment        `json:"attachment,omitempty"`
}

type Frame struct {
	unsignedFrame
	Signature string `json:"signature"`
}

func RunExporter(ctx context.Context, config ExportConfig) error {
	if err := requireAbsolutePaths(config.OutputDir, config.LedgerPath, config.SigningKey); err != nil {
		return err
	}
	privateKey, err := loadPrivateKey(config.SigningKey)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(config.OutputDir, 0o700); err != nil {
		return err
	}
	sequence, err := readSequence(config.LedgerPath)
	if err != nil {
		return err
	}
	connection, err := natstransport.Connect(config.NATS)
	if err != nil {
		return err
	}
	defer connection.Close()
	if config.MaxAttachmentBytes <= 0 {
		config.MaxAttachmentBytes = 16 * 1024 * 1024
	}
	consumer, err := natstransport.NewBoundConsumer(connection, config.Stream, config.Subject, config.Durable)
	if err != nil {
		return err
	}
	for {
		delivery, err := consumer.Fetch(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		sequence++
		attachment, err := loadAttachment(connection, config, delivery.Message)
		if err != nil {
			_ = delivery.Nak(5 * time.Second)
			continue
		}
		frame, err := signFrameWithAttachment(sequence, delivery.Message, attachment, privateKey)
		if err != nil {
			_ = delivery.Nak(time.Second)
			continue
		}
		name := fmt.Sprintf("%020d-%s.json", sequence, shortHash(delivery.Message.ID))
		if err := atomicJSON(filepath.Join(config.OutputDir, name), frame); err != nil {
			_ = delivery.Nak(5 * time.Second)
			continue
		}
		if err := writeSequence(config.LedgerPath, sequence); err != nil {
			_ = delivery.Nak(5 * time.Second)
			continue
		}
		if err := delivery.Ack(); err != nil {
			return err
		}
	}
}

func RunImporter(ctx context.Context, config ImportConfig) error {
	paths := []string{config.InputDir, config.ProcessedDir, config.LedgerPath, config.VerifyKey}
	if config.QuarantineDir != "" {
		paths = append(paths, config.QuarantineDir)
	}
	if err := requireAbsolutePaths(paths...); err != nil {
		return err
	}
	publicKey, err := loadPublicKey(config.VerifyKey)
	if err != nil {
		return err
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 500 * time.Millisecond
	}
	if config.MaxAttachmentBytes <= 0 {
		config.MaxAttachmentBytes = 16 * 1024 * 1024
	}
	if config.QuarantineDir == "" {
		config.QuarantineDir = filepath.Join(config.ProcessedDir, "quarantine")
	}
	if err := os.MkdirAll(config.InputDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(config.ProcessedDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(config.QuarantineDir, 0o700); err != nil {
		return err
	}
	processed, err := readProcessed(config.LedgerPath)
	if err != nil {
		return err
	}
	connection, err := natstransport.Connect(config.NATS)
	if err != nil {
		return err
	}
	defer connection.Close()
	publisher, err := natstransport.NewPublisher(connection, "")
	if err != nil {
		return err
	}
	ticker := time.NewTicker(config.PollInterval)
	defer ticker.Stop()
	for {
		if err := importAvailable(ctx, config, publicKey, connection, publisher, processed); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func importAvailable(ctx context.Context, config ImportConfig, publicKey ed25519.PublicKey, connection *nats.Conn, publisher *natstransport.Publisher, processed map[string]struct{}) error {
	if config.MaxAttachmentBytes <= 0 {
		config.MaxAttachmentBytes = 16 * 1024 * 1024
	}
	entries, err := os.ReadDir(config.InputDir)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(config.InputDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxFrameBytes(config.MaxAttachmentBytes) {
			if quarantineErr := quarantine(config, path, entry.Name(), fmt.Errorf("frame exceeds %d-byte encoded size limit", maxFrameBytes(config.MaxAttachmentBytes))); quarantineErr != nil {
				return quarantineErr
			}
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var frame Frame
		if err := json.Unmarshal(data, &frame); err != nil {
			if quarantineErr := quarantine(config, path, entry.Name(), fmt.Errorf("decode frame: %w", err)); quarantineErr != nil {
				return quarantineErr
			}
			continue
		}
		if err := verifyFrame(frame, publicKey); err != nil {
			if quarantineErr := quarantine(config, path, entry.Name(), fmt.Errorf("verify frame: %w", err)); quarantineErr != nil {
				return quarantineErr
			}
			continue
		}
		if !frame.Message.ExpiresAt.IsZero() && !time.Now().Before(frame.Message.ExpiresAt) {
			if quarantineErr := quarantine(config, path, entry.Name(), errors.New("message expired before protected-side import")); quarantineErr != nil {
				return quarantineErr
			}
			continue
		}
		if err := validateAttachmentInvariant(config.ExpectedAttachment, frame.Message, frame.Attachment); err != nil {
			if quarantineErr := quarantine(config, path, entry.Name(), err); quarantineErr != nil {
				return quarantineErr
			}
			continue
		}
		if frame.Attachment != nil {
			if int64(len(frame.Attachment.Data)) > config.MaxAttachmentBytes {
				if quarantineErr := quarantine(config, path, entry.Name(), errors.New("attachment exceeds configured size limit")); quarantineErr != nil {
					return quarantineErr
				}
				continue
			}
			if err := storeAttachment(connection, *frame.Attachment); err != nil {
				return err
			}
		}
		if _, exists := processed[frame.Message.ID]; !exists {
			if err := publisher.Publish(ctx, frame.Message); err != nil {
				return err
			}
			if err := appendProcessed(config.LedgerPath, frame.Message.ID); err != nil {
				return err
			}
			processed[frame.Message.ID] = struct{}{}
		}
		if err := moveDurably(path, filepath.Join(config.ProcessedDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func maxFrameBytes(maxAttachmentBytes int64) int64 {
	// Attachments are base64-encoded by JSON. Twice the binary limit leaves
	// room for that expansion plus bounded envelope/header metadata.
	return maxAttachmentBytes*2 + 1024*1024
}

func validateAttachmentInvariant(expected string, message hmessaging.Message, attachment *Attachment) error {
	if expected == "" {
		return nil
	}
	bucket, key, required, err := attachmentReference(ExportConfig{AttachObject: expected}, message)
	if err != nil {
		return err
	}
	if required && attachment == nil {
		return fmt.Errorf("%s attachment is required for message", expected)
	}
	if !required && attachment != nil {
		return fmt.Errorf("unexpected %s attachment for message", expected)
	}
	if required && (attachment.Bucket != bucket || attachment.Key != key) {
		return fmt.Errorf("%s attachment reference %s/%s does not match message reference %s/%s", expected, attachment.Bucket, attachment.Key, bucket, key)
	}
	return nil
}

func loadAttachment(connection *nats.Conn, config ExportConfig, message hmessaging.Message) (*Attachment, error) {
	bucket, key, required, err := attachmentReference(config, message)
	if err != nil || !required {
		return nil, err
	}
	js, err := connection.JetStream()
	if err != nil {
		return nil, err
	}
	store, err := js.ObjectStore(bucket)
	if err != nil {
		return nil, err
	}
	result, err := store.Get(key)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(result, config.MaxAttachmentBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > config.MaxAttachmentBytes {
		return nil, fmt.Errorf("object attachment %s/%s exceeds %d bytes", bucket, key, config.MaxAttachmentBytes)
	}
	return &Attachment{Bucket: bucket, Key: key, Data: data, PayloadSHA256: payloadHash(data)}, nil
}

func attachmentReference(config ExportConfig, message hmessaging.Message) (string, string, bool, error) {
	var bucket, key string
	switch config.AttachObject {
	case "":
		return "", "", false, nil
	case "bundle":
		var command hmessaging.CommandEnvelope
		if err := json.Unmarshal(message.Data, &command); err != nil {
			return "", "", false, err
		}
		if command.Type != "create-job" {
			return "", "", false, nil
		}
		bucket = config.BundleBucket
		if bucket == "" {
			bucket = "PO_RUNNER_BUNDLES"
		}
		key = command.OrganizationID + "/" + command.DeploymentID
	case "log":
		var event hmessaging.EventEnvelope
		if err := json.Unmarshal(message.Data, &event); err != nil {
			return "", "", false, err
		}
		if event.Type != "log-object-ready" {
			return "", "", false, nil
		}
		var reference struct {
			Bucket string `json:"bucket"`
			Key    string `json:"key"`
		}
		if err := json.Unmarshal(event.Payload, &reference); err != nil {
			return "", "", false, err
		}
		bucket, key = reference.Bucket, reference.Key
	default:
		return "", "", false, fmt.Errorf("unsupported diode attachment kind %q", config.AttachObject)
	}
	if bucket == "" || key == "" {
		return "", "", false, errors.New("object attachment reference is incomplete")
	}
	return bucket, key, true, nil
}

func storeAttachment(connection *nats.Conn, attachment Attachment) error {
	if attachment.Bucket == "" || attachment.Key == "" || len(attachment.Data) == 0 {
		return errors.New("attachment bucket, key, and data are required")
	}
	if attachment.PayloadSHA256 != payloadHash(attachment.Data) {
		return errors.New("attachment payload checksum mismatch")
	}
	js, err := connection.JetStream()
	if err != nil {
		return err
	}
	store, err := js.ObjectStore(attachment.Bucket)
	if err != nil {
		return err
	}
	_, err = store.PutBytes(attachment.Key, attachment.Data)
	return err
}

func signFrame(sequence uint64, message hmessaging.Message, key ed25519.PrivateKey) (Frame, error) {
	return signFrameWithAttachment(sequence, message, nil, key)
}

func signFrameWithAttachment(sequence uint64, message hmessaging.Message, attachment *Attachment, key ed25519.PrivateKey) (Frame, error) {
	frame := unsignedFrame{Version: FrameVersion, Sequence: sequence, Message: message, PayloadSHA256: payloadHash(message.Data), Attachment: attachment}
	canonical, err := json.Marshal(frame)
	if err != nil {
		return Frame{}, err
	}
	return Frame{unsignedFrame: frame, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, canonical))}, nil
}

func verifyFrame(frame Frame, key ed25519.PublicKey) error {
	if frame.Version != FrameVersion {
		return fmt.Errorf("unsupported frame version %q", frame.Version)
	}
	if frame.PayloadSHA256 != payloadHash(frame.Message.Data) {
		return errors.New("payload checksum mismatch")
	}
	if frame.Attachment != nil && frame.Attachment.PayloadSHA256 != payloadHash(frame.Attachment.Data) {
		return errors.New("attachment payload checksum mismatch")
	}
	signature, err := base64.StdEncoding.DecodeString(frame.Signature)
	if err != nil {
		return errors.New("invalid signature encoding")
	}
	canonical, err := json.Marshal(frame.unsignedFrame)
	if err != nil {
		return err
	}
	if !ed25519.Verify(key, canonical, signature) {
		return errors.New("signature verification failed")
	}
	return frame.Message.Validate()
}

func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("signing key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("signing key is not Ed25519")
	}
	return key, nil
}

func loadPublicKey(path string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("verification key is not PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("verification key is not Ed25519")
	}
	return key, nil
}

func requireAbsolutePaths(paths ...string) error {
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("diode paths must be absolute: %q", path)
		}
	}
	return nil
}

func atomicJSON(path string, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dir, ".frame-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

func readSequence(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.Trim(strings.TrimSpace(string(data)), "\""), 10, 64)
}

func writeSequence(path string, sequence uint64) error {
	return atomicJSON(path, strconv.FormatUint(sequence, 10))
}

func readProcessed(path string) (map[string]struct{}, error) {
	processed := map[string]struct{}{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return processed, nil
	}
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" {
			processed[line] = struct{}{}
		}
	}
	return processed, nil
}

func appendProcessed(path, id string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(id + "\n"); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func quarantine(config ImportConfig, source, name string, cause error) error {
	errorPath := config.LedgerPath + ".errors"
	record, _ := json.Marshal(map[string]interface{}{
		"frame": name, "error": cause.Error(), "quarantined_at": time.Now().UTC(),
	})
	if err := appendProcessed(errorPath, string(record)); err != nil {
		return err
	}
	return moveDurably(source, filepath.Join(config.QuarantineDir, name))
}

func moveDurably(source, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(source)); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(destination))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

func payloadHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}
