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

KIND_NATS_E2E_CLUSTER ?= po-nats-spike
KIND_NATS_E2E_CONTEXT ?= kind-$(KIND_NATS_E2E_CLUSTER)
KIND_NATS_E2E_GOARCH ?= $(shell go env GOARCH)
KIND_NATS_E2E_IMAGE ?= platform-orchestrator-runner:nats-e2e
KIND_NATS_E2E_URL ?= nats://127.0.0.1:24223
KIND_NATS_E2E_IN_CLUSTER_URL ?= nats://nats.po-nats-e2e.svc.cluster.local:4222

## Build the workspace runner and prove a real NATS-commanded deployment on an existing isolated Kind cluster
.PHONY: test-kind-nats-e2e
test-kind-nats-e2e:
	mkdir -p integration-tests/.kind-e2e
	CGO_ENABLED=0 GOOS=linux GOARCH=$(KIND_NATS_E2E_GOARCH) go build -o integration-tests/.kind-e2e/runner ./cmd/runner
	docker build -f integration-tests/kind-e2e.Dockerfile -t $(KIND_NATS_E2E_IMAGE) .
	kind load docker-image --name $(KIND_NATS_E2E_CLUSTER) $(KIND_NATS_E2E_IMAGE)
	kind export kubeconfig --name $(KIND_NATS_E2E_CLUSTER) --kubeconfig integration-tests/.kind-e2e/kubeconfig.yaml
	PO_NATS_KIND_E2E=1 \
	PO_NATS_KIND_CONTEXT=$(KIND_NATS_E2E_CONTEXT) \
	PO_NATS_KIND_KUBECONFIG=$(CURDIR)/integration-tests/.kind-e2e/kubeconfig.yaml \
	PO_NATS_KIND_RUNNER_IMAGE=$(KIND_NATS_E2E_IMAGE) \
	PO_NATS_KIND_NATS_URL=$(KIND_NATS_E2E_URL) \
	PO_NATS_KIND_NATS_IN_CLUSTER_URL=$(KIND_NATS_E2E_IN_CLUSTER_URL) \
	go test ./integration-tests -run '^TestKindNATSRunnerDeploysKubernetesResource$$' -count=1 -v

## Run golang tests
.PHONY: test-unit
test-unit:
	go tool gotestsum --format testname -- -coverprofile=cover.out ./internal/...
	go vet ./...
