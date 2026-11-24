package rolloutexec

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/config"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
)

func (e *Executor) Plan(ctx context.Context, request Request) (Plan, error) {
	if e.Validate(request, nil) != nil {
		return Plan{}, errors.New("invalid local rollout")
	}
	dir, err := e.workDirectory()
	if err != nil {
		return Plan{}, errors.New("local plan unavailable")
	}
	defer func() { _ = os.RemoveAll(dir) }()
	rn, err := e.prepareRunner(dir, request)
	if err != nil {
		return Plan{}, errors.New("local plan unavailable")
	}
	initLog, err := rn.Init(ctx)
	logs := "=== Initialization ===\n" + initLog
	if err != nil {
		return planWithLogs(Plan{}, logs), errors.New("local initialization failed")
	}
	planLog, planned, changes, err := rn.Plan(ctx)
	logs += "\n=== Plan diagnostics ===\n" + planLog
	if err != nil || changes == nil {
		return planWithLogs(Plan{}, logs), errors.New("local plan failed")
	}
	text, err := rn.PlanText(ctx)
	logs += "\n=== Saved plan ===\n" + text
	if err != nil {
		return planWithLogs(Plan{}, logs), errors.New("local plan rendering failed")
	}
	result := planWithLogs(Plan{}, logs)
	if result.LogsTooLarge {
		return result, errors.New("local plan logs exceed limit")
	}
	result, err = retain(ctx, rn, request, *changes)
	result.PlannedOutputs, _ = json.Marshal(planned)
	return planWithLogs(result, logs), err
}

func retain(ctx context.Context, rn runner.LocalRunner, request Request, changes runner.IaCChanges) (Plan, error) {
	review, err := rn.PlanReview(ctx)
	if err != nil || !json.Valid(review) {
		return Plan{}, errors.New("local plan review failed")
	}
	encoded, err := rn.RetainPlan(ctx, binding(request))
	if err != nil {
		return Plan{}, errors.New("local plan retention failed")
	}
	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return Plan{}, errors.New("local plan retention failed")
	}
	return Plan{Ciphertext: ciphertext, Review: review, Changes: changes}, nil
}

func (e *Executor) Apply(ctx context.Context, request Request, ciphertext []byte, digest string) ([]byte, error) {
	if e.Validate(request, nil) != nil {
		return nil, errors.New("invalid local rollout")
	}
	dir, err := e.workDirectory()
	if err != nil {
		return nil, errors.New("local apply unavailable")
	}
	defer func() { _ = os.RemoveAll(dir) }()
	rn, err := e.runnerForRequest(dir, request)
	if err != nil || rn.RestoreRetainedBytes(ctx, binding(request), ciphertext, digest) != nil {
		return nil, errors.New("local plan restore refused")
	}
	if err := e.stageNativeBackend(dir, request, true); err != nil {
		return nil, err
	}
	if _, _, err := rn.Apply(ctx); err != nil {
		return nil, errors.New("local apply failed")
	}
	outputs, err := rn.Output(ctx)
	if err != nil {
		return nil, errors.New("local outputs unavailable")
	}
	data, err := json.Marshal(outputs)
	if err != nil {
		return nil, errors.New("local outputs unavailable")
	}
	return data, nil
}

func binding(request Request) runner.RetainedBinding {
	binding := runner.RetainedBinding{
		Schema: "stellwerk.retained-plan/v1", OrgID: request.OrgID, EnvironmentID: request.EnvironmentID,
		RunnerID: request.RunnerID, RunnerUUID: request.RunnerUUID, SourceDeploymentID: request.ID, Backend: request.Backend, Tool: request.Tool,
	}
	if request.RunnerUUID != "" {
		binding.Schema = runner.NativeRetainedSchema
	}
	return binding
}

func (e *Executor) workDirectory() (string, error) {
	if !filepath.IsAbs(e.Directory) {
		return "", errors.New("invalid local directory")
	}
	if err := os.MkdirAll(e.Directory, 0700); err != nil {
		return "", err
	}
	info, err := os.Stat(e.Directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("local execution requires private storage")
	}
	return os.MkdirTemp(e.Directory, "execution-")
}

func planWithLogs(plan Plan, logs string) Plan {
	if len(logs) > PlanLogLimit {
		plan.LogsTooLarge = true
		return plan
	}
	plan.Logs = []byte(logs)
	return plan
}

func (e *Executor) prepareRunner(dir string, request Request) (runner.LocalRunner, error) {
	rn, err := e.runnerForRequest(dir, request)
	if err != nil {
		return nil, err
	}
	if len(request.Bundle) > 0 {
		err = rn.StageBundle(request.Bundle)
	} else {
		err = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(request.Configuration), 0600)
	}
	if err == nil {
		err = e.stageNativeBackend(dir, request, false)
	}
	return rn, err
}

func (e *Executor) runnerForRequest(dir string, request Request) (runner.LocalRunner, error) {
	var args []string
	var nativeProxy *string
	if err := e.validateNativeBackend(request); err != nil {
		return nil, err
	}
	if request.Backend == "kubernetes" && len(request.Bundle) > 0 && e.KubernetesBackendConfigPath != "" {
		path := e.KubernetesBackendConfigPath
		if request.RunnerUUID != "" {
			path = nativeBackendFile
			proxy := e.nativeBackend.proxy
			nativeProxy = &proxy
		}
		args = []string{"-backend-config=in_cluster_config=false", "-backend-config=config_path=" + path}
	}
	return runner.NewLocalRunner(runner.LocalOptions{ObservationOnly: request.Mode == "observation", Directory: dir, Backend: config.IaCBackendType(request.Tool), Identity: e.Identity, BackendConfigArgs: args, NativeKubernetesProxy: nativeProxy})
}
