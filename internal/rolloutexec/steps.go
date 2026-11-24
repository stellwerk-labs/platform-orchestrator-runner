package rolloutexec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	steps "github.com/stellwerk-labs/platform-orchestrator-runner/internal/pipelinesteps"
)

func (e *Executor) RunStep(ctx context.Context, request Request, step Step, review, outputs []byte) (steps.Result, error) {
	if e.Validate(request, []Step{step}) != nil {
		return steps.Result{}, errors.New("local task refused")
	}
	tool := e.selectedTool(step.ToolID)
	dir, err := e.workDirectory()
	if err != nil {
		return steps.Result{}, errors.New("local task unavailable")
	}
	defer func() { _ = os.RemoveAll(dir) }()
	inputs, err := taskInputs(dir, request.Manifest, tool.Phase, review, outputs)
	if err != nil {
		return steps.Result{}, errors.New("local task input unavailable")
	}
	effect, _ := json.Marshal([]string{request.OrgID, request.EnvironmentID, request.RunnerID, request.ID, step.ID, step.ToolID, string(step.Parameters)})
	result, err := steps.Execute(ctx, tool.Definition, steps.Request{
		Version: steps.ProtocolVersion, TaskID: step.ID, EffectID: bytesDigest(effect), Inputs: inputs,
		Parameters: step.Parameters, WorkDir: dir,
	}, tool.Environment)
	if err != nil || len(result.Outputs) != 0 {
		return steps.Result{}, errors.New("local task failed or produced unsupported output artifacts")
	}
	return result, nil
}

func (e *Executor) selectedTool(id string) Tool {
	for _, tool := range e.Tools {
		if tool.ID == id {
			return tool
		}
	}
	return Tool{}
}

func taskInputs(dir string, manifest []byte, phase string, review, outputs []byte) (map[string]steps.Input, error) {
	data := map[string][]byte{"manifest": manifest}
	if phase != "pre_plan" && len(review) > 0 {
		data["plan"] = review
	}
	if phase == "post_apply" {
		data["outputs"] = outputs
	}
	inputs := make(map[string]steps.Input, len(data))
	for name, content := range data {
		if !json.Valid(content) {
			return nil, errors.New("invalid local input")
		}
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, content, 0600); err != nil {
			return nil, err
		}
		inputs[name] = steps.Input{Path: path, SHA256: bytesDigest(content)}
	}
	return inputs, nil
}
