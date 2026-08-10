package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path"
	"runtime/debug"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hmessaging"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/diode"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/executor"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/limitedlogsbuffer"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/natstransport"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/utils"
)

type LogsUploader func(ctx context.Context, logsBuffer bytes.Buffer) error

const (
	StandardMode      = "standard"
	RemoteMode        = "remote"
	DiodeExportMode   = "diode-export"
	DiodeImportMode   = "diode-import"
	OutboxFlushMode   = "outbox-flush"
	maxLogsBufferSize = 10 * 1024 * 1024 // 10 MB
)

var (
	buildInfo      *debug.BuildInfo
	natsConnectURL string
)

func init() {
	buildInfo, _ = debug.ReadBuildInfo()
}

func main() {
	ctx := context.Background()
	var exitErrorCode int
	var logsBuffer bytes.Buffer
	uploader, err := mainInner(ctx, limitedlogsbuffer.NewLimitedLogsBuffer(&logsBuffer, maxLogsBufferSize))
	if err != nil {
		slog.ErrorContext(ctx, "execution finished with an error", "err", err.Error())
		exitErrorCode = 1
	} else {
		slog.InfoContext(ctx, "platform-orchestrator runner completed successfully")
	}
	if uploader != nil {
		if err := uploader(ctx, logsBuffer); err != nil {
			slog.ErrorContext(ctx, "failed to upload logs", "err", err.Error())
		}
	}

	os.Exit(exitErrorCode)
}

func mainInner(ctx context.Context, limitedLogsBuffer *limitedlogsbuffer.LimitedLogsBuffer) (LogsUploader, error) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{})))
	slog.InfoContext(ctx, "Starting", "app", path.Base(buildInfo.Main.Path), "version", buildInfo.Main.Version)

	// Define and parse flags
	flag.StringVar(&natsConnectURL, "nats-url", "", "NATS endpoint used for durable runner messages")
	flag.Parse()

	// Get positional arguments after flags
	args := flag.Args()
	var mode string

	if len(args) == 0 {
		// Default to standard mode when no arguments provided only for now to get unstuck, then platform-orchestrator-dp will be updated
		mode = StandardMode
	} else if len(args) == 1 {
		mode = args[0]
	} else {
		return nil, errors.Errorf("invalid number of positional arguments, expected 0 or 1, got %d", len(args))
	}

	switch mode {
	case StandardMode:
		if natsConnectURL != "" {
			return nil, errors.New("standard mode does not allow --nats-url parameter")
		}
		cfg, err := config.GetStandardModeConfiguration()
		if err != nil {
			return nil, errors.Wrap(err, "failed to read standard mode config")
		}
		programLevel := setLogLevel(ctx, cfg.LogLevel)
		apiUpdates, standardModeErr := executor.ExecuteStandardMode(ctx, cfg, runner.CreateRunner, limitedLogsBuffer, programLevel)
		resultErr := publishDeploymentResultToNATS(ctx, cfg, apiUpdates)
		logsUploader := uploadLogsToNATSObjectStore(cfg)
		if resultErr != nil {
			slog.ErrorContext(ctx, "[PLATFORM_ORCHESTRATOR]update-results", "err", resultErr)
			return logsUploader, errors.Wrap(resultErr, "failed to publish deployment results")
		} else if standardModeErr != nil {
			return logsUploader, errors.Wrap(standardModeErr, "failed to execute standard mode")
		} else {
			return logsUploader, nil
		}
	case RemoteMode:
		if cfg, err := config.GetRemoteModeConfiguration(natsConnectURL); err != nil {
			return nil, errors.Wrap(err, "failed to read remote mode config")
		} else {
			programLevel := setLogLevel(ctx, cfg.LogLevel)
			return nil, executor.ExecuteRemoteMode(ctx, cfg, programLevel)
		}
	case DiodeExportMode:
		return nil, diode.RunExporter(ctx, diode.ExportConfig{
			NATS: diodeNATSConfig(), Stream: os.Getenv("DIODE_STREAM"), Subject: os.Getenv("DIODE_SUBJECT"),
			Durable: os.Getenv("DIODE_DURABLE"), OutputDir: os.Getenv("DIODE_OUTPUT_DIR"),
			LedgerPath: os.Getenv("DIODE_LEDGER_PATH"), SigningKey: os.Getenv("DIODE_SIGNING_KEY_FILE"),
			AttachObject: os.Getenv("DIODE_ATTACH_OBJECT"), BundleBucket: os.Getenv("NATS_BUNDLE_BUCKET"),
		})
	case DiodeImportMode:
		return nil, diode.RunImporter(ctx, diode.ImportConfig{
			NATS: diodeNATSConfig(), InputDir: os.Getenv("DIODE_INPUT_DIR"),
			ProcessedDir: os.Getenv("DIODE_PROCESSED_DIR"), LedgerPath: os.Getenv("DIODE_LEDGER_PATH"),
			QuarantineDir: os.Getenv("DIODE_QUARANTINE_DIR"), VerifyKey: os.Getenv("DIODE_VERIFY_KEY_FILE"),
			ExpectedAttachment: os.Getenv("DIODE_EXPECT_ATTACHMENT"),
		})
	case OutboxFlushMode:
		return nil, runOutboxFlusher(ctx, diodeNATSConfig(), os.Getenv("NATS_OUTBOX_DIR"))
	default:
		return nil, errors.Errorf("invalid mode: %s. Must be '%s', '%s', '%s', '%s', or '%s'", mode, StandardMode, RemoteMode, DiodeExportMode, DiodeImportMode, OutboxFlushMode)
	}
}

