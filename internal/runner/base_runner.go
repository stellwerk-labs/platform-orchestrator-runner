package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"filippo.io/age"
	tfjson "github.com/hashicorp/terraform-json"
	"github.com/pkg/errors"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/utils"
)

// baseRunner holds the fields and shared logic used by all local IaC runners.
// It is not exported and is embedded by the concrete runner types.
type baseRunner struct {
	orgID        string
	deploymentID string
	folder       string
	binaryPath   string
	recipient    age.Recipient
	bundleLoader func(context.Context) ([]byte, error)
}

func (r *baseRunner) planFile() string {
	return filepath.Join(r.folder, "plan.tfplan")
}

// CreateIaCFolder downloads the deployment bundle and extracts it into the working folder.
func (r *baseRunner) CreateIaCFolder(ctx context.Context) error {
	if r.bundleLoader != nil {
		bundle, err := r.bundleLoader(ctx)
		if err != nil {
			return errors.Wrap(err, "failed to obtain deployment bundle from runner gateway")
		}
		return errors.Wrapf(unarchiveBundleToFolder(bundle, r.folder), "failed to unbundle the archive and store it in %s", r.folder)
	}
	return errors.New("runner gateway bundle loader is not configured")
}

// Version returns the version string reported by the configured binary.
func (r *baseRunner) Version(ctx context.Context) (string, error) {
	out, err := runCmd(ctx, r.binaryPath, "version")
	if err != nil {
		return out, errors.Wrap(err, fmt.Sprintf("failed to run `%s version`", r.binaryPath))
	}
	return out, nil
}

// OutputMetadata fetches a single named output as a raw JSON string.
func (r *baseRunner) OutputMetadata(ctx context.Context, metadata string) (string, error) {
	out, err := runCmd(ctx, r.binaryPath, "-chdir="+r.folder, "output", "-json", "-no-color", metadata)
	if err != nil {
		if parsedErr, err2 := r.parseError("output", err); err2 == nil && parsedErr != nil {
			return out, parsedErr
		}
		return out, errors.Wrap(err, fmt.Sprintf("failed to run `%s output`", r.binaryPath))
	}
	return out, nil
}

// EncryptString encrypts a string with the configured recipient key, or returns
// an empty string when no key is configured.
func (r *baseRunner) EncryptString(str string) (string, error) {
	if r.recipient != nil {
		return utils.EncryptBytes([]byte(str), r.recipient)
	}
	return "", nil
}

func (r *baseRunner) init(ctx context.Context, extraFlags ...string) (string, error) {
	args := append([]string{"-chdir=" + r.folder, "init", "-no-color", "-json", "-compact-warnings"}, extraFlags...)
	out, err := runCmd(ctx, r.binaryPath, args...)
	if err != nil {
		if parsedErr, err2 := r.parseError("init", err); err2 == nil && parsedErr != nil {
			return out, parsedErr
		}
		return out, errors.Wrap(err, fmt.Sprintf("failed to run `%s init`", r.binaryPath))
	}
	return out, nil
}

func (r *baseRunner) plan(ctx context.Context, extraFlags ...string) (string, map[string]interface{}, *IaCChanges, error) {
	args := append([]string{"-chdir=" + r.folder, "plan", "-no-color", "-json", "-compact-warnings", "-out", r.planFile()}, extraFlags...)
	out, err := runCmd(ctx, r.binaryPath, args...)
	if err != nil {
		if parsedErr, err2 := r.parseError("plan", err); err2 == nil && parsedErr != nil {
			return out, nil, nil, parsedErr
		}
		return out, nil, nil, errors.Wrap(err, fmt.Sprintf("failed to run `%s plan`", r.binaryPath))
	}

	planOut, err := runCmd(ctx, r.binaryPath, "-chdir="+r.folder, "show", "-no-color", "-json", r.planFile())
	if err != nil {
		return out, nil, nil, errors.Wrap(err, fmt.Sprintf("failed to extract `%s plan`", r.binaryPath))
	}

	plannedOutput, err := parseShowPlanOutput(planOut)
	if err != nil {
		return out, nil, nil, err
	}
	changes, err := scanOutputs([]byte(out))
	if err != nil {
		return out, plannedOutput, nil, err
	}
	return out, plannedOutput, changes, nil
}

func (r *baseRunner) apply(ctx context.Context, extraFlags ...string) (string, *IaCChanges, error) {
	args := append([]string{"-chdir=" + r.folder, "apply", "-auto-approve", "-no-color", "-json", "-compact-warnings"}, extraFlags...)
	args = append(args, r.planFile())
	out, err := runCmd(ctx, r.binaryPath, args...)
	if err != nil {
		if parsedErr, err2 := r.parseError("apply", err); err2 == nil && parsedErr != nil {
			return out, nil, parsedErr
		}
		return out, nil, errors.Wrap(err, fmt.Sprintf("failed to run `%s apply`", r.binaryPath))
	}
	changes, err := scanOutputs([]byte(out))
	if err != nil {
		return "", nil, err
	}
	return out, changes, nil
}

