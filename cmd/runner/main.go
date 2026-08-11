package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"filippo.io/age"
	"github.com/pkg/errors"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/diode"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/executor"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gateway"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gatewayapi"
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
	GatewayMode       = "gateway"
	maxLogsBufferSize = 10 * 1024 * 1024 // 10 MB
)

var (
	buildInfo  *debug.BuildInfo
	gatewayURL string
)

func init() {
	buildInfo, _ = debug.ReadBuildInfo()
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
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
	flag.StringVar(&gatewayURL, "gateway-url", "", "HTTPS runner gateway endpoint")
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
		if gatewayURL != "" {
			return nil, errors.New("standard mode does not allow --gateway-url parameter")
		}
		cfg, err := config.GetStandardModeConfiguration()
		if err != nil {
			return nil, errors.Wrap(err, "failed to read standard mode config")
		}
		programLevel := setLogLevel(ctx, cfg.LogLevel)
		apiUpdates, standardModeErr := executor.ExecuteStandardMode(ctx, cfg, runner.CreateRunner, limitedLogsBuffer, programLevel)
		resultErr := publishDeploymentResultToGateway(ctx, cfg, apiUpdates)
		logsUploader := uploadLogsToGateway(cfg)
		if resultErr != nil {
			slog.ErrorContext(ctx, "[PLATFORM_ORCHESTRATOR]update-results", "err", resultErr)
			return logsUploader, errors.Wrap(resultErr, "failed to publish deployment results")
		} else if standardModeErr != nil {
			return logsUploader, errors.Wrap(standardModeErr, "failed to execute standard mode")
		} else {
			return logsUploader, nil
		}
	case RemoteMode:
		if cfg, err := config.GetRemoteModeConfiguration(gatewayURL); err != nil {
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
		cfg, err := config.GetOutboxModeConfiguration()
		if err != nil {
			return nil, errors.Wrap(err, "failed to read outbox mode config")
		}
		return nil, runOutboxFlusher(ctx, cfg)
	case GatewayMode:
		cfg, err := config.GetGatewayModeConfiguration()
		if err != nil {
			return nil, errors.Wrap(err, "failed to read gateway mode config")
		}
		setLogLevel(ctx, cfg.LogLevel)
		return nil, runGateway(ctx, cfg)
	default:
		return nil, errors.Errorf("invalid mode: %s. Must be '%s', '%s', '%s', '%s', '%s', or '%s'", mode, StandardMode, RemoteMode, DiodeExportMode, DiodeImportMode, OutboxFlushMode, GatewayMode)
	}
}

func runGateway(ctx context.Context, cfg *config.GatewayModeConfiguration) error {
	connection, err := natstransport.Connect(natsConfig(cfg.NATS, "platform-orchestrator-runner-gateway"))
	if err != nil {
		return err
	}
	defer connection.Close()
	backend, err := gateway.NewNATSBackend(connection)
	if err != nil {
		return err
	}
	var publicKeys gateway.PublicKeyResolver
	if cfg.ControlPlaneURL != "" {
		publicKeys, err = gateway.NewControlPlanePublicKeyResolver(cfg.ControlPlaneURL, nil, cfg.PublicKeyCacheTTL)
	} else {
		publicKeys, err = gateway.NewStaticPublicKeyResolver(cfg.StaticOrganizationID, cfg.StaticRunnerID, cfg.StaticPublicKeyFile)
	}
	if err != nil {
		return err
	}
	receiptKey, err := decodeGatewayKey(cfg.ReceiptKey)
	if err != nil {
		return err
	}
	handler, err := gateway.NewServer(gateway.ServerConfig{
		BasePath: cfg.BasePath, Backend: backend, PublicKeys: publicKeys,
		ReceiptKey: receiptKey, RunnerTokenSalt: cfg.RunnerTokenSalt,
		FetchWait: cfg.FetchWait, MaxLogBytes: cfg.MaxLogBytes,
	})
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           handler.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      cfg.FetchWait + 10*time.Second,
		IdleTimeout:       time.Minute,
	}
	errorsChannel := make(chan error, 1)
	go func() {
		slog.InfoContext(ctx, "runner gateway listening", "address", server.Addr, "base_path", cfg.BasePath)
		errorsChannel <- server.ListenAndServe()
	}()
	select {
	case err := <-errorsChannel:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownContext)
	}
}

