package executor

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/limitedlogsbuffer"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/sshagent"
)

const (
	DestroyMode = "destroy"
)

// ExecuteStandardMode executes the standard IaC workflow
func ExecuteStandardMode(ctx context.Context, cfg *config.StandardModeConfiguration, runnerFactory runner.RunnerFactory,
	logsBuffer *limitedlogsbuffer.LimitedLogsBuffer, programLevel *slog.LevelVar) (result platformorchestratorapi.DeploymentResultsUpdateBody, resultErr error) {
	defer func() {
		if (cfg.RetainPlan || cfg.Mode == config.ApplyRetained) && resultErr != nil {
			resultErr = errors.New("retained plan execution failed")
			result = platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Failure, Error: &platformorchestratorapi.Error{Error: "RUNNER", Message: resultErr.Error()}}
		}
	}()
	logsWriter := io.MultiWriter(os.Stdout, logsBuffer)
	logger := slog.New(slog.NewJSONHandler(logsWriter, &slog.HandlerOptions{Level: programLevel}))
	slog.SetDefault(logger.WithGroup("runner").With(slog.String("org_id", cfg.OrgID), slog.String("deploy_id", cfg.DeploymentID), slog.String("mode", string(cfg.Mode))))

	retained := cfg.RetainPlan || cfg.Mode == config.ApplyRetained
	binding, err := retainedBinding(cfg)
	if err != nil {
		return platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Failure, Error: &platformorchestratorapi.Error{Error: "RUNNER", Message: err.Error()}}, err
	}
	if err := cfg.ValidateObservationProtocol(); err != nil {
		return platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Failure, Error: &platformorchestratorapi.Error{Error: "RUNNER", Message: err.Error()}}, err
	}
	if cfg.IsObservation() {
		return executeObservation(ctx, cfg, runnerFactory)
	}
	switch cfg.Mode {
	case config.Deploy, config.Destroy, config.PlanOnly, config.ApplyRetained:
	default:
		err := errors.New("unsupported runner mode")
		return platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Failure, Error: &platformorchestratorapi.Error{Error: "RUNNER", Message: err.Error()}}, err
	}

	rn, err := runnerFactory(ctx, cfg)
	if err != nil {
		return platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Failure, Error: &platformorchestratorapi.Error{Error: "RUNNER", Message: errors.Wrap(err, "failed to create runner").Error()}}, errors.Wrap(err, "[RUNNER]failed to create runner")
	}

	if err := rn.CreateIaCFolder(ctx); err != nil {
		return platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Failure, Error: &platformorchestratorapi.Error{Error: "RUNNER", Message: errors.Wrap(err, "failed to create a folder with IaC files").Error()}}, errors.Wrap(err, "[RUNNER]failed to create a folder with IaC files")
	}

	if cfg.Mode == config.ApplyRetained {
		retainedRunner, ok := rn.(interface {
			RestoreRetainedPlan(context.Context, runner.RetainedBinding, string) error
		})
		if !ok || retainedRunner.RestoreRetainedPlan(ctx, binding, cfg.RetainedSHA256) != nil {
			err := errors.New("retained plan restore failed")
			return platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Failure, Error: &platformorchestratorapi.Error{Error: "RUNNER", Message: err.Error()}}, err
		}
	}
	if closer, err := sshagent.Launch(filepath.Join(cfg.IaCCodeDir, ".ssh-agent")); err != nil {
		return platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Failure, Error: &platformorchestratorapi.Error{Error: "RUNNER", Message: errors.Wrap(err, "failed to launch ssh-agent").Error()}}, errors.Wrap(err, "[RUNNER]failed to launch ssh-agent")
	} else {
		defer closer()
	}

	if _, err := os.Stat(filepath.Join(cfg.IaCCodeDir, "main.tf")); err != nil {
		return platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Failure, Error: &platformorchestratorapi.Error{Error: "RUNNER", Message: errors.Wrap(err, "failed to check content of the folder with IaC files").Error()}}, errors.Wrap(err, "[RUNNER]failed to check content of the folder with IaC files")
	}

	// IaC Version
	if iacVersion, err := rn.Version(ctx); err != nil {
		return platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Failure, Error: &platformorchestratorapi.Error{Error: getIaCErrorCode(err), Message: err.Error()}}, errors.Wrap(err, "[IAC]version error")
	} else {
		if retained {
			slog.InfoContext(ctx, "IaC binary is available")
		} else {
			slog.InfoContext(ctx, "IaC binary is available", "version", iacVersion)
		}
	}

	// IaC Init
	var out string
	if cfg.Mode != config.ApplyRetained {
		out, err = rn.Init(ctx)
	}
	if !retained {
		writeOutputWithNewline(logsWriter, out)
	}
	if err != nil {
		return platformorchestratorapi.DeploymentResultsUpdateBody{Error: &platformorchestratorapi.Error{Error: getIaCErrorCode(err), Message: err.Error()}, Status: platformorchestratorapi.Failure}, errors.Wrap(err, "[IAC]init error")
	}

	// IaC Destroy
	if cfg.Mode == DestroyMode {
		slog.InfoContext(ctx, "executing destroy")
		destroyOutput, iacChanges, err := rn.Destroy(ctx)
		writeOutputWithNewline(logsWriter, destroyOutput)
		if err != nil {
			return platformorchestratorapi.DeploymentResultsUpdateBody{Error: &platformorchestratorapi.Error{Error: getIaCErrorCode(err), Message: err.Error()}, Status: platformorchestratorapi.Failure}, errors.Wrap(err, "[IAC]destroy error")
		} else {
			return platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Success, TfResourceCounts: fromIaCChangesToTFResourceCounts(iacChanges)}, nil
		}
	}

	if cfg.Mode != config.ApplyRetained {
		result, done, err := executePlan(ctx, cfg, rn, binding, logsWriter, retained)
		if done {
			return result, err
		}
	}

	// IaC Apply
	slog.InfoContext(ctx, "executing apply")
	applyOutput, iacChanges, err := rn.Apply(ctx)
	if !retained {
		writeOutputWithNewline(logsWriter, applyOutput)
	}
	if err != nil {
		return platformorchestratorapi.DeploymentResultsUpdateBody{Error: &platformorchestratorapi.Error{Error: getIaCErrorCode(err), Message: err.Error()}, Status: platformorchestratorapi.Failure}, errors.Wrap(err, "[IAC]apply error")
	} else {
		// IaC Metadata
		var metadata = make([]platformorchestratorapi.DeploymentResultMetadataPerNode, 0)
		var metadataPerNode map[string]map[string]interface{}
		if metadataOut, err := rn.OutputMetadata(ctx, cfg.MetadataKey); err != nil {
			return platformorchestratorapi.DeploymentResultsUpdateBody{Error: &platformorchestratorapi.Error{Error: getIaCErrorCode(err), Message: err.Error()}, Status: platformorchestratorapi.Failure}, errors.Wrap(err, "[IAC]fetch metadata error")
		} else {
			if err := json.Unmarshal([]byte(metadataOut), &metadataPerNode); err != nil {
				return platformorchestratorapi.DeploymentResultsUpdateBody{Error: &platformorchestratorapi.Error{Error: "RUNNER", Message: errors.Wrap(err, "failed to parse metadata output").Error()}, Status: platformorchestratorapi.Failure}, errors.Wrap(err, "[IAC]apply error")
			} else {
				for h, m := range metadataPerNode {
					metadata = append(metadata, platformorchestratorapi.DeploymentResultMetadataPerNode{NodeId: h, Metadata: m})
				}
				var encryptedOutputs string
				if len(cfg.EncryptingKey) > 0 {
					// IaC Outputs
					if outputMap, err := rn.Output(ctx); err != nil {
						return platformorchestratorapi.DeploymentResultsUpdateBody{Error: &platformorchestratorapi.Error{Error: getIaCErrorCode(err), Message: err.Error()}, Status: platformorchestratorapi.Failure}, errors.Wrap(err, "[IAC]outputs error")
					} else {
						delete(outputMap, cfg.MetadataKey)
						outputString, _ := json.Marshal(outputMap)
						if encryptedOutputs, err = rn.EncryptString(string(outputString)); err != nil {
							return platformorchestratorapi.DeploymentResultsUpdateBody{Error: &platformorchestratorapi.Error{Error: "RUNNER", Message: errors.Wrap(err, "failed to encrypt plan outputs").Error()}, Status: platformorchestratorapi.Failure}, errors.Wrap(err, "[RUNNER]results encryption error")
						}
					}
				}
				return platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Success, Outputs: &encryptedOutputs, TfResourceCounts: fromIaCChangesToTFResourceCounts(iacChanges), Metadata: metadata}, nil
			}
		}
	}
}

