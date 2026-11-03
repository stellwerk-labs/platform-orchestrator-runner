# Kubernetes agent runner chart 0.4.0

The chart now defaults to Runner `v3.2.0` through `appVersion`. The empty
`image.tag` value continues to select `appVersion`; explicit image repository
and tag overrides retain their existing behavior. The installation example
selects chart `0.4.0`.

The chart changes release metadata and documentation only; its validation and
publication workflows use the same pinned Helm `4.3.0` tool. Templates,
values, schemas, service accounts, RBAC, transport configuration and the locked
optional NATS `2.14.0` dependency are unchanged. Existing simple, edge and airgap
configuration is preserved. Rendered mode checks do not qualify a physical
diode or replace an installation test in the selected environment.

Publish this chart only after the corresponding stable Runner `v3.2.0` image
and compatible Core set are qualified. Its separate publication is triggered by
`chart-v0.4.0`, not by the Runner `v3.2.0` image tag. The chart publisher packages
the locked dependency and publishes the chart to GHCR plus the GitHub release.
It does not produce Helm provenance files or cryptographic chart signatures.
Verify the exact signed source tag, downloaded package bytes and OCI layer.

The [Runner 3.2.0 vendor disposition](vendor-residuals-v3.2.0.md) describes the
release-specific official OpenTofu residuals. Updating this chart does not
rebuild that engine, clear its findings or change the accepted artifact scope.
