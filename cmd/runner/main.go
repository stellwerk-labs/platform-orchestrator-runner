package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"runtime/debug"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/pkg/errors"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/executor"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/limitedlogsbuffer"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/utils"
)

type LogsUploader func(ctx context.Context, logsBuffer bytes.Buffer) error

const (
	StandardMode      = "standard"
	RemoteMode        = "remote"
	maxLogsBufferSize = 10 * 1024 * 1024 // 10 MB
)

var (
	buildInfo        *debug.BuildInfo
	remoteConnectURL string
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
	flag.StringVar(&remoteConnectURL, "remote-connect", "", "URL to connect to the Orchestrator and wait for remote runner messages")
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
		if remoteConnectURL != "" {
			return nil, errors.New("standard mode does not allow --remote-connect parameter")
		}
		cfg, err := config.GetStandardModeConfiguration()
		if err != nil {
			return nil, errors.Wrap(err, "failed to read standard mode config")
		}
		apiClient, err := platformorchestratorapi.NewClientWithResponses(
			cfg.PlatformOrchestratorApiPrefix,
			platformorchestratorapi.WithHTTPClient(utils.WrapHttpClientWithRetries(http.DefaultClient)),
		)
		if err != nil {
			return nil, errors.Wrap(err, "failed to initialize api client")
		}
		programLevel := setLogLevel(ctx, cfg.LogLevel)
		apiUpdates, standardModeErr := executor.ExecuteStandardMode(ctx, cfg, runner.CreateRunner, limitedLogsBuffer, programLevel)
		if err := utils.SendResultsToApi(ctx, apiClient, cfg.OrgID, cfg.DeploymentID, cfg.Token, apiUpdates); err != nil {
			slog.ErrorContext(ctx, "[PLATFORM_ORCHESTRATOR]update-results", "err", err)
			// logs uploaded if results cannot be pushed to api
			return uploadLogsToRunnerLogsBucket(cfg.EncryptingLogsKey, cfg.LogsUrl), errors.Wrap(err, "failed to send results to api")
		} else if standardModeErr != nil {
			// logs uploaded if results can be pushed to the api but execution failed
			return uploadLogsToRunnerLogsBucket(cfg.EncryptingLogsKey, cfg.LogsUrl), errors.Wrap(standardModeErr, "failed to execute standard mode")
		} else {
			// logs uploaded if results can be pushed to the api and execution was successful
			return uploadLogsToRunnerLogsBucket(cfg.EncryptingLogsKey, cfg.LogsUrl), nil
		}
	case RemoteMode:
		if cfg, err := config.GetRemoteModeConfiguration(); err != nil {
			return nil, errors.Wrap(err, "failed to read remote mode config")
		} else {
			// source remoteConnectURL from config if not provided via flag
			if remoteConnectURL == "" {
				remoteConnectURL = cfg.RemoteUrl
			}

			if _, err := url.Parse(remoteConnectURL); err != nil {
				return nil, errors.Wrap(err, "invalid remote connect URL provided")
			}

			programLevel := setLogLevel(ctx, cfg.LogLevel)
			if apiClient, err := platformorchestratorapi.NewClientWithResponses(
				remoteConnectURL,
				platformorchestratorapi.WithHTTPClient(utils.WrapHttpClientWithRetries(http.DefaultClient)),
			); err != nil {
				return nil, errors.Wrap(err, "failed to initialize api client")
			} else {
				return nil, executor.ExecuteRemoteMode(ctx, cfg, apiClient, programLevel)
			}
		}
	default:
		return nil, errors.Errorf("invalid mode: %s. Must be '%s' or '%s'", mode, StandardMode, RemoteMode)
	}
}

func uploadLogsToRunnerLogsBucket(encryptLogsKey, signedURL string) LogsUploader {
	if signedURL == "" {
		return nil
	}
	// This is already checked when the configuration is parsed
	recipient, _ := age.ParseX25519Recipient(encryptLogsKey)
	return func(ctx context.Context, logsBuffer bytes.Buffer) error {
		encryptedLogs, err := utils.EncryptBytes(logsBuffer.Bytes(), recipient)
		if err != nil {
			return errors.Wrap(err, "failed to encrypt logs")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, signedURL, bytes.NewReader([]byte(encryptedLogs)))
		if err != nil {
			return errors.Wrap(err, "failed to create HTTP request")
		}

		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("Content-Length", fmt.Sprintf("%d", len(encryptedLogs)))

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return errors.Wrap(err, "failed to execute HTTP request")
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return errors.Errorf("unexpected status code: %d, body: %s", resp.StatusCode, string(body))
		}

		return nil
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