func writeOutputWithNewline(w io.Writer, content string) {
	if len(content) > 0 {
		_, _ = w.Write([]byte(content))
		if !strings.HasSuffix(content, "\n") {
			_, _ = w.Write([]byte("\n"))
		}
	}
}

func getIaCErrorCode(err error) string {
	var coded interface{ Code() string }
	if errors.As(err, &coded) {
		return coded.Code()
	}
	return "IAC_COMMAND_ERROR"
}

func retainedBinding(cfg *config.StandardModeConfiguration) (runner.RetainedBinding, error) {
	var binding runner.RetainedBinding
	if !cfg.RetainPlan && cfg.Mode != config.ApplyRetained {
		return binding, nil
	}
	invalid := func() (runner.RetainedBinding, error) {
		return binding, errors.New("invalid retained plan configuration")
	}
	if !validRetainedMode(cfg) {
		return invalid()
	}
	if json.Unmarshal([]byte(cfg.RetainedBinding), &binding) != nil || binding.Schema != "stellwerk.retained-plan/v1" {
		return invalid()
	}
	if !retainedIdentityMatches(cfg, binding) {
		return invalid()
	}
	if cfg.RetainPlan && binding.SourceDeploymentID != cfg.DeploymentID {
		return invalid()
	}
	if cfg.Mode == config.ApplyRetained && len(cfg.RetainedSHA256) != 64 {
		return invalid()
	}
	return binding, nil
}

