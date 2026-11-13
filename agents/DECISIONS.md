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
