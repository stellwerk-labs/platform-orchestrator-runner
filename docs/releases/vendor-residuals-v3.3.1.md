# Official OpenTofu residuals accepted for Runner 3.3.1

On 27 November 2026 the release owner moved the bundled engine to the newest
official OpenTofu patch release, 1.13.10, and accepted its remaining residual
advisories for this Runner version until upstream fixes them. This decision does
not fix the vulnerabilities or establish that they are unreachable. A public
release remains a separate publication action.

The disposition applies only to `v3.3.1` and canonical `v3.3.1-rc.N` releases.
The validator rejects every other version, including `v3.3.0` and `v3.3.2`.
The earlier [Runner 3.3.0 disposition](vendor-residuals-v3.3.0.md) remains a
historical record and does not cover this inventory.

| Official Linux artifact | Executable SHA256 |
| --- | --- |
| AMD64 | `e9a8c4ba735bd4c25f4d53231bd68318e8a14039947c391df9e1eddb40973c8e` |
| ARM64 | `af360fed5b8f919f04c322c04583d7581aa6325ca2bb111b728093d99f0ae635` |

Both executables use Go `1.27.4`. Compared with 1.13.2, the scanner SBOM changes
only the main module and standard library; all 244 dependency versions are
unchanged. The existing official signature and checksum verification remain
mandatory; no vendor source rebuild is substituted.

Pinned govulncheck `v1.9.0`, using `https://vuln.go.dev` with the captured database
timestamp `2026-11-24T15:56:00Z`, reports these 14 advisory IDs on each executable.
Each entry names the affected dependency in the official binary and the first
fixed upstream version:

- GO-2026-5932: `golang.org/x/crypto` `v0.56.0`, fixed in `v0.58.1-0.20261009181718-2f1f834330b2`
- GO-2026-6179: `golang.org/x/mod` `v0.39.0`, fixed in `v0.40.0`
- GO-2026-6180: `golang.org/x/mod` `v0.39.0`, fixed in `v0.40.0`
- GO-2026-6443: `google.golang.org/grpc` `v1.83.1`, fixed in `v1.83.2`
- GO-2026-6603: `golang.org/x/net` `v0.57.0`, fixed in `v0.60.0`
- GO-2026-6610: `golang.org/x/net` `v0.57.0`, fixed in `v0.60.0`
- GO-2026-6611: `golang.org/x/net` `v0.57.0`, fixed in `v0.60.0`
- GO-2026-6612: `golang.org/x/net` `v0.57.0`, fixed in `v0.60.0`
- GO-2026-6617: `golang.org/x/net` `v0.57.0`, fixed in `v0.60.0`
- GO-2026-6860: `golang.org/x/net` `v0.57.0`, fixed in `v0.62.0`
- GO-2026-6865: `github.com/klauspost/compress` `v1.19.2`, fixed in `v1.20.2`
- GO-2026-6866: `golang.org/x/net` `v0.57.0`, fixed in `v0.62.0`
- GO-2026-6974: `go.opentelemetry.io/otel/sdk` and `otel/trace` `v1.45.0`, `otel/sdk/log` `v0.21.0`, fixed in `v1.47.0`
- GO-2026-6975: `go.opentelemetry.io/otel` `v1.45.0`, fixed in `v1.47.0`

Compared with the 3.3.0 inventory, GO-2026-6859 is resolved by the newer Go
toolchain; GO-2026-6974 and GO-2026-6975 were published after that review and
also affect the unchanged 1.13.2 executables.

Each report contains 1432 finding records across module, package and symbol
levels. The executables are stripped, so the pinned scanner falls back to
module-level evidence and synthesizes advisory-derived symbol names. These
counts are not observed vulnerable runtime paths. Runtime exploitability remains
unresolved.

`scripts/vendorcheck/accepted-opentofu.json` binds each exact binary hash to the
compiler, scanner, complete advisory set, record count and canonical finding
digest `6b31464e89d4c86c0a36af028c3d4b2ce051652d053127f0bbd7ee3ec84bc765`, which
both architecture reports produce. The pinned scanner labels each package with
the first binary module whose path prefixes it, iterating a Go map, so packages
under nested modules (here the GO-2026-6974 OpenTelemetry packages) get random
module labels between identical scans. The digest therefore identifies package and symbol records by package,
receiver and function only; module-level records keep module, found version and
fixed version, so every advisory stays bound to its module and fix. This
canonicalization was introduced with this disposition and approved by the
release owner. The fixtures retain their original JSON bytes under
deterministic gzip compression;
[fixture provenance](../../scripts/vendorcheck/testdata/README.md) records their
identities. The raw reports are kept unmodified; only the digest ignores the
package and symbol record labels. Changed, added, removed or duplicated findings
fail the existing exact-inventory check.

All other gates remain mandatory: raw scanner reports and text exit verification,
source and Runner binary scans, official artifact authentication, complete legal
notices, final image checks for both architectures, integration and release
controls. This disposition grants no acceptance for other vendor versions,
executables, source vulnerabilities, private plugins or future Runner releases.
