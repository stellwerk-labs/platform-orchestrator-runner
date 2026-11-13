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
