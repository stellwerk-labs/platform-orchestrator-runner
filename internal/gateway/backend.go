package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hmessaging"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/natstransport"
)

const (
	runnerBundlesBucket = "PO_RUNNER_BUNDLES"
	runnerLogsBucket    = "PO_RUNNER_LOGS"
)

var ErrNoCommand = errors.New("no runner command available")

type CommandDelivery struct {
	Message        hmessaging.Message
	Attempts       uint64
	AckSubject     string
	StreamSequence uint64
}

type StoredObject struct {
	Size uint64
}

type Backend interface {
	FetchCommand(ctx context.Context, organizationID, runnerID string) (CommandDelivery, error)
	Acknowledge(ctx context.Context, ackSubject string) error
	Retry(ctx context.Context, ackSubject string, delay time.Duration) error
	Reject(ctx context.Context, delivery CommandDelivery, cause error) error
	RejectReceipt(ctx context.Context, ackSubject string, streamSequence uint64, cause error) error
	GetBundle(ctx context.Context, key string) (io.ReadCloser, error)
	PutLog(ctx context.Context, key string, content io.Reader) (StoredObject, error)
	DeleteLog(ctx context.Context, key string) error
	Publish(ctx context.Context, message hmessaging.Message) error
}

type NATSBackend struct {
	connection *nats.Conn
	publisher  *natstransport.Publisher
	bundles    nats.ObjectStore
	logs       nats.ObjectStore
	jetStream  nats.JetStreamContext
	consumers  sync.Map
}

type cachedConsumer struct {
	mu       sync.Mutex
	consumer *natstransport.Consumer
}

func NewNATSBackend(connection *nats.Conn) (*NATSBackend, error) {
	if connection == nil {
		return nil, errors.New("NATS connection is required")
	}
	publisher, err := natstransport.NewPublisher(connection)
	if err != nil {
		return nil, err
	}
	js, err := connection.JetStream()
	if err != nil {
		return nil, fmt.Errorf("open JetStream context: %w", err)
	}
	bundles, err := js.ObjectStore(runnerBundlesBucket)
	if err != nil {
		return nil, fmt.Errorf("bind runner bundle Object Store: %w", err)
	}
	logs, err := js.ObjectStore(runnerLogsBucket)
	if err != nil {
		return nil, fmt.Errorf("bind runner log Object Store: %w", err)
	}
	return &NATSBackend{connection: connection, publisher: publisher, bundles: bundles, logs: logs, jetStream: js}, nil
}

func (b *NATSBackend) FetchCommand(ctx context.Context, organizationID, runnerID string) (CommandDelivery, error) {
	cacheKey := organizationID + "\x00" + runnerID
	value, ok := b.consumers.Load(cacheKey)
	if !ok {
		consumer, err := natstransport.NewRunnerConsumer(b.connection, organizationID, runnerID, false)
		if err != nil {
			return CommandDelivery{}, err
		}
		value, _ = b.consumers.LoadOrStore(cacheKey, &cachedConsumer{consumer: consumer})
	}
	entry := value.(*cachedConsumer)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	delivery, err := entry.consumer.Fetch(ctx)
	if errors.Is(err, nats.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return CommandDelivery{}, ErrNoCommand
	}
	if err != nil {
		return CommandDelivery{}, err
	}
	return CommandDelivery{
		Message: delivery.Message, Attempts: delivery.Attempts, AckSubject: delivery.AckSubject,
		StreamSequence: delivery.StreamSequence,
	}, nil
}

func (b *NATSBackend) Acknowledge(ctx context.Context, ackSubject string) error {
	return b.publishAcknowledgement(ctx, ackSubject, []byte("+ACK"))
}

func (b *NATSBackend) Retry(ctx context.Context, ackSubject string, delay time.Duration) error {
	payload := []byte("-NAK")
	if delay > 0 {
		payload = []byte(fmt.Sprintf("-NAK {\"delay\": %d}", delay.Nanoseconds()))
	}
	return b.publishAcknowledgement(ctx, ackSubject, payload)
}

func (b *NATSBackend) Reject(ctx context.Context, delivery CommandDelivery, cause error) error {
	subject, err := hmessaging.DeadLetterSubject(delivery.Message.Subject)
	if err != nil {
		return err
	}
	dlq := delivery.Message.Clone()
	dlq.ID += ":gateway-dlq"
	dlq.Subject = subject
	dlq.ExpiresAt = time.Time{}
	if dlq.Header == nil {
		dlq.Header = hmessaging.Header{}
	}
	dlq.Header.Set("Po-Dead-Letter-Reason", cause.Error())
	if err := b.publisher.Publish(ctx, dlq); err != nil {
		return err
	}
	return b.publishAcknowledgement(ctx, delivery.AckSubject, []byte("+TERM"))
}

func (b *NATSBackend) RejectReceipt(ctx context.Context, ackSubject string, streamSequence uint64, cause error) error {
	raw, err := b.jetStream.GetMsg(hmessaging.RunnerCommandsStreamName, streamSequence, nats.Context(ctx))
	if err != nil {
		return fmt.Errorf("load rejected runner command: %w", err)
	}
	header := make(hmessaging.Header, len(raw.Header))
	for key, values := range raw.Header {
		header[key] = append([]string(nil), values...)
	}
	messageID := raw.Header.Get(hmessaging.HeaderMessageID)
	if messageID == "" {
		messageID = fmt.Sprintf("stream:%s:%d", hmessaging.RunnerCommandsStreamName, streamSequence)
	}
	return b.Reject(ctx, CommandDelivery{
		Message:    hmessaging.Message{ID: messageID, Subject: raw.Subject, Data: raw.Data, Header: header, CreatedAt: raw.Time},
		AckSubject: ackSubject, StreamSequence: streamSequence,
	}, cause)
}

func (b *NATSBackend) publishAcknowledgement(ctx context.Context, subject string, payload []byte) error {
	if subject == "" {
		return errors.New("JetStream acknowledgement subject is empty")
	}
	if err := b.connection.Publish(subject, payload); err != nil {
		return err
	}
	deadline, ok := ctx.Deadline()
	timeout := 5 * time.Second
	if ok && time.Until(deadline) < timeout {
		timeout = time.Until(deadline)
	}
	if timeout <= 0 {
		return ctx.Err()
	}
	return b.connection.FlushTimeout(timeout)
}

func (b *NATSBackend) GetBundle(_ context.Context, key string) (io.ReadCloser, error) {
	return b.bundles.Get(key)
}

func (b *NATSBackend) PutLog(_ context.Context, key string, content io.Reader) (StoredObject, error) {
	info, err := b.logs.Put(&nats.ObjectMeta{Name: key}, content)
	if err != nil {
		return StoredObject{}, err
	}
	return StoredObject{Size: info.Size}, nil
}

func (b *NATSBackend) DeleteLog(_ context.Context, key string) error {
	return b.logs.Delete(key)
}

func (b *NATSBackend) Publish(ctx context.Context, message hmessaging.Message) error {
	return b.publisher.Publish(ctx, message)
}

func decodeCommandEnvelope(message hmessaging.Message) (hmessaging.CommandEnvelope, error) {
	var envelope hmessaging.CommandEnvelope
	if err := json.Unmarshal(message.Data, &envelope); err != nil {
		return hmessaging.CommandEnvelope{}, err
	}
	return envelope, nil
}
