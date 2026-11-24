package rolloutexec

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"filippo.io/age"
	api "github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/utils"
)

type Outcome struct {
	Outputs []byte
	Result  api.DeploymentResultsUpdateBody
	Logs    []byte
	Changes *runner.IaCChanges
	Started bool
	Applied bool
}

// Execute applies the admitted closure without manufacturing a saved-plan gate.
func (e *Executor) Execute(ctx context.Context, request Request, strategy string, beforeEffect func() error) (out Outcome, err error) {
	if err = e.Validate(request, nil); err != nil {
		return out, err
	}
	dir, err := e.workDirectory()
	if err != nil {
		return out, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	rn, err := e.prepareRunner(dir, request)
	if err != nil {
		return out, err
	}
	initLog, err := rn.Init(ctx)
	out.Logs = []byte("=== Initialization ===\n" + initLog)
	if err != nil {
		return out, errors.New("initialization failed")
	}
	if err = beforeEffect(); err != nil {
		return out, err
	}
	out.Started = true
	var log string
	if strategy == "destroy" {
		log, out.Changes, err = rn.Destroy(ctx)
	} else {
		log, out.Changes, err = rn.ApplyDirect(ctx)
	}
	out.Logs = append(out.Logs, []byte("\n=== Execution ===\n"+log)...)
	if err != nil {
		return out, errors.New("execution outcome requires reconciliation")
	}
	out.Applied = true
	if strategy == "destroy" {
		out.Outputs = []byte(`{}`)
		out.Result, err = Result(request, out.Outputs, out.Changes, false)
		return out, err
	}
	return collectOutcome(ctx, rn, request, out)
}

func (e *Executor) ApplySaved(ctx context.Context, request Request, ciphertext []byte, digest string) (out Outcome, err error) {
	return e.ApplySavedFrom(ctx, request, request, ciphertext, digest, nil)
}

// ApplySavedFrom restores source authority while keeping result ownership with the child.
func (e *Executor) ApplySavedFrom(ctx context.Context, request, source Request, ciphertext []byte, digest string, beforeEffect func() error) (out Outcome, err error) {
	if !ValidRunnerUUID(request.RunnerUUID) || !ValidRunnerUUID(source.RunnerUUID) || request.RunnerUUID != source.RunnerUUID {
		return out, errors.New("saved plan authority refused")
	}
	dir, err := e.workDirectory()
	if err != nil {
		return out, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	rn, err := e.runnerForRequest(dir, source)
	if err != nil || rn.RestoreRetainedBytes(ctx, binding(source), ciphertext, digest) != nil {
		return out, errors.New("saved plan restore refused")
	}
	if err = e.stageNativeBackend(dir, source, true); err != nil {
		return out, err
	}
	if beforeEffect != nil {
		if err = beforeEffect(); err != nil {
			return out, err
		}
	}
	out.Started = true
	log, changes, err := rn.Apply(ctx)
	out.Logs = []byte(log)
	out.Changes = changes
	if err != nil {
		return out, errors.New("execution outcome requires reconciliation")
	}
	out.Applied = true
	return collectOutcome(ctx, rn, request, out)
}

func collectOutcome(ctx context.Context, rn runner.LocalRunner, request Request, out Outcome) (Outcome, error) {
	outputs, err := rn.Output(ctx)
	if err != nil {
		return out, errors.New("outputs unavailable")
	}
	out.Outputs, err = json.Marshal(outputs)
	if err != nil {
		return out, err
	}
	out.Result, err = Result(request, out.Outputs, out.Changes, false)
	return out, err
}

// Result uses the admitted recipient and metadata contract, independently of site encryption.
func Result(request Request, raw []byte, changes *runner.IaCChanges, plan bool) (api.DeploymentResultsUpdateBody, error) {
	result := api.DeploymentResultsUpdateBody{Status: api.Success}
	if changes != nil {
		result.TfResourceCounts = api.DeploymentTFResourceCounts{NumResourcesAdded: changes.Added, NumResourcesChanged: changes.Change, NumResourcesRemoved: changes.Remove}
	}
	var outputs map[string]interface{}
	if json.Unmarshal(raw, &outputs) != nil {
		return result, errors.New("invalid execution outputs")
	}
	key := request.MetadataKey
	if key == "" {
		key = "platform_orchestrator_metadata"
	}
	if !plan && (request.Mode == "deploy" || request.Mode == "rollback") {
		var err error
		result.Metadata, err = executionMetadata(outputs[key])
		if err != nil {
			return result, err
		}
	}
	delete(outputs, key)
	if request.EncryptedOutputsRecipient == "" {
		return result, nil
	}
	recipient, err := age.ParseX25519Recipient(request.EncryptedOutputsRecipient)
	if err != nil {
		return result, err
	}
	encoded, err := json.Marshal(outputs)
	if err != nil {
		return result, err
	}
	encrypted, err := utils.EncryptBytes(encoded, recipient)
	if err != nil {
		return result, err
	}
	result.Outputs = &encrypted
	return result, nil
}

func executionMetadata(value interface{}) ([]api.DeploymentResultMetadataPerNode, error) {
	if value == nil {
		return nil, errors.New("execution metadata unavailable")
	}
	encoded, _ := json.Marshal(value)
	var nodes map[string]map[string]interface{}
	if json.Unmarshal(encoded, &nodes) != nil {
		return nil, errors.New("invalid execution metadata")
	}
	metadata := make([]api.DeploymentResultMetadataPerNode, 0, len(nodes))
	for node, fields := range nodes {
		metadata = append(metadata, api.DeploymentResultMetadataPerNode{NodeId: node, Metadata: fields})
	}
	return metadata, nil
}
