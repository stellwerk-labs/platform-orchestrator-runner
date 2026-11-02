# Environment observation candidate

Unreleased. Publication requires the coordinated component review and resolution
of the separate bundled-engine security hold.

The Runner adds an optional `environment-observation/v1` execution protocol. A
marked `plan_only` job performs a full refreshed Terraform/OpenTofu plan and
returns a bounded summary of observed changes and proposed actions. It does not
apply that plan or write refreshed state. The summary excludes resource
addresses, attribute and output values, and provider diagnostics.

Plain plan-only, deployment, rollback and destroy jobs retain their existing
behavior. Unsupported protocol markers and invalid mode combinations fail before
execution. An older Runner safely executes the marked plan-only job without the
structured result; a compatible Data Plane treats that result as unavailable,
not clean.

Baseline selection, organization authorization, queue admission and persistent
Runner capacity remain Core responsibilities. Operators must configure a matching
Core and Data Plane before declaring this protocol on a Runner. No commercial
plugin business logic or licence issuer is included in the image.

The candidate also inherits the prepared dependency, compiler, image-input and
OpenTofu maintenance described in [the security notes](security-maintenance.md).
Changing the engine from 1.10.0 to 1.13.2 requires its own state compatibility
qualification; it is not an application-library patch. Existing image tags stay
immutable.
