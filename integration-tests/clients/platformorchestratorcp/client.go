package platformorchestratorcp

//go:generate go tool oapi-codegen --config=oapi-codegen.cfg.yaml spec.yaml
//go:generate go tool mockgen -destination mocks/client_mock.go -package mockplatformorchestratorcp github.com/stellwerk-labs/platform-orchestrator-runner/integration-tests/clients/platformorchestratorcp ClientWithResponsesInterface
