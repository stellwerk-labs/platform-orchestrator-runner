# Native command authority

> **RC feature:** This page describes Deployment Pipelines in the coordinated
> release candidate (Runner `3.4.0-rc.1`). Pipelines are not available in the
> current stable release from `main`. Use matching Pipeline RC components; Native
> execution requires explicit operator activation.

A Native pipeline command carries the accepted 7-field execution target: domain Runner ID, registration UUID, runtime, pipeline version, keyed tools digest, state backend discriminator and Core state-configuration digest. Command/request identity and UUID must match it. The local Engine checks its actual runtime, UUID, owned catalogue and existing backend validation. The Core state digest remains an opaque accepted reference, not a locally reconstructed private backend identity.

The outer transport remains protocol1, type `execute-rollout-v1` and command ID `pipeline:<Deployment UUID>`. Its Runner route is the UUID token; inner Runner IDs stay domain slugs. The signed bundle is embedded, bounded and hashed. Native requests never mint or contain a legacy `deployment_token`, even empty/null, duplicate/case aliases or escaped field spellings. Fresh and retained decoding inspect request member names before projection. Legacy/v1 retains its original parsing and token behavior.

Native metadata uses the fixed compiled `platform_orchestrator_metadata` output through the existing empty-key default. The output recipient comes from the accepted Deployment, independently of the site storage identity and Agent authentication key. The actual DP producer emits OpenTofu. Terraform remains a separately qualified signed Engine/artifact path; there is no customer tool selector.

Format7 refuses older Native profiles without rewriting encrypted commands,
keys, effect claims or recovery state. Retain their history and the compatible
binary for controlled reconciliation. This profile does not provide an in-place
upgrade, reset or state-handoff mechanism.

Registration, Gateway access, and command delivery must all use the matching
coordinated RC. Removing the legacy direct token does not replace Gateway/DP
checks against a Deployment's accepted target. Unknown effects remain fenced;
a delivery timeout never proves that execution did not start.
