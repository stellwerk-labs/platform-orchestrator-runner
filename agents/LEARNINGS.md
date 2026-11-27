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

## 2026-11-27: Moving the Infracost pin

Infracost pins live in three layers: source/version strings, the receipt hashes (`PATCHES.json`, `infracost-source.json`) and a third copy of those receipt and corresponding-source hashes in `scripts/validate-notices.sh`; grep for the derived hashes, not only the version.
The corresponding-source archive hash depends only on the patched dependency tree, so compute it before fixing the receipt hash that embeds it.
`collect-licenses.sh` sorts with the caller's locale; run the notice tests under `C.UTF-8` like CI, an `en_US.UTF-8` shell fails the module-closure comparison.
The OpenTofu vendor gate pins an exact finding inventory; new database advisories against an unchanged official binary reject it without any source change.
govulncheck `v1.9.0` and `v1.10.0` binary mode label each package with the first module from a Go map whose path prefixes it (`internal/vulncheck/packages.go` `findModule`); any advisory on packages under nested modules, here GO-2026-6974 in otel, gets random labels and an unstable full-finding digest while advisories, counts and package/symbol tuples stay fixed.
