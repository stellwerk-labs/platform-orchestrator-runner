package natstransport

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/golib/hnats"
)

const (
	defaultAckWait    = 2 * time.Minute
	defaultMaxDeliver = 10
)

type Config struct {
	URL             string
	Token           string
	CredentialsFile string
	CAFile          string
	ClientCertFile  string
	ClientKeyFile   string
	OutboxDir       string
	Name            string
	ConnectTimeout  time.Duration
}

func Connect(config Config) (*nats.Conn, error) {
	if strings.TrimSpace(config.URL) == "" {
		return nil, errors.New("NATS_URL must not be empty")
	}
	var tlsConfig *tls.Config
	if config.CAFile != "" || config.ClientCertFile != "" || config.ClientKeyFile != "" {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if config.CAFile != "" {
		ca, err := os.ReadFile(config.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read NATS CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errors.New("NATS_CA_FILE does not contain a PEM certificate")
		}
		tlsConfig.RootCAs = pool
	}
	if config.ClientCertFile != "" || config.ClientKeyFile != "" {
		if config.ClientCertFile == "" || config.ClientKeyFile == "" {
			return nil, errors.New("both NATS_CLIENT_CERT_FILE and NATS_CLIENT_KEY_FILE are required for mTLS")
		}
		certificate, err := tls.LoadX509KeyPair(config.ClientCertFile, config.ClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load NATS client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}
	connectTimeout := config.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = 10 * time.Second
	}
	return hnats.Connect(hnats.ConnectionConfig{
		URLs: strings.Split(config.URL, ","), Name: config.Name, Token: config.Token,
		CredentialsFile: config.CredentialsFile, TLSConfig: tlsConfig,
		ConnectTimeout: connectTimeout, ReconnectWait: time.Second, MaxReconnects: -1,
	}, nil)
}

type Publisher struct {
	publisher hmessaging.Publisher
	outboxDir string
	mu        sync.Mutex
}

func NewPublisher(connection *nats.Conn, outboxDir string) (*Publisher, error) {
	js, err := hnats.NewJetStream(connection)
	if err != nil {
		return nil, fmt.Errorf("create JetStream publisher: %w", err)
	}
	publisher := &Publisher{publisher: hnats.NewPublisher(js, "", nil), outboxDir: outboxDir}
	if outboxDir != "" {
		if err := os.MkdirAll(outboxDir, 0o700); err != nil {
			return nil, fmt.Errorf("create NATS outbox: %w", err)
		}
	}
	return publisher, nil
}

// PublishMessage also persists the message when the process cannot establish
// its initial NATS connection. Short-lived edge Jobs cannot rely on a later
// reconnect callback because they may exit immediately after publishing.
func PublishMessage(ctx context.Context, config Config, message hmessaging.Message) error {
	connection, err := Connect(config)
	if err == nil {
		defer connection.Close()
		publisher, publisherErr := NewPublisher(connection, config.OutboxDir)
		if publisherErr == nil {
			return publisher.Publish(ctx, message)
		}
		err = publisherErr
	}
	if config.OutboxDir == "" {
		return err
	}
	publisher := &Publisher{outboxDir: config.OutboxDir}
	if err := os.MkdirAll(config.OutboxDir, 0o700); err != nil {
		return err
	}
	if spoolErr := publisher.spool(message); spoolErr != nil {
		return fmt.Errorf("connect or initialize NATS publisher: %w; persist outbox message: %v", err, spoolErr)
	}
	return nil
}

func (p *Publisher) Publish(ctx context.Context, message hmessaging.Message) error {
	if err := message.Validate(); err != nil {
		return err
	}
	if err := p.publish(ctx, message); err != nil {
		if p.outboxDir == "" {
			return err
		}
		if spoolErr := p.spool(message); spoolErr != nil {
			return fmt.Errorf("publish NATS message: %w; persist outbox message: %v", err, spoolErr)
		}
		slog.WarnContext(ctx, "NATS unavailable, message persisted to outbound spool", "message_id", message.ID, "subject", message.Subject)
	}
	return nil
}

func (p *Publisher) publish(ctx context.Context, message hmessaging.Message) error {
	return p.publisher.Publish(ctx, message)
}

func (p *Publisher) spool(message hmessaging.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(message.ID))
	name := hex.EncodeToString(sum[:]) + ".json"
	temporary, err := os.CreateTemp(p.outboxDir, ".pending-*")
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
	if err := os.Rename(temporaryName, filepath.Join(p.outboxDir, name)); err != nil {
		return err
	}
	return syncDir(p.outboxDir)
}

