package platformorchestratorapi

//go:generate go tool oapi-codegen --config=oapi-codegen.cfg.yaml spec.yaml
//go:generate go tool mockgen -destination mocks/client_mock.go -package mockplatformorchestratorapi github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi ClientWithResponsesInterface
