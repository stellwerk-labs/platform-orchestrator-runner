// Package rollout owns site-local rollout execution and durable import authority.
package rollout

import (
	"crypto/ed25519"
	"encoding/json"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"
)

const Version = "stellwerk.rollout/v1"
const PipelineVersion = "stellwerk.pipeline/v1"
const NativePipelineVersion = "stellwerk.pipeline/v2"

type Config struct {
	Postgres   *PostgresConfig
	Directory  string
	OrgID      string
	RunnerID   string
	RunnerUUID string
	PublicKey  ed25519.PublicKey
	Executor   *rolloutexec.Executor
}

type SignedCommand struct {
	Payload   json.RawMessage `json:"payload"`
	Signature string          `json:"signature"`
}

type Approval struct {
	PolicyID      string `json:"policy_id,omitempty"`
	PolicyVersion string `json:"policy_version,omitempty"`
	Mode          string `json:"mode"`
	MaxAdd        int    `json:"max_add"`
	MaxChange     int    `json:"max_change"`
	MaxDestroy    int    `json:"max_destroy"`
}

type RecipeReference struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	Digest  string `json:"digest"`
}

type Command struct {
	ExecutionLimits      *ExecutionLimits       `json:"execution_limits,omitempty"`
	NativeTarget         *NativeExecutionTarget `json:"native_target,omitempty"`
	Recipe               *RecipeReference       `json:"recipe,omitempty"`
	Cost                 *CostPolicy            `json:"cost,omitempty"`
	PlanBase             *PlanBase              `json:"plan_base,omitempty"`
	SourcePlan           *SourcePlan            `json:"source_plan,omitempty"`
	Strategy             string                 `json:"strategy,omitempty"`
	Version              string                 `json:"version"`
	ID                   string                 `json:"id"`
	OrgID                string                 `json:"org_id"`
	RunnerID             string                 `json:"runner_id"`
	RunnerUUID           string                 `json:"runner_uuid,omitempty"`
	EnvironmentID        string                 `json:"environment_id"`
	IssuedAt             time.Time              `json:"issued_at"`
	ExpiresAt            time.Time              `json:"expires_at"`
	Request              rolloutexec.Request    `json:"request"`
	Steps                []rolloutexec.Step     `json:"steps"`
	Approval             Approval               `json:"approval"`
	ToolsDigest          string                 `json:"tools_digest"`
	RequiredCapabilities []string               `json:"required_capabilities"`
}

type PlanBase struct {
	BaseDeploymentID string `json:"base_deployment_id"`
}

type SourcePlan struct {
	SourcePlanID     string `json:"source_plan_id"`
	Subject          string `json:"subject"`
	BaseDeploymentID string `json:"base_deployment_id"`
}

type CostPolicy struct {
	PolicyRevision int64 `json:"policy_revision,omitempty"`
	Enabled        bool  `json:"enabled"`
}

type StepOutcome struct {
	Verdict     string `json:"verdict"`
	Meaning     string `json:"meaning,omitempty"`
	ToolID      string `json:"tool_id,omitempty"`
	ToolName    string `json:"tool_name,omitempty"`
	ToolVersion string `json:"tool_version,omitempty"`
}

type ApprovalDecision struct {
	Source         string    `json:"source"`
	Decision       string    `json:"decision"`
	ActorID        string    `json:"actor_id,omitempty"`
	PolicyID       string    `json:"policy_id,omitempty"`
	PolicyVersion  string    `json:"policy_version,omitempty"`
	DecidedAt      time.Time `json:"decided_at"`
	Reason         string    `json:"reason,omitempty"`
	Subject        string    `json:"subject"`
	EvidenceDigest string    `json:"evidence_digest,omitempty"`
}

type StepResult struct {
	Outcome *StepOutcome `json:"outcome,omitempty"`
	ID      string       `json:"id"`
	Phase   string       `json:"phase"`
	Status  string       `json:"status"`
	Code    string       `json:"code,omitempty"`
}

type Progress struct {
	PostRepair            *PostRepairAuthority   `json:"post_repair,omitempty"`
	LogCapture            *nativelogs.Descriptor `json:"log_capture,omitempty"`
	PlanChanges           *runner.IaCChanges     `json:"plan_changes,omitempty"`
	ApplyChanges          *runner.IaCChanges     `json:"apply_changes,omitempty"`
	Cost                  *runner.CostComparison `json:"cost,omitempty"`
	ApprovalDecision      *ApprovalDecision      `json:"approval_decision,omitempty"`
	InfrastructureApplied bool                   `json:"infrastructure_applied"`
	ID                    string                 `json:"id"`
	EnvironmentID         string                 `json:"environment_id"`
	Phase                 string                 `json:"phase"`
	Subject               string                 `json:"subject,omitempty"`
	ErrorCode             string                 `json:"error_code,omitempty"`
	FailedStep            string                 `json:"failed_step,omitempty"`
	Revision              int64                  `json:"revision"`
	CreatedAt             time.Time              `json:"created_at"`
	Changes               *runner.IaCChanges     `json:"changes,omitempty"`
	Steps                 []StepResult           `json:"steps"`
}

type Event struct {
	ID       int64    `json:"id"`
	Progress Progress `json:"progress"`
}

// Error contains only a bounded public code; private tool errors never cross the API.
type Error struct{ Code string }

func (e *Error) Error() string { return e.Code }

func refused(code string) error { return &Error{Code: code} }