func (r *baseRunner) destroy(ctx context.Context, extraFlags ...string) (string, *IaCChanges, error) {
	args := append([]string{"-chdir=" + r.folder, "destroy", "-auto-approve", "-no-color", "-json", "-compact-warnings"}, extraFlags...)
	out, err := runCmd(ctx, r.binaryPath, args...)
	if err != nil {
		if parsedErr, err2 := r.parseError("destroy", err); err2 == nil && parsedErr != nil {
			return out, nil, parsedErr
		}
		return out, nil, errors.Wrap(err, fmt.Sprintf("failed to run `%s destroy`", r.binaryPath))
	}
	changes, err := scanOutputs([]byte(out))
	if err != nil {
		return "", nil, err
	}
	return out, changes, nil
}

func (r *baseRunner) output(ctx context.Context, extraFlags ...string) (map[string]interface{}, error) {
	args := append([]string{"-chdir=" + r.folder, "output", "-json", "-no-color"}, extraFlags...)
	out, err := runCmd(ctx, r.binaryPath, args...)
	if err != nil {
		if parsedErr, err2 := r.parseError("output", err); err2 == nil && parsedErr != nil {
			return nil, parsedErr
		}
		return nil, errors.Wrap(err, fmt.Sprintf("failed to run `%s output`", r.binaryPath))
	}
	var workingOutMap map[string]interface{}
	if err := json.Unmarshal([]byte(out), &workingOutMap); err != nil {
		return nil, errors.Wrap(err, "failed to parse outputs")
	}
	outMap := make(map[string]interface{}, len(workingOutMap))
	for k, v := range workingOutMap {
		outMap[k] = v.(map[string]interface{})["value"]
	}
	return outMap, nil
}

// parseShowPlanOutput extracts output_changes from the JSON emitted by `show -json`.
func parseShowPlanOutput(planOut string) (map[string]interface{}, error) {
	if i := strings.LastIndex(planOut, "\n{"); i > 0 {
		planOut = planOut[i+1:]
	}
	var anon = struct {
		OutputChanges map[string]OutputChange `json:"output_changes"`
	}{}
	if err := json.Unmarshal([]byte(planOut), &anon); err != nil {
		return nil, errors.Wrap(err, "failed to parse plan output_changes")
	}
	plannedOutput := make(map[string]interface{}, len(anon.OutputChanges))
	for s, change := range anon.OutputChanges {
		plannedOutput[s] = change.Compact()
	}
	return plannedOutput, nil
}

// scanOutputs scans JSON log lines for a change_summary entry.
func scanOutputs(out []byte) (*IaCChanges, error) {
	scanner := bufio.NewScanner(bytes.NewBuffer(out))
	var changes *IaCChanges
	for scanner.Scan() {
		if line := scanner.Bytes(); strings.HasPrefix(string(line), "{") {
			var logLine IaCLogs
			if err := json.Unmarshal(line, &logLine); err == nil && logLine.Type == IaCLogsChangeSummaryType {
				changes = &logLine.Changes
				break
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.Wrap(err, "failed to read apply output")
	}
	if changes == nil {
		return nil, errors.New("failed to find change summary in apply output")
	}
	return changes, nil
}

// parseError attempts to extract a structured RunnerError from JSON log output.
func (r *baseRunner) parseError(action string, err error) (*RunnerError, error) {
	if err == nil {
		return nil, nil
	}
	view := []byte(err.Error())
	for {
		i := bytes.LastIndexByte(view, '\n')
		var tfLogMessage tfjson.DiagnosticLogMessage
		if err := json.Unmarshal(view[i+1:], &tfLogMessage); err == nil {
			if tfLogMessage.Level() == tfjson.Error && (tfLogMessage.Summary != "" || tfLogMessage.Detail != "") {
				runnerErr := &RunnerError{
					Action:  action,
					Summary: tfLogMessage.Summary,
					Detail:  tfLogMessage.Detail,
				}
				if tfLogMessage.Snippet != nil && tfLogMessage.Snippet.Context != nil {
					idWithType := strings.Split(*tfLogMessage.Snippet.Context, " ")
					if len(idWithType) >= 1 {
						switch idWithType[0] {
						case "provider":
							runnerErr.EntityType = CategoryProvider
						case "module":
							runnerErr.EntityType = CategoryModule
						case "output":
							runnerErr.EntityType = CategoryOutput
						default:
							if tfLogMessage.Range != nil && strings.HasPrefix(tfLogMessage.Range.Filename, "modules/") {
								runnerErr.EntityType = CategoryModule
								parts := strings.Split(strings.TrimPrefix(tfLogMessage.Range.Filename, "modules/"), "/")
								if len(parts) >= 2 {
									runnerErr.EntityId = parts[0]
									runnerErr.EntityVersion = parts[1]
								}
							} else {
								runnerErr.EntityType = Category(idWithType[0])
							}
						}
					}
					if len(idWithType) >= 2 && runnerErr.EntityId == "" {
						if unquoted, err := strconv.Unquote(idWithType[1]); err == nil {
							idWithType[1] = unquoted
						}
						runnerErr.EntityId = idWithType[1]
					}
					runnerErr.CodeHint = strings.TrimSpace(tfLogMessage.Snippet.Code)
				}
				return runnerErr, nil
			}
		}
		if i <= 0 {
			break
		}
		view = view[:i]
	}
	return nil, nil
}
