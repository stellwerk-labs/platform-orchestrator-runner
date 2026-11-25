# Disable all the default make stuff
MAKEFLAGS += --no-builtin-rules
.SUFFIXES:

## Display help menu
.PHONY: help
help:
	@echo Documented Make targets:
	@perl -e 'undef $$/; while (<>) { while ($$_ =~ /## (.*?)(?:\n# .*)*\n.PHONY:\s+(\S+).*/mg) { printf "\033[36m%-30s\033[0m %s\n", $$2, $$1 } }' $(MAKEFILE_LIST) | sort

.PHONY: .ALWAYS
.ALWAYS:

## Generate mocks
.PHONY: generate
generate:
	go generate -v ./...

.PHONY: prepare-integration-tests
prepare-integration-tests:
	$(MAKE) -C integration-tests prepare-tests

## Teardown any docker compose containers
.PHONY: clean
clean:
	$(MAKE) -C integration-tests clean

## Run integration tests, starting docker compose containers if necessary
.PHONY: test-integration
test-integration: prepare-integration-tests
	$(MAKE) -C integration-tests test

.PHONY:
test-integration-logs:
	$(MAKE) -C integration-tests test-integration-logs

KIND_GATEWAY_E2E_CLUSTER ?= po-nats-spike
KIND_GATEWAY_E2E_CONTEXT ?= kind-$(KIND_GATEWAY_E2E_CLUSTER)
KIND_GATEWAY_E2E_GOARCH ?= $(shell go env GOARCH)
KIND_GATEWAY_E2E_IMAGE ?= platform-orchestrator-runner:gateway-e2e
KIND_GATEWAY_E2E_NATS_URL ?= nats://127.0.0.1:24223
KIND_GATEWAY_E2E_NATS_IN_CLUSTER_URL ?= nats://nats.po-nats-e2e.svc.cluster.local:4222

## Build the workspace runner and prove a real HTTPS-gateway deployment on an existing isolated Kind cluster
.PHONY: test-kind-gateway-e2e
test-kind-gateway-e2e:
	mkdir -p integration-tests/.kind-e2e
	CGO_ENABLED=0 GOOS=linux GOARCH=$(KIND_GATEWAY_E2E_GOARCH) go build -o integration-tests/.kind-e2e/runner ./cmd/runner
	docker build -f integration-tests/kind-e2e.Dockerfile -t $(KIND_GATEWAY_E2E_IMAGE) .
	kind load docker-image --name $(KIND_GATEWAY_E2E_CLUSTER) $(KIND_GATEWAY_E2E_IMAGE)
	kind export kubeconfig --name $(KIND_GATEWAY_E2E_CLUSTER) --kubeconfig integration-tests/.kind-e2e/kubeconfig.yaml
	PO_GATEWAY_KIND_E2E=1 \
	PO_GATEWAY_KIND_CONTEXT=$(KIND_GATEWAY_E2E_CONTEXT) \
	PO_GATEWAY_KIND_KUBECONFIG=$(CURDIR)/integration-tests/.kind-e2e/kubeconfig.yaml \
	PO_GATEWAY_KIND_RUNNER_IMAGE=$(KIND_GATEWAY_E2E_IMAGE) \
	PO_GATEWAY_KIND_NATS_URL=$(KIND_GATEWAY_E2E_NATS_URL) \
	PO_GATEWAY_KIND_NATS_IN_CLUSTER_URL=$(KIND_GATEWAY_E2E_NATS_IN_CLUSTER_URL) \
	go test ./integration-tests -run '^TestKindGatewayRunnerDeploysKubernetesResource$$' -count=1 -v

## Run golang tests
.PHONY: test-unit
test-unit:
	go tool gotestsum --format testname -- -coverprofile=cover.out ./internal/... ./cmd/...
	go vet ./...

## Execute legacy artifact carry-forward and history rollback with installed OpenTofu (IAC_TEST_BINARIES=tofu,terraform for both backends)
.PHONY: test-artifact-integration
test-artifact-integration:
	go test -tags=integration ./internal/runner -run '^TestArtifactExecutionPreservesLegacyHistory$$' -count=1 -v

