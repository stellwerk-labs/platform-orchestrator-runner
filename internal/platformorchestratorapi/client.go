package platformorchestratorapi

//go:generate go tool oapi-codegen --config=oapi-codegen.cfg.yaml spec.yaml
//go:generate go tool mockgen -destination mocks/client_mock.go -package mockplatformorchestratorapi github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi ClientWithResponsesInterface

// Preserve the runner client's established mode names when upstream adds other
// mode enums and the generator qualifies their constants to avoid collisions.
const (
	Deploy   = DeploymentCreateBodyModeDeploy
	PlanOnly = DeploymentCreateBodyModePlanOnly
	Rollback = DeploymentCreateBodyModeRollback
)
