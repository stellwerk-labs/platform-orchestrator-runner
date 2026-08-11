# Stellwerk Platform Orchestrator Runner

This is the image executed in customers infra to execute [OpenTofu](https://opentofu.org/).

It is responsible for:

1. Fetching the deployment bundle through the runner gateway.
2. Running `tofu apply`.
3. Inspecting the results.
4. Returning deployment results and encrypted logs through the gateway.

## Remote runner configuration

Remote runners require `RUNNER_GATEWAY_URL`, `ORG_ID`, `RUNNER_ID`, and an
Ed25519 private key in `PRIVATE_KEY`. The agent signs short-lived gateway JWTs;
deployment Jobs receive deployment-scoped tokens. Neither receives NATS
credentials. Use `RUNNER_GATEWAY_CA_FILE` for a private CA and the client
certificate/key settings when the gateway requires mTLS.

The Helm chart supports three deployment modes:

- `simple` connects the runner to the central HTTPS gateway. JetStream remains
  internal to the Orchestrator.
- `edge` uses the same gateway and a persistent HTTPS outbox. The
  chart requires a `ReadWriteMany` storage class because Jobs and the flusher
  may run on different nodes.
- `airgap` runs a small protected-side NATS server and signed file-transfer
  relays on both sides of an external diode. The relay attaches only the bundle
  selected by the exact runner command and only the encrypted log selected by
  its ready event; it never copies a shared Object Store stream wholesale.

The physical diode path is not part of the `v3.0.0` qualification.
Treat `airgap` as unqualified until it has passed a test with the selected diode
device and its exact filesystem semantics.

Diode frames are Ed25519-signed and attachment size is limited to 16 MiB. This
spike therefore preserves the existing buffered log model (runner logs are
limited to 10 MiB); it does not implement live log tailing. Live streaming
would require a separate ordered, resumable chunk protocol.

## Publishing Helm Chart

The Helm chart for this runner is located in the `charts/platform-orchestrator-kubernetes-agent-runner` directory.
To publish a new version of the chart, update the version in `Chart.yaml` and
push a matching `chart-v<version>` tag. The release workflow builds the locked
protected-side NATS dependency, packages the chart, and publishes it to
`ghcr.io/stellwerk-labs/charts/platform-orchestrator-kubernetes-agent-runner`.

Publishing behavior:

- **`chart-v*.*.*` tag**: Publishes only when the tag matches `Chart.yaml`.
- **Manual workflow dispatch**: Can optionally add a `-beta` package suffix.

## Releases

Pushing a signed `v*.*.*` tag builds the AMD64 and ARM64 image, pushes the exact
tag to GHCR, and creates a GitHub release from `docs/releases/<tag>.md`.