## Prove safe full-plan observations with real installed Terraform/OpenTofu and local/HTTP state backends
.PHONY: test-observation-integration
test-observation-integration:
	go test -tags=integration ./internal/runner ./internal/executor -run '^TestObservation' -count=1 -v -timeout=230s

## Prove native frozen delivery and action errors with real isolated Gateway/NATS and OpenTofu
.PHONY: test-agent-delivery-integration
test-agent-delivery-integration:
	sh scripts/test-agent-delivery-integration.sh

## Qualify explicit PostgreSQL rollout storage with real DBOS, OpenTofu and reconnect faults (requires RUNNER_POSTGRES_TEST_DSN_FILE)
.PHONY: test-rollout-postgres
test-rollout-postgres:
	go test -race -tags=integration ./internal/rollout -run '^TestPostgres' -count=1 -v -timeout=15m

## Prove real Terraform/OpenTofu Kubernetes backend portability using an explicit owned test-admin config
.PHONY: test-native-kubernetes-backend
test-native-kubernetes-backend:
	@test -n "$${NATIVE_BACKEND_ADMIN_CONFIG:-}" && test -f "$$NATIVE_BACKEND_ADMIN_CONFIG"
	@command -v tofu >/dev/null
	@command -v terraform >/dev/null
	@tests="$$(go test -tags=integration ./internal/rolloutexec -list '^TestNativeKubernetesBackendCrossWorkerSavedPlan$$')" || exit $$?; \
	case "$$tests" in *TestNativeKubernetesBackendCrossWorkerSavedPlan*) ;; *) echo 'Real Kubernetes backend qualification test is missing' >&2; exit 1;; esac
	go test -race -tags=integration ./internal/rolloutexec -run '^TestNativeKubernetesBackendCrossWorkerSavedPlan$$' -count=1 -v -timeout=180s

## Qualify shared pipeline queue/fencing with owned PostgreSQL/NATS and separate real CLI workers
.PHONY: test-rollout-ha-integration
test-rollout-ha-integration:
	sh scripts/test-rollout-ha-integration.sh

## Refuse native custody mutation on an owned unsupported NATS release while preserving legacy delivery
.PHONY: test-unsupported-native-nats
test-unsupported-native-nats:
	sh scripts/test-unsupported-native-nats.sh

## Refuse actual retained format6 history without recovery (requires explicit old and new qualified binaries plus owned HA services)
.PHONY: test-native-command-format
test-native-command-format:
	@test -n "$${NATIVE_FORMAT6_RUNNER_BINARY:-}" && test -x "$$NATIVE_FORMAT6_RUNNER_BINARY"
	@test -n "$${HA_RUNNER_BINARY:-}" && test -x "$$HA_RUNNER_BINARY"
	go test -race -tags=integration ./internal/rolloutagent -run '^TestNativeFormatSevenRefusesActualFormatSixHistory$$' -count=1 -timeout=90s -v

## Execute exact DP-produced Native OpenTofu commands with owned PG/NATS/Kubernetes fixtures
.PHONY: test-native-command-contract
test-native-command-contract:
	@test -n "$${D2_DP_API_TEST_BINARY:-}" && test -x "$$D2_DP_API_TEST_BINARY"
	@test -n "$${HA_RUNNER_BINARY:-}" && test -x "$$HA_RUNNER_BINARY"
	@test -n "$${NATIVE_BACKEND_ADMIN_CONFIG:-}" && test -f "$$NATIVE_BACKEND_ADMIN_CONFIG"
	@test -n "$${NATIVE_COMMAND_EVIDENCE_DIRECTORY:-}" && test -d "$$NATIVE_COMMAND_EVIDENCE_DIRECTORY"
	go test -race -tags=integration ./internal/rolloutagent -run '^TestNativeDPProducerExecutesOpenTofuOnKubernetes$$' -count=1 -timeout=90s -v

## Prove separately authored Terraform Native Engine source/child authority on owned PostgreSQL
.PHONY: test-native-standalone-artifacts
test-native-standalone-artifacts:
	go test -race -tags=integration ./internal/rollout -run '^TestPostgresStandaloneNativeTerraformSavedPlan$$|^TestPostgresNativeSavedSourceKeepsOpaqueTargetDigest$$' -count=1 -timeout=90s -v