func (p *Publisher) Flush(ctx context.Context) error {
	if p.outboxDir == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	entries, err := os.ReadDir(p.outboxDir)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(p.outboxDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var message hmessaging.Message
		if err := json.Unmarshal(data, &message); err != nil {
			return fmt.Errorf("decode outbound message %s: %w", entry.Name(), err)
		}
		if err := p.publish(ctx, message); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		if err := syncDir(p.outboxDir); err != nil {
			return err
		}
	}
	return nil
}

type Delivery struct {
	Message  hmessaging.Message
	Attempts uint64
	ack      func() error
	nak      func(time.Duration) error
	term     func() error
}

func NewDelivery(message hmessaging.Message, attempts uint64, ack func() error, nak func(time.Duration) error, term func() error) Delivery {
	return Delivery{Message: message, Attempts: attempts, ack: ack, nak: nak, term: term}
}

func (d Delivery) Ack() error                    { return d.ack() }
func (d Delivery) Nak(delay time.Duration) error { return d.nak(delay) }
func (d Delivery) Term() error                   { return d.term() }

type Consumer struct {
	subscription *nats.Subscription
}

func NewRunnerConsumer(connection *nats.Conn, organizationID, runnerID string, bootstrapStreams bool) (*Consumer, error) {
	subject, err := hmessaging.RunnerCommandSubject(organizationID, runnerID)
	if err != nil {
		return nil, err
	}
	if bootstrapStreams {
		modern, err := hnats.NewJetStream(connection)
		if err != nil {
			return nil, fmt.Errorf("create JetStream consumer: %w", err)
		}
		if err := hnats.EnsureStandardStreams(context.Background(), modern, 1); err != nil {
			return nil, err
		}
	}
	js, err := connection.JetStream()
	if err != nil {
		return nil, fmt.Errorf("create legacy JetStream consumer adapter: %w", err)
	}
	durable := durableName(organizationID, runnerID)
	subscription, err := js.PullSubscribe(subject, durable,
		nats.BindStream(hmessaging.RunnerCommandsStreamName),
		nats.ManualAck(),
		nats.AckExplicit(),
		nats.AckWait(defaultAckWait),
		nats.MaxDeliver(defaultMaxDeliver),
	)
	if err != nil {
		return nil, fmt.Errorf("create runner command consumer: %w", err)
	}
	return &Consumer{subscription: subscription}, nil
}

func NewBoundConsumer(connection *nats.Conn, stream, subject, durable string) (*Consumer, error) {
	if stream == "" || subject == "" || durable == "" {
		return nil, errors.New("stream, subject, and durable consumer name are required")
	}
	js, err := connection.JetStream()
	if err != nil {
		return nil, fmt.Errorf("create JetStream consumer: %w", err)
	}
	subscription, err := js.PullSubscribe(subject, durable,
		nats.BindStream(stream), nats.ManualAck(), nats.AckExplicit(),
		nats.AckWait(defaultAckWait), nats.MaxDeliver(defaultMaxDeliver),
	)
	if err != nil {
		return nil, fmt.Errorf("bind durable consumer %q on stream %q: %w", durable, stream, err)
	}
	return &Consumer{subscription: subscription}, nil
}

func durableName(organizationID, runnerID string) string {
	sum := sha256.Sum256([]byte(organizationID + "\x00" + runnerID))
	return "runner-" + hex.EncodeToString(sum[:12])
}

func (c *Consumer) Fetch(ctx context.Context) (Delivery, error) {
	fetchContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	messages, err := c.subscription.Fetch(1, nats.Context(fetchContext))
	if err != nil {
		return Delivery{}, err
	}
	message := messages[0]
	metadata, err := message.Metadata()
	if err != nil {
		return Delivery{}, err
	}
	createdAt := metadata.Timestamp
	if value := message.Header.Get(hmessaging.HeaderCreatedAt); value != "" {
		if parsed, parseErr := time.Parse(time.RFC3339Nano, value); parseErr == nil {
			createdAt = parsed
		}
	}
	var expiresAt time.Time
	if value := message.Header.Get(hmessaging.HeaderExpiresAt); value != "" {
		expiresAt, _ = time.Parse(time.RFC3339Nano, value)
	}
	header := make(hmessaging.Header, len(message.Header))
	for key, values := range message.Header {
		header[key] = append([]string(nil), values...)
	}
	messageID := message.Header.Get(hmessaging.HeaderMessageID)
	if messageID == "" {
		messageID = fmt.Sprintf("stream:%s:%d", metadata.Stream, metadata.Sequence.Stream)
	}
	return Delivery{
		Message: hmessaging.Message{
			ID:        messageID,
			Subject:   message.Subject,
			Data:      message.Data,
			Header:    header,
			CreatedAt: createdAt,
			ExpiresAt: expiresAt,
		},
		Attempts: metadata.NumDelivered,
		ack:      func() error { return message.Ack() },
		nak:      func(delay time.Duration) error { return message.NakWithDelay(delay) },
		term:     func() error { return message.Term() },
	}, nil
}
