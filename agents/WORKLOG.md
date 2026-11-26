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

## Native pipeline source export

Prepared the Native pipeline application source export. Public artifact publication and hosted qualification are separate release steps.

## 2026-11-24: Bound Native reverse transport

Bound Native reverse imports to the configured organization and runner identity before attachment, ledger or publish effects. Native control results enforce the same 8 KiB envelope limit as their consumers. Helm routes and exporter consumers use the bound identity; the documented cutover preserves queued data.
Focused real-NATS regression checks, the ordinary unit suite, vet, lint and Helm template checks passed. Independent source reviews passed. Updated image qualification and public publication remain separate steps.

## 2026-11-24: Runner chart release candidate

Prepared chart `0.5.0-rc.1` metadata, packaged README, tag notes and the conditional GitHub prerelease argument. The local release contracts, chart metadata, Helm render and package checks passed with Bash `5.3.20` and Helm `4.3.2`. The hosted producer pins Helm `4.3.0`; its publication checks remain separate. The unchanged Go native wrapper was not rerun.

## 2026-11-25: Pipeline RC documentation

Prepared customer-facing RC availability and deployment-default guidance without
runtime, schema, dependency or version changes. Focused documentation checks cover
text, links, file hashes and applicable generator templates; the full functional
suite was not rerun because the qualified application code is unchanged.
See DECISIONS.md and LEARNINGS.md for scope and documentation maintenance guidance.

### Correct regular-file test fixtures

Updated the existing protocol fixture to retain an installed regular shell or privately copy a final-component shell alias, and the catalogue secrecy fixture to use the canonical echo path. Existing protocol, digest, confinement, cancellation and private-environment assertions are retained. No production, dependency, workflow or runtime behavior changes. See DECISIONS.md and LEARNINGS.md.

### Prepare Runner and chart RC2 metadata

Prepared forward application and chart candidate notes, README references and chart version/appVersion for the reviewed portable test fixtures. Existing candidate notes, runtime, dependencies, templates and activation defaults remain unchanged.

### Correct the HA bootstrap endpoint

Prepared the existing HA PostgreSQL probe with an explicit loopback TCP host and changed only the existing cold OpenTofu setup and backend E2E outer allowances from 5 to 10 minutes. Aligned the complete PostgreSQL selector to a 15-minute aggregate in the script and standalone Make target, and both complete HA workflow steps to 20 minutes. Application code, dependencies, runtime security and individual deadline assertions are unchanged. A real initialization-barrier regression and the full existing HA target are required before source signing.

### Prepare Runner and chart RC3 metadata

Prepared forward application and chart notes, README references and chart version/appVersion for the reviewed readiness and qualification-budget corrections. Existing notes, application code, dependencies, workload templates and activation defaults remain unchanged.

### Prepare the missing Terraform unit-job prerequisite

Prepared only the existing checksum-pinned Terraform installation in the two consuming unit-job tool steps and these generic notes. Production code, dependencies, tests, limits, filters, runtime packaging and previous candidates remain unchanged. Actual Linux full-selector qualification remains required before forward candidate tagging and publication.

### Prepare matched forward candidate metadata

Prepared Runner and chart RC4 notes, README references and chart version/appVersion as a separate metadata change. Existing candidate notes, application code, dependencies, templates, security and activation defaults remain unchanged. Application and publication are still pending.

## 2026-11-26: Retry the deployment result without an outbox

Deployment Jobs on the no-outbox path (chart `simple` and `airgap` modes, DP-direct Jobs) posted their result once, so a gateway restart lost it and the deployment never finished. `Client.PostResultsRetrying` repeats transport errors and 429/502/503/504 with backoff from 1s to 15s for at most 2 minutes; other statuses stay final. The gateway publishes results under a stable message ID with a 10 minute duplicate window and DP applies each result event once, so repeats are harmless. `PostResults` stays single-shot for the outbox, which must spool at once. The integration harness also restarts and waits for the gateway after the kind node joins the compose network.

## 2026-11-26: Open chart pod metadata and placement maps

The values schema closed `podAnnotations`, `podLabels`, `nodeSelector` and `affinity` with `additionalProperties: false` and no properties, so any user entry failed `helm install` (since chart 0.4.0). The three Kubernetes string maps now accept string values and `affinity` accepts any object. `scripts/chart-pod-placement_test.sh` renders both the agent Deployment and the native worker StatefulSet with all four values and rejects a non-string annotation; both chart validation jobs run it.

### Prepare Runner and chart RC5 metadata

Prepared Runner and chart RC5 notes, README references and chart version/appVersion as a separate metadata change on top of the gateway result retry, its harness wait, the clean-order fix and the chart schema fix. Earlier candidate notes, dependencies and activation defaults remain unchanged.