func decodeGatewayKey(encoded string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{base64.RawURLEncoding, base64.StdEncoding} {
		if key, err := encoding.DecodeString(encoded); err == nil {
			if len(key) != 32 {
				return nil, errors.New("RUNNER_GATEWAY_RECEIPT_KEY must decode to exactly 32 bytes")
			}
			return key, nil
		}
	}
	return nil, errors.New("RUNNER_GATEWAY_RECEIPT_KEY must be base64 encoded")
}

func runOutboxFlusher(ctx context.Context, cfg *config.OutboxModeConfiguration) error {
	client, err := gatewayapi.NewClient(gatewayapi.ClientConfig{
		BaseURL: cfg.Gateway.URL, OrganizationID: cfg.OrganizationID, RunnerID: cfg.RunnerID,
		CAFile: cfg.Gateway.CAFile, ClientCertFile: cfg.Gateway.ClientCertFile,
		ClientKeyFile: cfg.Gateway.ClientKeyFile,
	})
	if err != nil {
		return err
	}
	outbox, err := gatewayapi.NewOutbox(cfg.Gateway.OutboxDir, client)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if err := outbox.Flush(ctx); err != nil {
			slog.WarnContext(ctx, "failed to flush runner gateway outbox", "err", err)
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
		ClientKeyFile: cfg.ClientKeyFile, Name: name,
	}
}

func standardGatewayClient(cfg *config.StandardModeConfiguration) (*gatewayapi.Client, error) {
	return gatewayapi.NewClient(gatewayapi.ClientConfig{
		BaseURL: cfg.Gateway.URL, OrganizationID: cfg.OrgID, RunnerID: cfg.RunnerID,
		CAFile: cfg.Gateway.CAFile, ClientCertFile: cfg.Gateway.ClientCertFile,
		ClientKeyFile: cfg.Gateway.ClientKeyFile,
	})
}

func publishDeploymentResultToGateway(ctx context.Context, cfg *config.StandardModeConfiguration, result platformorchestratorapi.DeploymentResultsUpdateBody) error {
	client, err := standardGatewayClient(cfg)
	if err != nil {
		return err
	}
	if cfg.Gateway.OutboxDir == "" {
		return client.PostResults(ctx, cfg.DeploymentID, cfg.DeploymentToken, result)
	}
	outbox, err := gatewayapi.NewOutbox(cfg.Gateway.OutboxDir, client)
	if err != nil {
		return err
	}
	return outbox.PostResults(ctx, cfg.DeploymentID, cfg.DeploymentToken, result)
}

func uploadLogsToGateway(cfg *config.StandardModeConfiguration) LogsUploader {
	if cfg.EncryptingLogsKey == "" {
		return nil
	}
	recipient, _ := age.ParseX25519Recipient(cfg.EncryptingLogsKey)
	return func(ctx context.Context, logsBuffer bytes.Buffer) error {
		if cfg.DeploymentEnvUUID == "" {
			return errors.New("DEPLOYMENT_ENV_UUID is required to upload encrypted logs")
		}
		encryptedLogs, err := utils.EncryptBytes(logsBuffer.Bytes(), recipient)
		if err != nil {
			return err
		}
		client, err := standardGatewayClient(cfg)
		if err != nil {
			return err
		}
		if cfg.Gateway.OutboxDir == "" {
			return client.PutLogs(ctx, cfg.DeploymentEnvUUID, cfg.DeploymentID, cfg.DeploymentToken, strings.NewReader(encryptedLogs))
		}
		outbox, err := gatewayapi.NewOutbox(cfg.Gateway.OutboxDir, client)
		if err != nil {
			return err
		}
		return outbox.PutLogs(ctx, cfg.DeploymentEnvUUID, cfg.DeploymentID, cfg.DeploymentToken, encryptedLogs)
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
