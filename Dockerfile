FROM alpine:3.23.4 AS tofu

RUN apk --no-cache add curl

RUN curl --proto '=https' --tlsv1.2 -fsSL https://get.opentofu.org/install-opentofu.sh -o install-opentofu.sh
RUN chmod +x ./install-opentofu.sh
RUN apk add gpg gpg-agent
RUN ./install-opentofu.sh --install-method standalone --opentofu-version 1.10.0 --install-path / --symlink-path -

FROM golang:1.26.2-alpine AS builder

WORKDIR /app

ENV CGO_ENABLED=0 GOOS=linux GOWORK=off

COPY . .

RUN --mount=target=. \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -o /opt/runner/runner ./cmd/runner

FROM builder AS artifact-test-builder

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go test -tags=integration -c -o /opt/runner/artifact-tests ./internal/runner

FROM alpine:3.23.4 AS artifact-integration

RUN apk add --no-cache git
COPY --from=artifact-test-builder /opt/runner/artifact-tests /opt/runner/artifact-tests
COPY --from=tofu /tofu /usr/local/bin/tofu
RUN /opt/runner/artifact-tests -test.run '^TestArtifactExecutionPreservesLegacyHistory$' -test.v

FROM alpine:3.23.4 AS final

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
COPY --chown=${NOBODY_UID}:${NOBODY_GID} --from=builder /opt/runner/runner /opt/runner/runner
COPY --chown=${NOBODY_UID}:${NOBODY_GID} --from=tofu /tofu /usr/local/bin/tofu
ENTRYPOINT ["/opt/runner/runner"]