func runOutboxFlusher(ctx context.Context, natsConfig natstransport.Config, outboxDir string) error {
	natsConfig.OutboxDir = outboxDir
	connection, err := natstransport.Connect(natsConfig)
	if err != nil {
		return err
	}
	defer connection.Close()
	publisher, err := natstransport.NewPublisher(connection, outboxDir)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if err := publisher.Flush(ctx); err != nil {
			slog.WarnContext(ctx, "failed to flush NATS message outbox", "err", err)
		}
		if err := natstransport.FlushLogObjects(ctx, connection, outboxDir); err != nil {
			slog.WarnContext(ctx, "failed to flush NATS log outbox", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func diodeNATSConfig() natstransport.Config {
	return natstransport.Config{
		URL: os.Getenv("NATS_URL"), Token: os.Getenv("NATS_TOKEN"),
		CredentialsFile: os.Getenv("NATS_CREDS_FILE"), CAFile: os.Getenv("NATS_CA_FILE"),
		ClientCertFile: os.Getenv("NATS_CLIENT_CERT_FILE"), ClientKeyFile: os.Getenv("NATS_CLIENT_KEY_FILE"),
		Name: "platform-orchestrator-diode-relay",
	}
}

func natsConfig(cfg config.NATSConfiguration, name string) natstransport.Config {
	return natstransport.Config{
		URL: cfg.URL, Token: cfg.Token, CredentialsFile: cfg.CredentialsFile,
		CAFile: cfg.CAFile, ClientCertFile: cfg.ClientCertFile,
		ClientKeyFile: cfg.ClientKeyFile, OutboxDir: cfg.OutboxDir, Name: name,
	}
}

func publishDeploymentResultToNATS(ctx context.Context, cfg *config.StandardModeConfiguration, result platformorchestratorapi.DeploymentResultsUpdateBody) error {
	if cfg.RunnerID == "" {
		return errors.New("RUNNER_ID is required when standard mode publishes results through NATS")
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	subject, err := hmessaging.RunnerEventSubject(cfg.OrgID, cfg.RunnerID, "deployment-result")
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	event := hmessaging.EventEnvelope{
		ProtocolVersion: hmessaging.ProtocolVersionV1, EventID: cfg.DeploymentID + ":deployment-result",
		OrganizationID: cfg.OrgID, RunnerID: cfg.RunnerID, DeploymentID: cfg.DeploymentID,
		Type: "deployment-result", CreatedAt: now, Payload: payload,
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return natstransport.PublishMessage(ctx, natsConfig(cfg.NATS, "platform-orchestrator-job/"+cfg.DeploymentID), hmessaging.Message{
		ID: event.EventID, Subject: subject, Data: data, CreatedAt: now,
	})
}

func uploadLogsToNATSObjectStore(cfg *config.StandardModeConfiguration) LogsUploader {
	if cfg.EncryptingLogsKey == "" {
		return nil
	}
	recipient, _ := age.ParseX25519Recipient(cfg.EncryptingLogsKey)
	return func(ctx context.Context, logsBuffer bytes.Buffer) error {
		encryptedLogs, err := utils.EncryptBytes(logsBuffer.Bytes(), recipient)
		if err != nil {
			return err
		}
		objectKey := cfg.OrgID + "/" + cfg.DeploymentID
		if cfg.DeploymentEnvUUID != "" {
			objectKey = cfg.DeploymentEnvUUID + "/" + cfg.DeploymentID
		}
		sum := sha256.Sum256([]byte(encryptedLogs))
		payload, _ := json.Marshal(map[string]interface{}{
			"bucket": "PO_RUNNER_LOGS", "key": objectKey,
			"size": len(encryptedLogs), "sha256": hex.EncodeToString(sum[:]),
		})
		subject, err := hmessaging.RunnerEventSubject(cfg.OrgID, cfg.RunnerID, "log-object-ready")
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		event := hmessaging.EventEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1,
			EventID: cfg.DeploymentID + ":log-object-ready", OrganizationID: cfg.OrgID,
			RunnerID: cfg.RunnerID, DeploymentID: cfg.DeploymentID, Type: "log-object-ready",
			CreatedAt: now, Payload: payload}
		data, _ := json.Marshal(event)
		readyEvent := hmessaging.Message{ID: event.EventID, Subject: subject, Data: data, CreatedAt: now}
		return natstransport.PublishLogObject(ctx, natsConfig(cfg.NATS, "platform-orchestrator-logs/"+cfg.DeploymentID), natstransport.LogObject{
			Bucket: natstransport.RunnerLogsBucket, Key: objectKey, EncryptedLog: []byte(encryptedLogs), ReadyEvent: readyEvent,
		}, os.Getenv("NATS_BOOTSTRAP_OBJECT_STORE") == "true")
	}
}

func setLogLevel(ctx context.Context, logLevel string) *slog.LevelVar {
	programLevel := new(slog.LevelVar)
	var slogLevel slog.Level
	switch strings.ToUpper(logLevel) {
	case "DEBUG":
		slogLevel = slog.LevelDebug
	case "INFO":
		slogLevel = slog.LevelInfo
	case "WARN":
		slogLevel = slog.LevelWarn
	case "ERROR":
		slogLevel = slog.LevelError
	default:
		slog.WarnContext(ctx, fmt.Sprintf("failed to parse log level from config `%s` - using default level INFO", logLevel))
		slogLevel = slog.LevelInfo
	}
	programLevel.Set(slogLevel)
	return programLevel
}
