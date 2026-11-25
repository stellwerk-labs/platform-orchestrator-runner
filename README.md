# Stellwerk Platform Orchestrator Runner

This is the image executed in customers infra to execute [OpenTofu](https://opentofu.org/).

It is responsible for:

1. Fetching the deployment bundle through the runner gateway.
2. Running `tofu apply`.
3. Inspecting the results.
4. Returning deployment results and encrypted logs through the gateway.

## Deployment Pipelines availability (RC)

Deployment Pipelines are an RC feature in [v3.4.0-rc.4](docs/releases/v3.4.0-rc.4.md),
part of the coordinated Pipeline release candidate. The current stable release
from `main` does not include Pipelines. Use the matching RC services and clients
when following Pipeline documentation.

Upgrading alone does not activate Native execution or change existing deployment
behavior. On an explicitly enabled Pipeline installation, the built-in **Apply**
default runs Apply directly, with no additional Plan, approval, pre-deployment or
post-deployment steps. Planning remains an explicit action or part of a selected
recipe. Publishing a recipe alone does not change any default; an authorized
operator must select it or update deployment-use policy.

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

Validate air-gapped transport with the selected physical diode device and its
exact filesystem semantics before production use.

Diode frames are Ed25519-signed and attachment size is limited to 16 MiB.
Runner logs use the buffered log model and are limited to 10 MiB. Live log
tailing is not supported over this transport.

## Publishing Helm Chart

The Helm chart for this runner is located in the `charts/platform-orchestrator-kubernetes-agent-runner` directory.
To publish a new version of the chart, update the version in `Chart.yaml` and
push a matching `chart-v<version>` tag. The release workflow builds the locked
protected-side NATS dependency, packages the chart, and publishes it to
`ghcr.io/stellwerk-labs/charts/platform-orchestrator-kubernetes-agent-runner`.

For the Pipeline RC, use chart `0.5.0-rc.4` with Runner image `v3.4.0-rc.4`
explicitly selected. Native mode is opt-in; do not rely on a historical default
image tag to enable the RC.
Runner `v*.*.*` tags do not trigger the chart publisher.

Publishing behavior:

- **`chart-v*.*.*` tag**: Publishes only when the tag matches `Chart.yaml`.
- **Manual workflow dispatch**: Can optionally add a `-beta` package suffix.

## Releases

Pushing a signed `v*.*.*` tag builds the AMD64 and ARM64 image, pushes the exact
tag to GHCR, and creates a GitHub release from `docs/releases/<tag>.md`.

## Infrastructure cost estimates

Runner can log advisory monthly infrastructure cost estimates after successful manual and Drift plan inspection. Estimation defaults to disabled and requires installation Admin enablement and a customer-owned Infracost API key. See [cost estimation](docs/cost-estimation.md).
