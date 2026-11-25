# Learnings

## 2026-11-04: Component SDK and release identities

Shared SDK modules have independent versions. A combined CP/DP version statement
can become stale when only one adds an API. Verify each published module against
the actual application dependency closure.

A squash or merge changes source identity even when application bytes match the
candidate. Preserve the original candidate proof and explicitly bind the new
stable producer and consumer to the actual source, tree and tag.

## 2026-11-13: Infracost artifact qualification

Stripped Go executables can make govulncheck fall back to module-wide findings; retain symbols and distinguish actual symbol findings from unused-module advisories.
With trimpath, Go build metadata can omit ldflags, so validate exact debug/buildinfo settings and dependency identities, then test actual CLI version output separately.
Go overlays cannot replace files beneath GOMODCACHE; a dedicated authenticated private cache permits explicit dependency patches without changing shared sources or losing module identity.
Sensitive Terraform provider-region variables are outside planned resource masks; check configuration declarations and references before invoking the pricing client.
Some dependencies carry their complete copyright/licence notice inline rather than in a standalone LICENSE file; preserve the exact pinned header and full terms.

## Native pipeline source export

Keep the released application source and its public SDK contracts aligned; preserve existing compatibility exports.

## 2026-11-24: Runner chart release candidate

Run the actual workflow Bash when checking release arguments. A local `gh` argument recorder captures stable, RC and beta command lines without contacting GitHub.

## 2026-11-25: Pipeline RC documentation

Put availability guidance at feature entry points and keep operational limits explicit.
Customer guidance must avoid internal experiment history and private environment details.
For generated documentation, preserve notices in the source templates while retaining
dynamic schemas, examples and imports.

### Distribution aliases are not approved executable fixtures

Standard shell and echo paths can be final-component symlinks. A fixture which hashes their targets but supplies the aliases contradicts the regular-file execution contract. Retain already regular installed executables. When a final-component alias requires a copy, real shell bytes under their shell basename preserve behavior without a mock or an operating-system branch.

### Treat the chart default image as release metadata

A forward Runner candidate also needs an explicitly aligned chart appVersion, candidate version and installation examples. This changes version selection rather than Native activation or workload behavior.

### Leave headroom for cold tool compilation

A cold tool compilation can exhaust a setup budget before any backend test starts. Preserve the original timeout outcome and distinguish the workflow commit from the exact candidate checked out by the supplemental job. Carry the ordinary setup-budget correction into the next normal source update, never a frozen verification checkout.
