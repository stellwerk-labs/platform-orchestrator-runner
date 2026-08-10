# Stellwerk Platform Orchestrator Runner

This is the image executed in customers infra to execute [OpenTofu](https://opentofu.org/).

It is responsible for:

1. Fetching the deployment bundle from the `PO_RUNNER_BUNDLES` NATS Object Store.
2. Running `tofu apply`.
3. Inspecting the results.
4. Publishing deployment results and encrypted logs through NATS JetStream.

## Remote runner configuration

Remote runners require `NATS_URL`, `ORG_ID`, and `RUNNER_ID`. Authentication is
provided with `NATS_TOKEN`, `NATS_CREDS_FILE`, or mTLS. `NATS_CA_FILE` pins a
private CA. Production runners bind pre-provisioned streams and buckets; only
development environments should set `NATS_BOOTSTRAP_STREAMS=true`.

The Helm chart supports three deployment modes:

- `simple` connects the runner to the central JetStream cluster.
- `edge` uses the same central cluster and a persistent outbound spool. The
  chart requires a `ReadWriteMany` storage class because Jobs and the flusher
  may run on different nodes.
- `airgap` runs a small protected-side NATS server and signed file-transfer
  relays on both sides of an external diode. The relay attaches only the bundle
  selected by the exact runner command and only the encrypted log selected by
  its ready event; it never copies a shared Object Store stream wholesale.

The physical diode path was not tested as part of the `v2.0.0` qualification.
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
NATS dependency, packages the chart, and publishes it to
`ghcr.io/stellwerk-labs/charts/platform-orchestrator-kubernetes-agent-runner`.

Publishing behavior:

- **`chart-v*.*.*` tag**: Publishes only when the tag matches `Chart.yaml`.
- **Manual workflow dispatch**: Can optionally add a `-beta` package suffix.

## Releases

Pushing a signed `v*.*.*` tag builds the AMD64 and ARM64 image, pushes the exact
tag to GHCR, and creates a GitHub release from `docs/releases/<tag>.md`.
