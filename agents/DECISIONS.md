# Decisions

## 2026-11-04: Stable SDK compatibility notes

List the published CP and DP shared SDK versions separately: CP shared/v3
3.2.0 and DP shared/v3 3.1.0. Keep Runner application code, dependencies,
workflows, recovery limits and the release-scoped OpenTofu residual disclosure
unchanged. Preserve the immutable RC4 source and artifacts.

A normal stable PR creates a new main identity. Bind the stable tag, producer
and original consumer to that actual reviewed identity after landing; do not
substitute the RC source SHA or infer future artifact identities.

## 2026-11-13: Customer-owned Infracost pricing

Replace the embedded Pogi implementation completely with an optional standalone classic Infracost build.
Installation Admin supplies the customer pricing API key; enablement defaults to false, and no missing/failed estimate may block manual or Drift execution.
The child environment is constructed explicitly and the pricing key is stripped from Terraform/provider environments.
The local parser keeps plan data private and rejects sensitive provider-region variable inputs before any pricing request.

Pin upstream `v0.10.49`, source `40ad0ac9f247d883884cd0f60057367b4af3df08`, and identify our executable as `v0.10.49+stellwerk.1`.
The stock release links deprecated OpenPGP code; the selected private-cache build upgrades 8 existing Go x modules and makes 6 import substitutions plus 4 nil-config compatibility additions in 4 existing dependency files.
Authenticate module archives before those exact hash-guarded edits; preserve original module identities for actual binary scanning.
Current v2 changes hosted authentication and performs dashboard/telemetry calls, so it does not satisfy the pricing-key-only and pricing-only network contract.
Patch maintenance is an explicit build obligation, not an unmodified-vendor claim.

## Native pipeline source export

Prepare Native pipeline application changes as a source export with all product code, tests, documentation and release controls retained.

## 2026-11-24: Runner chart release candidate

Chart `0.5.0-rc.1` selects Runner `v3.4.0-rc.1` through `appVersion`. Tag-triggered GitHub releases derive `--prerelease` from the final chart version and keep `--latest=false`.

## 2026-11-25: Pipeline RC documentation

Mark Deployment Pipelines as an RC feature, separate from the current stable release.
Preserve direct Apply as the built-in default; publishing a recipe alone changes no default.
This documentation correction changes no runtime, schema, dependency or RC version
and does not invalidate the existing functional qualification.

### Use actual regular executables in protocol fixtures

Keep approved-tool validation strict about regular files and frozen digests. Retain an already regular installed shell; copy only a final-component shell symlink into the existing owned test directory under basename sh. This preserves real shell behavior, including multicall dispatch, without unnecessarily relocating system executables. Resolve the catalogue-only echo fixture before hashing it. Production execution and confinement policies remain unchanged.

### Align forward Runner and chart candidate metadata

A correction after a reserved application candidate receives a forward version. Keep the chart candidate and its default image version aligned with the corrected Runner. Preserve previous notes and tags.

### Check final PostgreSQL TCP readiness in the HA fixture

The HA bootstrap must wait for the final TCP server rather than the temporary Unix-only initialization server. Keep the existing bounded probe and cleanup, adding only its loopback host. Allow the existing cold OpenTofu setup and backend E2E outer steps 10 minutes each without changing the Native job cap, actual Go-test deadline or checkout. The complete PostgreSQL selector has a 15-minute aggregate in both existing callers, with 20 minutes for the complete HA workflow step. Production behavior and each internal deadline assertion remain unchanged.

### Preserve candidate source while forwarding qualification fixes

A selected release-fixture correction after a reserved candidate receives a forward application candidate. Keep the chart candidate and its default image version aligned, preserving previous source, notes and tags.

### Supply every real IaC tool to its consuming test job

The ordinary and release unit jobs run the same dual-tool PostgreSQL tests. Install the already checksum-pinned Terraform version beside the shipped OpenTofu and Infracost binaries in their existing private tool directory. Preserve real execution, assertions, source/runtime inputs and all existing release controls.

### Prepare matched forward candidate metadata

Reserve the forward application candidate only after selected source qualification succeeds. Keep the chart version and default Runner image aligned, preserving previous candidate notes and tags.

## 2026-11-26: Bounded deployment result retry without an outbox

Jobs without a gateway outbox retry their result for at most 2 minutes, from 1 to 15 seconds apart, on transport errors and 429/502/503/504. The gateway publishes the result under a stable message ID inside a 10 minute duplicate window and Data Plane applies a result event once, so a repeat after a lost response is harmless. A 403 stays final: a real denial must not be repeated, and the gateway should instead answer 503 when it cannot reach the Data Plane authority. The outbox keeps a single attempt because it must spool at once and its flusher already retries every 5 seconds.

## 2026-11-27: Drop DWARF and saved testdata from the Runner image

Build the Runner, OpenTofu and Infracost executables with `-w` (the Runner also with `-trimpath`) and never `-s`: binary govulncheck needs the symbol table for symbol-level results. The licence collectors delete `testdata/` directories from saved dependency source after `go-licenses save`; `go list -deps -f '{{.EmbedFiles}}'` showed no linked or embedded file under `testdata/` in the Runner, OpenTofu or Infracost graphs, so MPL-2.0 corresponding source stays complete. Upstream OpenTofu source in its archive keeps its `testdata/`. Jay accepted, on the RC line, that the new OpenTofu and Infracost bytes change the Native catalogue digest: Native sites upgrading from rc.5 run `rollout-catalogue-update`, which the rc.6 notes state. Infracost stays in the single Runner image; UPX, base image changes and removing git/openssh are out of scope.
