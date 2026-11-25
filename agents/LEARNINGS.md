# Learnings

## 2026-11-04: Component SDK and release identities

Shared SDK modules have independent versions. A combined CP/DP version statement
can become stale when only one adds an API. Verify each published module against
the actual application dependency closure.

A squash or merge changes source identity even when application bytes match the
candidate. Preserve the original candidate proof and explicitly bind the new
stable producer and consumer to the actual source, tree and tag.

## 2026-11-13: Infracost artifact qualification

Stripped Go executables can make govulncheck fall back to module-wide findings; retain symbols and distinguish actual symbol findings from unused-module advisories.
With trimpath, Go build metadata can omit ldflags, so validate exact debug/buildinfo settings and dependency identities, then test actual CLI version output separately.
Go overlays cannot replace files beneath GOMODCACHE; a dedicated authenticated private cache permits explicit dependency patches without changing shared sources or losing module identity.
Sensitive Terraform provider-region variables are outside planned resource masks; check configuration declarations and references before invoking the pricing client.
Some dependencies carry their complete copyright/licence notice inline rather than in a standalone LICENSE file; preserve the exact pinned header and full terms.

## Native pipeline source export

Keep the released application source and its public SDK contracts aligned; preserve existing compatibility exports.

## 2026-11-24: Runner chart release candidate

Run the actual workflow Bash when checking release arguments. A local `gh` argument recorder captures stable, RC and beta command lines without contacting GitHub.

## 2026-11-25: Pipeline RC documentation

Put availability guidance at feature entry points and keep operational limits explicit.
Customer guidance must avoid internal experiment history and private environment details.
For generated documentation, preserve notices in the source templates while retaining
dynamic schemas, examples and imports.

### Distribution aliases are not approved executable fixtures

Standard shell and echo paths can be final-component symlinks. A fixture which hashes their targets but supplies the aliases contradicts the regular-file execution contract. Retain already regular installed executables. When a final-component alias requires a copy, real shell bytes under their shell basename preserve behavior without a mock or an operating-system branch.

### Treat the chart default image as release metadata

A forward Runner candidate also needs an explicitly aligned chart appVersion, candidate version and installation examples. This changes version selection rather than Native activation or workload behavior.

### Match readiness to the endpoint the next operation needs

An official PostgreSQL image can briefly accept Unix-socket probes while initialization is still running. A TCP readiness probe distinguishes that temporary server from the final service without a fixed delay, retry expansion or database policy change. A growing integration suite also needs an aggregate allowance that includes all cases and deliberate deadline observations, while retaining their individual bounds.

### Align complete-suite qualification with candidate metadata

Qualify the full existing selector before reserving the forward candidate. Update the chart version, image default and installation examples together. Documentation edits alone do not change product qualification.

### Job-local tool setup does not cross workflow job boundaries

A tool installed in a separate backend job is absent from the unit job that runs the full PostgreSQL selector. Make that prerequisite explicit in each existing consuming job, using the same authenticated archive rather than skipping cases or substituting another executable.

### Prepare matched forward candidate metadata

Release metadata is separate from qualification setup. Advance installation examples and paired chart/image versions together while retaining the customer-facing behavior and recovery guidance.
