Runner includes a separate Apache-2.0 Infracost executable, version `v0.10.49`, source commit `40ad0ac9f247d883884cd0f60057367b4af3df08`.
Its licence is independent of Runner's EUPL licence.

The image builds the pinned source using Go `1.26.11` and does not bundle upstream Terraform versions, installer helpers or v2 plugins.
`scripts/release-notices/infracost-source.json` records the source archive, licence and Go module input hashes.
`/licenses/infracost/` includes the original licence, provenance, actual target production module graph, dependency legal notices, detected licence report, executable build receipt and SPDX dependency inventory.
The production collector validates these against the exact source and executable for each supported architecture.

The Infracost Cloud Pricing API is a separate hosted service.
Enablement and customer API credentials are installation settings, not a licence to redistribute that service or its pricing data.

This executable is identified as `v0.10.49+stellwerk.1`, not an unmodified upstream binary.
An isolated private module-cache build upgrades `golang.org/x/crypto` to `v0.59.0` and its 7 required companion modules, and changes 6 obsolete OpenPGP imports in 4 existing dependency files to the already included maintained ProtonMail implementation.
`scripts/infracost-patches/PATCHES.json` records every original and patched file hash plus the effective module input hashes.
The original upstream archive and shared Go module cache are never modified.
The Go module archives are verified before the 4 explicit source patches; exact after-hashes validate those files afterward.
The final binary retains its symbols for accurate vulnerability checking, and the licence bundle preserves the exact changed source files and effective dependency graph.
