package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	api "github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
)

const maxObservationPlanBytes = 32 << 20
const maxObservationChanges = 2000

// ObservationRunner is optional, so older factories cannot accidentally execute a deployment.
type ObservationRunner interface {
	Observe(context.Context) (*api.ObservationResult, error)
}

var observationVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
var observationResourceType = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,127}$`)

// Observe runs a normal refreshing plan. A saved plan is never applied or retained.
func (r *baseRunner) Observe(ctx context.Context) (result *api.ObservationResult, err error) {
	if !r.observationOnly || (r.tool != "terraform" && r.tool != "opentofu") {
		return nil, errors.New("observation mode is unsupported")
	}
	planDir, err := os.MkdirTemp(r.folder, ".observation-")
	if err != nil {
		return nil, errors.New("observation plan storage unavailable")
	}
	defer func() {
		if cleanupErr := os.RemoveAll(planDir); cleanupErr != nil {
			result, err = nil, errors.New("observation plan cleanup failed")
		}
	}()
	planFile := filepath.Join(planDir, "plan.tfplan")
	flags := []string{}
	if r.tool == "opentofu" {
		flags = append(flags, "-consolidate-errors")
	}
	initArgs := append([]string{"-chdir=" + r.folder, "init", "-input=false", "-no-color", "-json", "-compact-warnings"}, flags...)
	if _, err := r.observationCommand(ctx, 0, initArgs...); err != nil {
		return nil, errors.New("observation initialization failed")
	}
	if err := verifyArtifactManifest(r.folder); err != nil {
		return nil, errors.New("observation artifact verification failed")
	}
	args := append([]string{"-chdir=" + r.folder, "plan", "-input=false", "-refresh=true", "-lock=true", "-lock-timeout=0s", "-no-color", "-json", "-compact-warnings", "-out=" + planFile}, flags...)
	if _, err := r.observationCommand(ctx, 0, args...); err != nil {
		return nil, errors.New("observation plan failed")
	}
	data, err := r.observationCommand(ctx, maxObservationPlanBytes, "-chdir="+r.folder, "show", "-no-color", "-json", planFile)
	if err != nil {
		return nil, errors.New("observation plan inspection failed")
	}
	result, err = parseObservationPlan(data, r.tool)
	if ctx.Err() != nil {
		return nil, errors.New("observation canceled")
	}
	if err == nil {
		r.logDiskEstimate(ctx, data)
	}
	return result, err
}

// Raw diagnostics and process errors must never cross the observation boundary.
func (r *baseRunner) observationCommand(ctx context.Context, limit int, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.binaryPath, args...)
	cmd.WaitDelay = 2 * time.Second
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if strings.HasPrefix(key, "TF_CLI_ARGS") || strings.HasPrefix(key, "TF_LOG") || strings.HasPrefix(key, "TOFU_LOG") {
			continue
		}
		cmd.Env = append(cmd.Env, env)
	}
	cmd.Stderr = io.Discard
	var out boundedObservationBuffer
	out.remaining = limit
	if limit > 0 {
		cmd.Stdout = &out
	} else {
		cmd.Stdout = io.Discard
	}
	if err := cmd.Run(); err != nil || ctx.Err() != nil || out.exceeded {
		return nil, errors.New("observation command failed")
	}
	return out.buffer.Bytes(), nil
}

type boundedObservationBuffer struct {
	buffer    bytes.Buffer
	remaining int
	exceeded  bool
}

func (b *boundedObservationBuffer) Write(p []byte) (int, error) {
	if len(p) > b.remaining {
		b.exceeded = true
		return 0, errors.New("observation size limit exceeded")
	}
	b.remaining -= len(p)
	return b.buffer.Write(p)
}

type observationPlanChange struct {
	Type   string `json:"type"`
	Change *struct {
		Actions   []string        `json:"actions"`
		Importing json.RawMessage `json:"importing"`
	} `json:"change"`
}

type observationPlan struct {
	Format        string          `json:"format_version"`
	Version       string          `json:"terraform_version"`
	Complete      *bool           `json:"complete"`
	Errored       *bool           `json:"errored"`
	Configuration json.RawMessage `json:"configuration"`
	PlannedValues json.RawMessage `json:"planned_values"`
	Deferred      json.RawMessage `json:"deferred_changes"`
	Checks        []struct {
		Status string `json:"status"`
	} `json:"checks"`
	Drift   []observationPlanChange `json:"resource_drift"`
	Changes []observationPlanChange `json:"resource_changes"`
	Outputs map[string]struct {
		Actions []string `json:"actions"`
	} `json:"output_changes"`
}

func parseObservationPlan(data []byte, tool string) (*api.ObservationResult, error) {
	invalid := errors.New("observation plan is invalid or unsupported")
	if len(data) == 0 || len(data) > maxObservationPlanBytes || (tool != "terraform" && tool != "opentofu") {
		return nil, invalid
	}
	// Reject duplicate keys too: accepting a second complete/errored field can hide a partial plan.
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := validateObservationJSON(decoder); err != nil {
		return nil, invalid
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, invalid
	}
	var plan observationPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return nil, invalid
	}
	if plan.Format != "1.2" || len(plan.Version) > 64 || !observationVersion.MatchString(plan.Version) || plan.Errored == nil || *plan.Errored || !jsonObject(plan.Configuration) || !jsonObject(plan.PlannedValues) {
		return nil, invalid
	}
	// OpenTofu 1.x has no complete field. Successful unrestricted full plan execution
	// and absence of deferred changes establish completeness for its 1.2 format.
	if (tool == "terraform" && plan.Complete == nil) || (plan.Complete != nil && !*plan.Complete) {
		return nil, invalid
	}
	if len(plan.Deferred) > 0 && string(plan.Deferred) != "[]" && string(plan.Deferred) != "null" {
		return nil, invalid
	}
	for _, check := range plan.Checks {
		if check.Status != "pass" {
			return nil, invalid
		}
	}
	result := &api.ObservationResult{SchemaVersion: "1", Complete: true, Tool: api.ObservationResultTool(tool), ToolVersion: plan.Version, ObservedChanges: []api.ObservationChange{}, ProposedChanges: []api.ObservationChange{}}
	var err error
	if result.ObservedChanges, err = sanitizeObservationChanges(plan.Drift, "observed"); err != nil {
		return nil, invalid
	}
	if result.ProposedChanges, err = sanitizeObservationChanges(plan.Changes, "proposed"); err != nil {
		return nil, invalid
	}
	if len(plan.Outputs) > 1000000 {
		return nil, invalid
	}
	for _, output := range plan.Outputs {
		if !validObservationActions(output.Actions) {
			return nil, invalid
		}
		if !(len(output.Actions) == 1 && output.Actions[0] == "no-op") {
			result.OutputChangeCount++
		}
	}
	return result, nil
}

func jsonObject(data json.RawMessage) bool { return len(data) > 1 && data[0] == '{' }

func sanitizeObservationChanges(changes []observationPlanChange, prefix string) ([]api.ObservationChange, error) {
	if len(changes) > maxObservationChanges {
		return nil, errors.New("too many observation changes")
	}
	result := make([]api.ObservationChange, 0, len(changes))
	for _, change := range changes {
		if !observationResourceType.MatchString(change.Type) || change.Change == nil || !validObservationActions(change.Change.Actions) || (len(change.Change.Importing) > 0 && string(change.Change.Importing) != "null") {
			return nil, errors.New("unsupported observation change")
		}
		if len(change.Change.Actions) == 1 && change.Change.Actions[0] == "no-op" {
			continue
		}
		actions := make([]api.ObservationChangeActions, len(change.Change.Actions))
		for i, action := range change.Change.Actions {
			actions[i] = api.ObservationChangeActions(action)
		}
		result = append(result, api.ObservationChange{Id: fmt.Sprintf("%s_%d", prefix, len(result)+1), ResourceType: change.Type, Actions: actions})
	}
	return result, nil
}

func validObservationActions(actions []string) bool {
	if len(actions) == 2 {
		return (actions[0] == "create" && actions[1] == "delete") || (actions[0] == "delete" && actions[1] == "create")
	}
	if len(actions) != 1 {
		return false
	}
	switch actions[0] {
	case "create", "read", "update", "delete", "no-op", "forget":
		return true
	}
	return false
}

func validateObservationJSON(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("duplicate JSON key")
			}
			seen[name] = true
			if err := validateObservationJSON(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := validateObservationJSON(decoder); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}
