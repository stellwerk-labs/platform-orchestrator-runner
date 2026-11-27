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

## 2026-11-27: Stable Infracost v0.10.51 security patch

The v3.3.0 Infracost build links GO-2026-6974 (otel `v1.45.0`) and GO-2026-6278 (gorilla/websocket `v1.5.3`).
The release owner chose the newest upstream release `v0.10.51` (source `aa7176706bc8fed866c8280213ca44cea70954f9`, packaged as `v0.10.51+stellwerk.1`) over back-porting module bumps onto `v0.10.49`, and kept the private patch mechanism so the websocket fix still applies.
Upstream `v0.10.51` already carries otel `v1.47.0`; the effective module set adds only `golang.org/x/crypto` `v0.59.0` (6 companion x modules) and websocket `v1.5.4-0.20240701034025-d67f41855da4`.
The 4 patched dependency versions are unchanged upstream, so every before/after hash in `PATCHES.json` stays byte-identical.
Upstream replaced `go.mozilla.org/gopgagent` with `github.com/getsops/gopgagent` (ships a LICENSE) and dropped `github.com/jmespath/go-jmespath`, so both inline-licence declarations and their go-licenses ignores were removed; the fail-closed inline-licence check remains.
Remaining unfixable advisories are accepted only once they are enumerated exactly; the Infracost executable currently has none.

## 2026-11-27: Runner 3.3.1 OpenTofu 1.13.10 disposition

Official OpenTofu 1.13.2 gained GO-2026-6974 and GO-2026-6975 after the 3.3.0 review, so its exact inventory no longer matched.
The release owner chose the newest official patch release, 1.13.10, and accepted its remaining 14 advisories for `v3.3.1` and its RCs until upstream fixes them.
Against 1.13.2 the scanner SBOM changes only the main module and stdlib (Go `1.27.4`, which resolves GO-2026-6859); all 244 dependencies are unchanged.
The disposition stays exact-version scoped: `v3.3.0` and `v3.3.2` are refused, and the official signed artifacts stay mandatory instead of a vendor rebuild.
govulncheck's `findModule` labels a package with the first module from a Go map whose path prefixes it, so GO-2026-6974 records under nested otel modules got random labels and the full-finding digest was not reproducible.
The release owner approved hashing package and symbol records without module, version and fixed version while module-level records keep all three; this keeps every advisory bound to its module and fix and makes the gate deterministic across runs and architectures. The gate change gets a fresh review.
