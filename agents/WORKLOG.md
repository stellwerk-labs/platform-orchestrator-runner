# Work log

## 2026-11-04: Stable Runner SDK notes

Corrected the CP shared SDK version in the stable 3.2.0 notes and distinguished
it from the unchanged DP and IAM SDK versions. Added these three agent notes.
No application, dependency, workflow, recovery-policy or vendor-policy change.

Validation for this notes-only successor uses scoped Markdown formatting,
release-contract tests, diff checks and a strict new-commit scan. Exact source,
parity and signature receipts are retained in the release coordinator's private
evidence. This does not qualify a new application artifact or publish a release.

## 2026-11-13: Infracost replacement, local qualification

Removed Pogi source, projection/vendor machinery, offline catalogue policy and notices.
Added disabled-by-default customer-key plumbing, isolated best-effort estimator process and shared manual/Drift logging; credentials cannot reach Terraform/provider children.
Real OpenTofu boundaries and real Infracost GraphQL/privacy/failure/cancellation tests passed, including a sensitive provider-region no-request regression.
The fresh application clean-code review passed with zero prescribed complexity findings.
Both actual Linux target executables pass symbol-aware vulnerability scanning; the full production notice/target/tamper pipeline passes 7 tests, including corresponding-source delivery.
Generation, 160 ordinary unit tests (1 existing optional NATS skip), vet, full pinned lint and the prescribed complexity gates pass.
The unrelated baseline lint cleanup is separately committed as 83edb33; this feature does not change the OpenTofu release-scoped security hold.
See DECISIONS.md and LEARNINGS.md for the selected architecture and artifact lessons.


## 2026-11-13: Public Runner source preparation

Removed the private installation identifier from the 3.3.0 residual-risk disposition before public source qualification. The accepted version family, exact binaries, advisory inventory and unresolved applicability remain unchanged. No workflow or application behavior changed.

## 2026-11-27: Stable Infracost v0.10.51 patch, local preparation

Moved the pinned source, archive hash, build label and every receipt/validator hash to Infracost `v0.10.51`; regenerated the effective module inputs with `go get` only (x/crypto `v0.59.0`, websocket fix) and kept the 4 OpenPGP file patches unchanged.
Both linux executables build and scan with zero findings at any level; the notice pipeline passes 7 tests and the real Infracost boundary tests pass against the new binary.
See DECISIONS.md and LEARNINGS.md (2026-11-27).

## 2026-11-27: Runner 3.3.1 OpenTofu 1.13.10, local preparation

Moved the official engine pin, notice receipt, notice fixtures and state-version expectation to OpenTofu 1.13.10, verified against its signed `SHA256SUMS`.
Scoped the vendor disposition, CI release argument and release tests to `v3.3.1`, captured the 1.13.10 scanner fixtures and wrote the 3.3.1 release and residual notes.
The vendor digest now ignores the scanner's random module labels on package/symbol records; repeated live scans and repeated security/final builds are accepted deterministically.
See DECISIONS.md and LEARNINGS.md (2026-11-27).
