ARG RUNNER_IMAGE=stellwerk-labs/platform-orchestrator-runner:test
FROM ${RUNNER_IMAGE} AS mirror

USER root
COPY integration-tests/provider-mirror/main.tf /tmp/provider-mirror/main.tf
RUN tofu -chdir=/tmp/provider-mirror providers mirror /opt/runner/test-providers

FROM ${RUNNER_IMAGE}
COPY --from=mirror /opt/runner/test-providers /opt/runner/test-providers
COPY integration-tests/provider-mirror/installation.tfrc /opt/runner/test-provider-installation.tfrc
ENV TF_CLI_CONFIG_FILE=/opt/runner/test-provider-installation.tfrc
