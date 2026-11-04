# Learnings

## 2026-11-04: Component SDK and release identities

Shared SDK modules have independent versions. A combined CP/DP version statement
can become stale when only one adds an API. Verify each published module against
the actual application dependency closure.

A squash or merge changes source identity even when application bytes match the
candidate. Preserve the original candidate proof and explicitly bind the new
stable producer and consumer to the actual source, tree and tag.
