package natstransport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hmessaging"
)

const RunnerLogsBucket = "PO_RUNNER_LOGS"

// LogObject is the complete durable unit for a runner log upload. Keeping the
// encrypted bytes and ready event together prevents an edge Job from losing
// logs when its NATS connection disappears during shutdown.
type LogObject struct {
	Bucket       string             `json:"bucket"`
	Key          string             `json:"key"`
	EncryptedLog []byte             `json:"encrypted_log"`
	ReadyEvent   hmessaging.Message `json:"ready_event"`
}

func PublishLogObject(ctx context.Context, config Config, object LogObject, bootstrapObjectStore bool) error {
	connection, err := Connect(config)
	if err == nil {
		defer connection.Close()
		err = putLogObject(ctx, connection, config.OutboxDir, object, bootstrapObjectStore)
	}
	if err == nil {
		return nil
	}
	if config.OutboxDir == "" {
		return err
	}
	if spoolErr := spoolLogObject(config.OutboxDir, object); spoolErr != nil {
		return fmt.Errorf("publish log object: %w; persist log object spool: %v", err, spoolErr)
	}
	return nil
}

func FlushLogObjects(ctx context.Context, connection *nats.Conn, outboxDir string) error {
	if outboxDir == "" {
		return nil
	}
	dir := filepath.Join(outboxDir, "logs")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var object LogObject
		if err := json.Unmarshal(data, &object); err != nil {
			return fmt.Errorf("decode log object spool %s: %w", entry.Name(), err)
		}
		if err := putLogObject(ctx, connection, outboxDir, object, false); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		if err := syncDir(dir); err != nil {
			return err
		}
	}
	return nil
}

func putLogObject(ctx context.Context, connection *nats.Conn, outboxDir string, object LogObject, bootstrap bool) error {
	if object.Bucket == "" || object.Key == "" || len(object.EncryptedLog) == 0 {
		return errors.New("log object bucket, key, and encrypted bytes are required")
	}
	if err := object.ReadyEvent.Validate(); err != nil {
		return fmt.Errorf("validate log ready event: %w", err)
	}
	js, err := connection.JetStream()
	if err != nil {
		return err
	}
	store, err := js.ObjectStore(object.Bucket)
	if errors.Is(err, nats.ErrBucketNotFound) && bootstrap {
		store, err = js.CreateObjectStore(&nats.ObjectStoreConfig{Bucket: object.Bucket, Storage: nats.FileStorage, Replicas: 1})
	}
	if err != nil {
		return fmt.Errorf("open pre-provisioned NATS Object Store bucket %s: %w", object.Bucket, err)
	}
	if _, err := store.PutBytes(object.Key, object.EncryptedLog); err != nil {
		return err
	}
	publisher, err := NewPublisher(connection, outboxDir)
	if err != nil {
		return err
	}
	return publisher.Publish(ctx, object.ReadyEvent)
}

func spoolLogObject(outboxDir string, object LogObject) error {
	data, err := json.Marshal(object)
	if err != nil {
		return err
	}
	dir := filepath.Join(outboxDir, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(object.Bucket + "\x00" + object.Key))
	path := filepath.Join(dir, hex.EncodeToString(sum[:])+".json")
	temporary, err := os.CreateTemp(dir, ".pending-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
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
	if err := os.Rename(temporaryName, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(path string) error {
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
