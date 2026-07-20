package config

import (
	"context"
	"os"

	"filippo.io/age"
	"github.com/pkg/errors"

	"github.com/go-playground/validator"
	"github.com/google/uuid"
	"github.com/sethvargo/go-envconfig"
)

type RunnerMode string

const (
	PlanOnly RunnerMode = "plan_only"
	Destroy  RunnerMode = "destroy"
	Deploy   RunnerMode = "deploy"
)

type IaCBackendType string

const (
	BackendOpenTofu  IaCBackendType = "opentofu"
	BackendTerraform IaCBackendType = "terraform"
)

type StandardModeConfiguration struct {
	LogLevel                      string         `env:"LOG_LEVEL, default=info"`
	OrgID                         string         `env:"ORG_ID,required"`
	PlatformOrchestratorApiPrefix string         `env:"PLATFORM_ORCHESTRATOR_API_PREFIX,required"`
	DeploymentID                  string         `env:"DEPLOYMENT_ID,required"`
	Token                         string         `env:"TOKEN,required"`
	Mode                          RunnerMode     `env:"MODE,required"`
	EncryptingKey                 string         `env:"ENCRYPTING_KEY"`
	MetadataKey                   string         `env:"METADATA_KEY, default=platform_orchestrator_metadata"`
	IaCCodeDir                    string         `env:"IAC_CODE_DIR, default=/opt/runner/tofu"`
	IaCBackend                    IaCBackendType `env:"IAC_BACKEND, default=opentofu"`
	EncryptingLogsKey             string         `env:"ENCRYPTING_LOGS_KEY"`
	LogsUrl                       string         `env:"LOGS_URL"`
}

type RemoteModeConfiguration struct {
	LogLevel   string `env:"LOG_LEVEL, default=info"`
	OrgID      string `env:"ORG_ID,required"`
	RunnerId   string `env:"RUNNER_ID,required"`
	PrivateKey string `env:"PRIVATE_KEY,required"`
	RemoteUrl  string `env:"REMOTE_URL"`
}

func GetStandardModeConfiguration() (*StandardModeConfiguration, error) {
	conf := &StandardModeConfiguration{}

	ctx := context.Background()

	// Backwards compatibility: if IAC_CODE_DIR is unset, fall back to TOFU_CODE_DIR.
	if os.Getenv("IAC_CODE_DIR") == "" && os.Getenv("TOFU_CODE_DIR") != "" {
		if err := os.Setenv("IAC_CODE_DIR", os.Getenv("TOFU_CODE_DIR")); err != nil {
			return nil, errors.Wrap(err, "failed to apply TOFU_CODE_DIR fallback")
		}
	}
	if err := envconfig.ProcessWith(ctx, &envconfig.Config{
		DefaultOverwrite: true,
		DefaultNoInit:    true,
		Target:           conf,
	}); err != nil {
		return nil, err
	}

	validate := validator.New()
	if err := validate.Struct(conf); err != nil {
		return nil, err
	}

	if err := validateMode(conf.Mode); err != nil {
		return nil, err
	}

	if err := validateIaCBackend(conf.IaCBackend); err != nil {
		return nil, err
	}

	if _, err := uuid.Parse(conf.DeploymentID); err != nil {
		return nil, errors.Wrap(err, "DEPLOYMENT_ID should be a valid uuid")
	}

	if conf.EncryptingKey != "" {
		if _, err := age.ParseX25519Recipient(conf.EncryptingKey); err != nil {
			return nil, errors.Wrap(err, "ENCRYPTING_KEY is not a valid public recipient")
		}
	}

	return conf, nil
}

func GetRemoteModeConfiguration() (*RemoteModeConfiguration, error) {
	conf := &RemoteModeConfiguration{}

	ctx := context.Background()

	if err := envconfig.ProcessWith(ctx, &envconfig.Config{
		DefaultOverwrite: true,
		DefaultNoInit:    true,
		Target:           conf,
	}); err != nil {
		return nil, err
	}

	validate := validator.New()
	if err := validate.Struct(conf); err != nil {
		return nil, err
	}

	return conf, nil
}

func validateMode(mode RunnerMode) error {
	switch mode {
	case Deploy, Destroy, PlanOnly:
		return nil
	default:
		return errors.Errorf("mode %s is not supported", mode)
	}
}

func validateIaCBackend(backend IaCBackendType) error {
	switch backend {
	case BackendOpenTofu, BackendTerraform:
		return nil
	default:
		return errors.Errorf("IAC_BACKEND %q is not supported, must be %q or %q", backend, BackendOpenTofu, BackendTerraform)
	}
}
