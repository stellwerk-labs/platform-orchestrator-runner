//go:generate go tool mockgen -destination=mocks/runner.go -package mock_runner github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner RunnerInterface

package runner

import "context"

const IaCLogsChangeSummaryType = "change_summary"

// RunnerInterface defines the operations that any IaC backend must support.
type RunnerInterface interface {
	CreateIaCFolder(ctx context.Context) error
	Version(ctx context.Context) (string, error)
	Init(ctx context.Context) (string, error)
	Plan(ctx context.Context) (string, map[string]interface{}, *IaCChanges, error)
	Apply(ctx context.Context) (string, *IaCChanges, error)
	Destroy(ctx context.Context) (string, *IaCChanges, error)
	Output(ctx context.Context) (map[string]interface{}, error)
	OutputMetadata(ctx context.Context, metadata string) (string, error)
	EncryptString(str string) (string, error)
}

// IaCChanges holds the resource-change counters from a plan/apply/destroy run.
type IaCChanges struct {
	Added  int `json:"add"`
	Change int `json:"change"`
	Import int `json:"import"`
	Remove int `json:"remove"`
}

// IaCLogs represents a single JSON log line emitted by Terraform/OpenTofu
// when run with the -json flag. Only the fields relevant to the runner are decoded.
type IaCLogs struct {
	Level   string     `json:"@level"`
	Message string     `json:"@message"`
	Changes IaCChanges `json:"changes"`
	Type    string     `json:"type"`
}
