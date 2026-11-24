package natstransport

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
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
}

func NewPublisher(connection *nats.Conn) (*Publisher, error) {
	js, err := hnats.NewJetStream(connection)
	if err != nil {
		return nil, fmt.Errorf("create JetStream publisher: %w", err)
	}
	return &Publisher{publisher: hnats.NewPublisher(js, "", nil)}, nil
}

func (p *Publisher) Publish(ctx context.Context, message hmessaging.Message) error {
	if err := message.Validate(); err != nil {
		return err
	}
	return p.publisher.Publish(ctx, message)
}

type Delivery struct {
	Message        hmessaging.Message
	Attempts       uint64
	AckSubject     string
	StreamSequence uint64
	ack            func() error
	nak            func(time.Duration) error
	term           func() error
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
	native := strings.HasPrefix(runnerID, hmessaging.NativeRunnerTokenPrefix)
	if native {
		if _, err := hmessaging.NativeRunnerToken(strings.TrimPrefix(runnerID, hmessaging.NativeRunnerTokenPrefix)); err != nil {
			return nil, err
		}
		version := connection.ConnectedServerVersion()
		var major, minor, patch int
		_, err := fmt.Sscanf(version, "%d.%d.%d", &major, &minor, &patch)
		if err != nil || major < 0 || minor < 0 || patch < 0 || version != fmt.Sprintf("%d.%d.%d", major, minor, patch) || major < 2 || major == 2 && minor < 10 {
			return nil, errors.New("native consumer requires stable NATS 2.10.0 or later")
		}
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
	options := []nats.SubOpt{nats.BindStream(hmessaging.RunnerCommandsStreamName), nats.ManualAck(), nats.AckExplicit(), nats.AckWait(defaultAckWait), nats.MaxDeliver(defaultMaxDeliver)}
	if native {
		modern, err := hnats.NewJetStream(connection)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// CreateConsumer refuses differing existing configuration instead of updating it.
		_, err = modern.CreateConsumer(ctx, hmessaging.RunnerCommandsStreamName, jetstream.ConsumerConfig{Durable: durable, Description: "stellwerk.runner.native/v1", FilterSubject: subject, AckPolicy: jetstream.AckExplicitPolicy, AckWait: defaultAckWait, MaxDeliver: defaultMaxDeliver})
		if err != nil {
			return nil, fmt.Errorf("native consumer configuration refused: %w", err)
		}
		options = []nats.SubOpt{nats.Bind(hmessaging.RunnerCommandsStreamName, durable), nats.ManualAck()}
	}
	subscription, err := js.PullSubscribe(subject, durable, options...)
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
		Attempts:       metadata.NumDelivered,
		AckSubject:     message.Reply,
		StreamSequence: metadata.Sequence.Stream,
		ack:            func() error { return message.Ack() },
		nak:            func(delay time.Duration) error { return message.NakWithDelay(delay) },
		term:           func() error { return message.Term() },
	}, nil
}
