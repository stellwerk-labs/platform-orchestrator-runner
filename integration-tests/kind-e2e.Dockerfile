ARG BASE_IMAGE=ghcr.io/stellwerk-labs/platform-orchestrator-runner:v1.0.3
FROM ${BASE_IMAGE}

COPY --chown=65534:65534 integration-tests/.kind-e2e/runner /opt/runner/runner
