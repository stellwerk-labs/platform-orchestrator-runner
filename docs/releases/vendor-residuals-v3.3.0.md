# Official OpenTofu residuals accepted for Runner 3.3.0

On 13 November 2026 the release owner explicitly accepted the current residual
risk for this Runner version and requested its deployment to WX1. This decision
retains the unchanged official OpenTofu 1.13.2 executables. It does not fix the
vulnerabilities or establish that they are unreachable. A public release remains
a separate publication action.

The disposition applies only to `v3.3.0` and canonical `v3.3.0-rc.N` releases.
The validator rejects every other version, including `v3.2.0` and `v3.4.0`.
The earlier [Runner 3.2.0 disposition](vendor-residuals-v3.2.0.md) remains a
historical record and does not cover this inventory.

| Official Linux artifact | Executable SHA256 |
| --- | --- |
| AMD64 | `949061cbb3c3d4558928ca9a459a5052b8c5e5545d17656f33316d2e3bc335f2` |
| ARM64 | `92fb091345bc9ba9339496663aac81bd9a3686969682d8142d4c08ce18d9b9a8` |

Both executables use Go `1.27.3`. The existing official signature and checksum
verification remain mandatory; no vendor source rebuild is substituted.

Pinned govulncheck `v1.9.0`, using `https://vuln.go.dev` with the captured database
timestamp `2026-11-11T02:51:11Z`, reports these 13 advisory IDs on each executable:

- GO-2026-5932
- GO-2026-6179
- GO-2026-6180
- GO-2026-6443
- GO-2026-6603
- GO-2026-6610
- GO-2026-6611
- GO-2026-6612
- GO-2026-6617
- GO-2026-6859
- GO-2026-6860
- GO-2026-6865
- GO-2026-6866

Each report contains 1563 finding records across module, package and symbol
levels. The executables are stripped, so the pinned scanner falls back to
module-level evidence and synthesizes advisory-derived symbol names. These
counts are not observed vulnerable runtime paths. Separate read-only function
table inspection nevertheless found 117 advisory-listed HTTP names for
`GO-2026-6859` and 5 compression names for `GO-2026-6865` in each exact binary.
Runtime exploitability remains unresolved.

`scripts/vendorcheck/accepted-opentofu.json` binds each exact binary hash to the
compiler, scanner, complete advisory set, record count and canonical full finding
digest `5f8e13d9d42dd50f98ceb36cad83a047487e8847419fa1e22d3a0172eb2525a6`.
Both independently captured architecture reports produce this digest. The
fixtures retain their original JSON bytes under deterministic gzip compression;
[fixture provenance](../../scripts/vendorcheck/testdata/README.md) records their
identities. No findings are filtered or rewritten. Changed, added, removed or
duplicated findings fail the existing exact-inventory check.

All other gates remain mandatory: raw scanner reports and text exit verification,
source and Runner binary scans, official artifact authentication, complete legal
notices, final image checks for both architectures, integration and release
controls. This disposition grants no acceptance for other vendor versions,
executables, source vulnerabilities, private plugins or future Runner releases.
