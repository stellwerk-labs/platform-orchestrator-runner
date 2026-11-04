# Runner security maintenance

Unreleased. These notes describe the local maintenance branch, not an available
artifact or a completed deployment.

## Security changes

Official builds use Go 1.26.10, including the certificate verification fix for
[CVE-2026-111943](https://pkg.go.dev/vuln/GO-2026-6809).

Existing HTTP, cryptography, OpenAPI and Kubernetes client dependencies are updated to patched versions. The Kubernetes API, machinery and client packages use one compatible version. Legacy artifact carry-forward and exact history rollback retain their existing contracts.

The bundled OpenTofu executable advances from 1.10.0 to
[1.13.2](https://github.com/opentofu/opentofu/releases/tag/v1.13.2), which includes
the compromised-intermediate certificate fix and an HTTP/2 resource-exhaustion
fix during dependency installation. This is an engine minor-version upgrade,
not just a Runner dependency update. The installer is pinned to an immutable
source commit and checked against its SHA-256 digest before execution. It
verifies the release checksum signature against the expected OpenTofu GPG key.
Neither check may be skipped.

The runtime, installer and artifact-test stages use Alpine 3.23.7 on the existing
3.23 release line, pinned by the multi-platform image index digest.

The Docker context excludes local shell credentials, direnv state, Git metadata
and generated build outputs. A real network-disabled Docker COPY test checks
that private sentinel inputs are excluded while application source remains.

Normal CI now checks source dependencies with pinned govulncheck 1.9.0 and fails
on reachable known vulnerabilities. A passing source scan is not a guarantee
against unknown vulnerabilities or a substitute for deployment qualification.

## Compatibility and release checks

Existing published versions and artifacts remain immutable. Use a new
version after the normal integration and artifact checks pass. Source builds
following the supported release tooling require Go 1.26.10. Clients using the
corrected vendored API documents must verify their exact published server source
pins before release. Qualify the bundled engine separately from the Runner's Go
module graph. Run the artifact integration stage with the shipped OpenTofu,
including legacy artifact carry-forward and exact history rollback, before
publishing. The stage creates real state with the previously shipped OpenTofu
1.10.0, then carries it forward, updates it and rolls back the artifact using
1.13.2, retaining state lineage and nondecreasing serials. These executions have
no network access; the legacy engine is test-only and is not in the final image.
Other provider and state configurations still require deployment
qualification; do not assume state written by the newer engine can be restored
by downgrading the executable. Base images require their own security scan.

The private Drift Detection candidate inherits this maintenance work. Its actual
Runner executable and source scan have no reported reachable vulnerabilities.
The bundled official OpenTofu 1.13.2 executable still reports 11 symbol-level
advisories in the 2 November scan. That is a separate unresolved vendor-artifact
hold, not an accepted exception or proof that every reported symbol is reachable
through the CLI. Functional upgrade tests do not clear that hold.
