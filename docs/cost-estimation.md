# Monthly cost estimates

Cost estimation is optional and disabled by default.
The installation Admin enables it and configures a customer-owned Infracost API key.
The dispatch environment authoritatively supplies `COST_ESTIMATION_ENABLED` and `INFRACOST_API_KEY`; a missing key leaves estimation unavailable and never blocks infrastructure execution.

The Runner invokes its image-owned classic Infracost `v0.10.49` executable (packaged build `v0.10.49+stellwerk.1`, with its bounded dependency compatibility/security patch documented in the third-party notices) after successful manual and Drift plan inspection.
The plan is parsed locally; only product/price filter queries go to `https://pricing.api.infracost.io`.
Pricing attributes such as cloud region and instance class are disclosed.
Cloud uploads, organization settings, events, update checks, envfile loading and persisted price caches are disabled.
No cloud credentials or deployment environment are passed to the estimator, and its API key is stripped from Terraform/OpenTofu and provider process environments.
Temporary input is private and removed after execution; raw plan JSON, resource identities, API keys and child diagnostics are never added to the cost log.

The existing encrypted deployment log receives an aggregate `cost_estimation` record, including `estimated_monthly_subtotal` in USD when available, original managed-resource count, priced/free/unsupported/unknown resources and missing usage counts.
Sensitive resources are excluded from estimation, with coverage reported as unknown.
The subtotal estimates planned resources after reconciliation, not measured current billing or a cost delta.
Infracost may apply defaults; `upstream_defaults_possible=true` makes this explicit, and partial coverage is never represented as a complete known bill.
Usage, discounts and unsupported resources can remain unestimated.
The vendor catalogue refresh timestamp is not established by a new request, so `catalogue_updated_at` is `unknown`.

Failures, missing executable/key, invalid data, bounded output and the 30-second timeout affect only this advisory record.
Plan, apply and Drift observation results remain authoritative.
The executable and calculators are pinned; pricing responses can change independently.

Keys remain accessible to administrators of the dispatch infrastructure: Kubernetes Jobs, ECS task definitions, queued remote commands and exported diode frames can contain the dispatched environment.
The dispatch transport follows the existing deployment-token boundary; configuring this feature does not make those stores memory-only or end-to-end encrypted.
Key rotation/removal affects newly assembled dispatches and cannot revoke copies already queued.
