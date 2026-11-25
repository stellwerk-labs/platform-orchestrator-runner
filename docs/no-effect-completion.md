# Pipeline completion without a mutating claim

> **RC feature:** This page describes Deployment Pipelines in the coordinated
> release candidate (Runner `3.4.0-rc.1`). Pipelines are not available in the
> current stable release from `main`. Use matching Pipeline RC components; Native
> execution requires explicit operator activation.

A terminal unsuccessful native pipeline can return an optional `no_effect_receipt` through the existing authenticated result callback. It contains protocol version `stellwerk.no-effect/v1`, SHA256 of the original signed command payload and `effect: not_started`. It contains no deployment credential, executable input or plan. DP must match the authenticated target and retained signed command, and use its own immutable captured predecessor before changing effective-state projection.

Completion atomically rereads the command and current terminal progress. Status labels alone are not evidence that execution did not start. Stale revisions, missing authority and current unknown outcomes do not produce a completion. The receipt requires absence of every mutating effect claim, including completed task claims, and absence of a permanent local blocking fact. Failed observations retain their existing contract; successful Plans and successful Applies do not receive this failure-only receipt.

The Runner permanently records whether any mutating execution claim, applying
or unknown outcome, or infrastructure application has occurred. Recovery must
retain these records together with commands and execution history. Incomplete or
inconsistent history refuses recovery instead of enabling an infrastructure retry.

An unknown outcome followed by a failed status cannot become proof that no effect started. A completed no-op Apply also claimed execution and remains ineligible, even with zero resource changes. There is no API to clear that record. Preserve it together with command replay and single-use identity across expiry, history retention and backups; deleting detailed events or artifacts must not make a command eligible again.

The claim boundary assumes site-controlled approved tools obey their declared execution/retry contract. It is not isolation for malicious customer code or a proof that an arbitrary program made no external change. Uncertain effects still require separate evidence-based reconciliation and retain existing fences.

Deploy the compatible DP receipt consumer and predecessor capture with this Runner change. Old commands lacking captured DP authority remain conservatively ineligible for automatic projection; do not manufacture a receipt, delete claims/facts or clear an unknown fence to bypass that limitation. Backups and restores must retain these effect claims together with execution history; reconcile uncertain effects before resuming execution.
