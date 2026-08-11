# Kubernetes agent runner chart 0.3.0

This breaking chart release deploys runner `v3.0.0` and standardizes the
runner-facing protocol on outbound HTTPS. Runner agents and deployment Jobs no
longer receive NATS endpoints or broker credentials.

All modes use an Ed25519 runner identity stored in a Kubernetes Secret:

- `simple` connects to the central HTTPS runner gateway and relies on central
  JetStream for durable command buffering.
- `edge` adds a persistent outbound result and encrypted-log outbox. It
  requires an RWX-capable StorageClass on multi-node clusters.
- `airgap` deploys a protected-side gateway and JetStream. Signed file relays
  transfer commands and bundles into the compartment and results and logs back
  through a separately authorized return path.

Private gateway CAs and mutual TLS are supported. The chart validates mode,
identity, gateway URL, protected-side Secret, and outbox combinations before it
renders.

Simple, edge, and air-gap manifests were linted and rendered. Simple and edge
were exercised with a real local gateway and runner deployment. The physical
diode path was not tested and is not qualified by this release.
