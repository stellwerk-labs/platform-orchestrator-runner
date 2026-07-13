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

## Run golang tests
.PHONY: test-unit
test-unit:
	go tool gotestsum --format testname -- -coverprofile=cover.out ./internal/...
	go vet ./...

