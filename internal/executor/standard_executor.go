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
	logsBuffer *limitedlogsbuffer.LimitedLogsBuffer, programLevel *slog.LevelVar) (platformorchestratorapi.DeploymentResultsUpdateBody, error) {
	logsWriter := io.MultiWriter(os.Stdout, logsBuffer)
	logger := slog.New(slog.NewJSONHandler(logsWriter, &slog.HandlerOptions{Level: programLevel}))
	slog.SetDefault(logger.WithGroup("runner").With(slog.String("org_id", cfg.OrgID), slog.String("deploy_id", cfg.DeploymentID), slog.String("mode", string(cfg.Mode))))

	rn, err := runnerFactory(ctx, cfg)
	if err != nil {
		return platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Failure, Error: &platformorchestratorapi.Error{Error: "RUNNER", Message: errors.Wrap(err, "failed to create runner").Error()}}, errors.Wrap(err, "[RUNNER]failed to create runner")
	}

	if err := rn.CreateIaCFolder(ctx); err != nil {
		return platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Failure, Error: &platformorchestratorapi.Error{Error: "RUNNER", Message: errors.Wrap(err, "failed to create a folder with IaC files").Error()}}, errors.Wrap(err, "[RUNNER]failed to create a folder with IaC files")
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
		slog.InfoContext(ctx, "IaC binary is available", "version", iacVersion)
	}

	// IaC Init
	out, err := rn.Init(ctx)
	writeOutputWithNewline(logsWriter, out)
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

	// IaC Plan
	slog.InfoContext(ctx, "executing plan")
	planOutput, plannedOutputs, iacChanges, err := rn.Plan(ctx)
	writeOutputWithNewline(logsWriter, planOutput)
	if err != nil {
		return platformorchestratorapi.DeploymentResultsUpdateBody{Error: &platformorchestratorapi.Error{Error: getIaCErrorCode(err), Message: err.Error()}, Status: platformorchestratorapi.Failure}, errors.Wrap(err, "[IAC]plan error")
	} else if cfg.Mode == config.PlanOnly {
		var encryptedOutputs string
		if len(cfg.EncryptingKey) > 0 {
			delete(plannedOutputs, cfg.MetadataKey)
			encodedPlannedOutputs, _ := json.Marshal(plannedOutputs)
			if encryptedOutputs, err = rn.EncryptString(string(encodedPlannedOutputs)); err != nil {
				return platformorchestratorapi.DeploymentResultsUpdateBody{Error: &platformorchestratorapi.Error{Error: "RUNNER", Message: errors.Wrap(err, "failed to encrypt plan outputs").Error()}, Status: platformorchestratorapi.Failure}, errors.Wrap(err, "[RUNNER]results encryption error")
			}
		}
		return platformorchestratorapi.DeploymentResultsUpdateBody{Status: platformorchestratorapi.Success, Outputs: &encryptedOutputs, TfResourceCounts: fromIaCChangesToTFResourceCounts(iacChanges)}, nil
	}

	// IaC Apply
	slog.InfoContext(ctx, "executing apply")
	applyOutput, iacChanges, err := rn.Apply(ctx)
	writeOutputWithNewline(logsWriter, applyOutput)
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
	var runnerError *runner.RunnerError
	if errors.As(err, &runnerError) {
		return runnerError.Code()
	}
	return "IAC_COMMAND_ERROR"
}
