# Kubernetes agent runner chart 0.4.1

The chart now defaults to Runner `v3.3.1` through `appVersion` (chart `0.4.0`
selected `v3.2.0`). The empty `image.tag` value continues to select
`appVersion`; explicit image repository and tag overrides retain their existing
behavior. The installation example selects chart `0.4.1`.

Templates, values, schemas, RBAC, transport configuration and the locked
optional NATS `2.14.0` dependency are unchanged from `0.4.0`. Upgrading the
chart without an explicit `image.tag` moves the Runner from `v3.2.0` to
`v3.3.1`, so it also brings the [Runner 3.3.0](v3.3.0.md) changes: optional,
Admin-enabled cost estimation (disabled by default). The
[Runner 3.3.1 notes](v3.3.1.md) describe the security patch and the newer
bundled OpenTofu `1.13.10`.

The chart publisher packages the locked dependency and publishes the chart to
GHCR plus the GitHub release. It does not produce Helm provenance files or
cryptographic chart signatures. Verify the exact signed source tag, downloaded
package bytes and OCI layer.
