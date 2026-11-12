# Offline monthly base disk estimates

Runner logs one aggregate estimate after successful manual and Drift plan inspection.
`COST_ESTIMATION_ENABLED` defaults to `true`; set it to `false` in the Runner pod environment to opt out.
Existing per-Runner pod-template environment overrides can set this flag without an API change.
Failure or unsupported pricing adds a sanitized warning or coverage result and does not change plan, deployment or observation success.

Estimates use `INFO`, or `WARN` when estimation fails, and respect `LOG_LEVEL`.
Default Drift jobs use `INFO` and emit these entries to the job container's stdout.
Drift observations have no encrypted logs recipient, so these estimates are available through operator Pod logs or an external log collector, not ordinary CLI/Console deployment-log retrieval.
Pod logs follow the job's lifetime and are not durable estimate storage; external retention depends on the operator's collector.
Manual estimates enter stored deployment logs when an encrypted logs recipient is configured.

The initial reviewed pricing subset covers AWS `aws_ebs_volume` GP3 disks with known size, type, IOPS, throughput and region, plus Azure `azurerm_managed_disk` `Premium_LRS`/`Premium_ZRS` disks with known capacity and location.
AWS regions resolve from a literal provider region, a known root variable, a known resource region or a standard availability zone.
Conflicting regions, unresolved expressions and sensitive pricing inputs stay unknown.
Azure custom performance tiers, on-demand bursting and shared-disk charges are excluded; nondefault or unresolved settings stay unknown.
EC2 and other compute shapes are deferred because operating-system, root-disk and usage assumptions need separate review.

The log message says **Monthly base disk estimate** and always carries resource coverage counts.
`known_monthly_subtotal` sums only fully eligible disk rows. Other resources make the result `partial`.
If all resources are unpriced, the result is `unknown` and has no amount.
Only AWS VPCs and Azure resource groups are explicitly accepted as having no stand-alone charge; a plan containing only these can report a known zero.
Unknown providers and every GCP resource are unpriced, because this source release has an empty GCP catalogue.
Data resources and deleted resources are excluded. Unchanged and nested managed resources in `planned_values` remain in coverage.
This is a planned-resource base subtotal, not a cost delta or a complete infrastructure bill.

Pricing runs locally using the selected MIT Pogi `0.2.0` pricing core, pinned to commit `002937be7308b8fe3f9f5d65698c49d47b65aaf6`.
AWS and Azure catalogue snapshots declare `2026-10-20`, USD, on-demand/list prices, with `730` hours per month for hourly rates.
Logs include the snapshot date, declared source and age; execution time is not a price refresh.
Taxes, discounts, credits, usage charges and future provider price changes are outside the estimate.
Amounts are approximate and displayed to `2` decimal places.

No accounts, pricing keys, telemetry, subprocesses, runtime downloads or network price requests are required.
The estimator accepts at most `8 MiB`, `2000` resources and bounded JSON/module nesting.
Raw plans, resource addresses and attribute values do not enter estimate logs.
The existing private observation-plan deletion, backend locking and no-state-write behavior remain unchanged.
No automatic rightsizing or budget enforcement is included.

[Third-party notices](../THIRD_PARTY_NOTICES.md) describe the retained MIT licence, source hashes and shipped SPDX component inventory.
Catalogue changes require an explicit reviewed source update.