func validRetainedMode(cfg *config.StandardModeConfiguration) bool {
	return cfg.RetainedPlansEnabled && !cfg.IsObservation() && (!cfg.RetainPlan || cfg.Mode == config.PlanOnly)
}

func retainedIdentityMatches(cfg *config.StandardModeConfiguration, b runner.RetainedBinding) bool {
	return b.OrgID == cfg.OrgID && b.EnvironmentID == cfg.DeploymentEnvUUID && b.RunnerID == cfg.RunnerID && b.SourceDeploymentID != "" && b.Backend != "" && b.Tool == string(cfg.IaCBackend)
}

func executePlan(ctx context.Context, cfg *config.StandardModeConfiguration, rn runner.RunnerInterface, binding runner.RetainedBinding, logsWriter io.Writer, retained bool) (platformorchestratorapi.DeploymentResultsUpdateBody, bool, error) {
	slog.InfoContext(ctx, "executing plan")
	planOutput, plannedOutputs, iacChanges, err := rn.Plan(ctx)
	if !retained {
		writeOutputWithNewline(logsWriter, planOutput)
	}
	if err != nil {
		return platformorchestratorapi.DeploymentResultsUpdateBody{Error: &platformorchestratorapi.Error{Error: getIaCErrorCode(err), Message: err.Error()}, Status: platformorchestratorapi.Failure}, true, errors.Wrap(err, "[IAC]plan error")
	}
	if cfg.Mode != config.PlanOnly {
		return platformorchestratorapi.DeploymentResultsUpdateBody{}, false, nil
	}
	if cfg.RetainPlan {
		return exportRetainedPlan(ctx, rn, binding, iacChanges)
	}
	var encryptedOutputs string
	if len(cfg.EncryptingKey) > 0 {
		delete(plannedOutputs, cfg.MetadataKey)
		encodedPlannedOutputs, _ := json.Marshal(plannedOutputs)
		if encryptedOutputs, err = rn.EncryptString(string(encodedPlannedOutputs)); err != nil {
			return platformorchestratorapi.DeploymentResultsUpdateBody{Error: &platformorchestratorapi.Error{Error: "RUNNER", Message: errors.Wrap(err, "failed to encrypt plan outputs").Error()}, Status: platformorchestratorapi.Failure}, true, errors.Wrap(err, "[RUNNER]results encryption error")
		}
	}
	return platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Success, Outputs: &encryptedOutputs, TfResourceCounts: fromIaCChangesToTFResourceCounts(iacChanges)}, true, nil
}

func exportRetainedPlan(ctx context.Context, rn runner.RunnerInterface, binding runner.RetainedBinding, changes *runner.IaCChanges) (platformorchestratorapi.DeploymentResultsUpdateBody, bool, error) {
	retainedRunner, ok := rn.(interface {
		RetainPlan(context.Context, runner.RetainedBinding) (string, error)
	})
	if ok {
		encrypted, err := retainedRunner.RetainPlan(ctx, binding)
		if err == nil {
			return platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Success, Outputs: &encrypted, TfResourceCounts: fromIaCChangesToTFResourceCounts(changes)}, true, nil
		}
	}
	err := errors.New("retained plan export failed")
	return platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Failure, Error: &platformorchestratorapi.Error{Error: "RUNNER", Message: err.Error()}}, true, err
}
