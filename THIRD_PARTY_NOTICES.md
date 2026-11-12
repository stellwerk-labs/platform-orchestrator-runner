Pogi pricing source is separately licensed under MIT, copyright (c) 2026 nrndev.
The Runner's root EUPL licence does not relicense this source.

Runner includes only the selected Pogi v0.2.0 pricing core, Terraform model and embedded price catalogues, pinned to commit `002937be7308b8fe3f9f5d65698c49d47b65aaf6`.
The complete upstream MIT licence, including its excluded-icon notice, is preserved in `internal/thirdparty/pogi/LICENSE`.
No icons, emulator service models, discovery documents, or Azure REST specifications are included.
`internal/thirdparty/pogi/SOURCE.json` records the exact original and adapted SHA256 of every selected file, the import-path-only modification and catalogue provenance.
`internal/thirdparty/pogi/pogi.spdx.json` identifies the MIT source component separately from Go modules.

Production images ship these 3 notice and provenance files under `/licenses/pogi/`.
Release notice validation checks the exact source closure, the preserved MIT licence and SPDX component, and rejects changed or missing copies.
Catalogue updates require a reviewed source pin and hash update; Runner performs no network price refresh.

Run `python3 scripts/vendor-pogi.py PINNED_UPSTREAM_SOURCE` to reproduce the selected files offline. It checks the manifest bound to the exact upstream commit and every original byte before applying the sole import rewrite.
