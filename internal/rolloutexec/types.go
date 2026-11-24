// Package rolloutexec executes trusted rollout steps using local Runner artifacts.
package rolloutexec

import (
	"encoding/json"

	"filippo.io/age"

	steps "github.com/stellwerk-labs/platform-orchestrator-runner/internal/pipelinesteps"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
)

type Request struct {
	Bundle                    []byte          `json:"bundle,omitempty"`
	BundleSHA256              string          `json:"bundle_sha256,omitempty"`
	Mode                      string          `json:"mode,omitempty"`
	EncryptedLogsRecipient    string          `json:"encrypted_logs_recipient,omitempty"`
	EncryptedOutputsRecipient string          `json:"encrypted_outputs_recipient,omitempty"`
	MetadataKey               string          `json:"metadata_key,omitempty"`
	DeploymentToken           string          `json:"deployment_token,omitempty"`
	ID                        string          `json:"id"`
	OrgID                     string          `json:"org_id"`
	EnvironmentID             string          `json:"environment_id"`
	RunnerID                  string          `json:"runner_id"`
	RunnerUUID                string          `json:"runner_uuid,omitempty"`
	Configuration             string          `json:"configuration,omitempty"`
	Backend                   string          `json:"backend"`
	Tool                      string          `json:"tool"`
	Manifest                  json.RawMessage `json:"manifest"`
}

type Step struct {
	ID         string          `json:"id"`
	ToolID     string          `json:"tool_id"`
	Parameters json.RawMessage `json:"parameters"`
}

type Tool struct {
	ID                   string            `json:"id"`
	Name                 string            `json:"name"`
	Phase                string            `json:"phase"`
	Retry                string            `json:"retry"`
	Definition           steps.Definition  `json:"definition"`
	Environment          map[string]string `json:"environment"`
	RequiredCapabilities []string          `json:"required_capabilities"`
}

const PlanLogLimit = 4 << 20

type Plan struct {
	Cost           *runner.CostComparison
	Logs           []byte
	LogsTooLarge   bool
	Ciphertext     []byte
	Review         []byte
	PlannedOutputs []byte
	Changes        runner.IaCChanges
}

type Executor struct {
	Cost                        *runner.CostConfiguration
	KubernetesBackendConfigPath string
	Directory                   string
	Identity                    *age.X25519Identity
	Tools                       []Tool
	Capabilities                []string
	nativeBackend               *nativeBackend
	catalogueKey                []byte
}
