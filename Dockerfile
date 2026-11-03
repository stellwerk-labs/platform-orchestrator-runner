FROM alpine:3.23.7@sha256:eaa20f3de212fa5cda0c64dc715b55ce28619928536d0d2a97ec1a922c1edce8 AS tofu

RUN apk --no-cache add curl gpg gpg-agent

RUN curl --proto '=https' --tlsv1.2 -fsSL \
    https://raw.githubusercontent.com/opentofu/get.opentofu.org/3d354dfa62d9a36f33df16cdd6bb506ace1e6e2e/static/install-opentofu.sh \
    -o install-opentofu.sh \
    && echo 'e4e899ae7bd86241fd17ad99e9d32f5b2d20a11b1a40d088acabb6e06dba49c5  install-opentofu.sh' | sha256sum -c - \
    && chmod +x ./install-opentofu.sh
RUN ./install-opentofu.sh --install-method standalone --opentofu-version 1.13.2 \
    --gpg-key-id E3E6E43D84CB852EADB0051D0C0AF313E5FD9F80 \
    --install-path / --symlink-path -

FROM tofu AS legacy-tofu

RUN ./install-opentofu.sh --install-method standalone --opentofu-version 1.10.0 \
    --gpg-key-id E3E6E43D84CB852EADB0051D0C0AF313E5FD9F80 \
    --install-path /legacy --symlink-path -

FROM golang:1.26.10-alpine@sha256:5cb67f19c5e7994dcb80c074f7cfca8aeeada45bbdca035f73df269cefdcce61 AS builder

WORKDIR /app

ENV CGO_ENABLED=0 GOOS=linux GOWORK=off

COPY . .

RUN --mount=target=. \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -o /opt/runner/runner ./cmd/runner

FROM builder AS security

ARG RELEASE_VERSION
COPY --from=tofu /tofu /opt/runner/tofu
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    sh scripts/check-go-security.sh binary /opt/runner/runner \
    && sh scripts/check-go-security.sh vendor-binary "$RELEASE_VERSION" /opt/runner/tofu

FROM builder AS artifact-test-builder

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go test -tags=integration -c -o /opt/runner/artifact-tests ./internal/runner

FROM alpine:3.23.7@sha256:eaa20f3de212fa5cda0c64dc715b55ce28619928536d0d2a97ec1a922c1edce8 AS artifact-integration

RUN apk add --no-cache git
COPY --from=artifact-test-builder /opt/runner/artifact-tests /opt/runner/artifact-tests
COPY --from=tofu /tofu /usr/local/bin/tofu
COPY --from=legacy-tofu /legacy/tofu /opt/runner/legacy-tofu
RUN --network=none IAC_LEGACY_BINARY=/opt/runner/legacy-tofu \
    /opt/runner/artifact-tests -test.run '^TestArtifactExecutionPreservesLegacyHistory$' -test.v
RUN --network=none IAC_TEST_BINARIES=tofu \
    /opt/runner/artifact-tests -test.run '^TestObservationHTTPBackendLocksCancellationAndNoStateWrites$' -test.v

FROM alpine:3.23.7@sha256:eaa20f3de212fa5cda0c64dc715b55ce28619928536d0d2a97ec1a922c1edce8 AS final

LABEL org.opencontainers.image.source="https://github.com/stellwerk-labs/platform-orchestrator-runner"

RUN apk add --no-cache git openssh-client

ARG NOBODY_UID=65534
ARG NOBODY_GID=65534

WORKDIR /opt/runner
# Precreate and chown number of local cache paths that are commonly written to by Terraform providers for example, the
# Helm provider uses .cache. For security, the customer may run with readOnlyRootFileSystem and mount emptyDir to these
# locations via the runner configuration.
RUN mkdir /.cache /.config /.local /opt/runner/tofu
RUN chown ${NOBODY_UID}:${NOBODY_GID} /.cache /.config /.local /opt/runner/tofu

USER ${NOBODY_UID}
COPY --chown=${NOBODY_UID}:${NOBODY_GID} --from=security /opt/runner/runner /opt/runner/runner
COPY --chown=${NOBODY_UID}:${NOBODY_GID} --from=security /opt/runner/tofu /usr/local/bin/tofu
ENTRYPOINT ["/opt/runner/runner"]
