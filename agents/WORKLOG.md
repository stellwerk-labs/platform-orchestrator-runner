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
