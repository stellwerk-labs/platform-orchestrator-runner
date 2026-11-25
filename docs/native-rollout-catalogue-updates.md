# Updating a Native execution catalogue

> **RC feature:** This page describes Deployment Pipelines in the coordinated
> release candidate (Runner `3.4.0-rc.1`). Pipelines are not available in the
> current stable release from `main`. Use matching Pipeline RC components; Native
> execution requires explicit operator activation.

Use `rollout-catalogue-update` when changing a Native site's private tool environment,
IaC executable inventory, capabilities or pricing credentials. It retains the site
identity, registration UUID, saved plans, encrypted execution history and result
receipts. Ordinary startup continues to refuse an unapproved catalogue change.

This procedure requires the coordinated Native RC, including atomic storage
verification and worker activation. It does not support a rolling transition
from earlier incompatible Native profiles. Stop and revoke those workers first.
Existing SQLite development storage is unchanged.

1. Pause Core admission, publishers and remote/offline carriers. Complete accepted
   executions, or resolve their existing recovery fences. Pending approval,
   unfinished Plans, uncertain effects and post-task recovery all block maintenance.
2. Stop supervisors, Agents and workers. Keep the retained age key, signing trust,
   org, Runner slug and registration UUID, application/system schemas and physical
   state backend destination/trust unchanged. Prepare the desired actual image and
   private configuration. Use immutable executable files during this operation.
3. Run the offline command with the previously published current tools digest and
   a separate private PostgreSQL schema-owner/migration-admin DSN file. Flags must
   precede the mode, following the Runner's existing flag parser:

   ```sh
   ROLLOUT_CONFIG_FILE=/private/desired-runner.json \
     runner --expected-catalogue=PREVIOUS_TOOLS_DIGEST \
       --admin-dsn-file=/private/migration-admin.dsn rollout-catalogue-update
   ```

   The principal must own, or inherit owner privileges on, both configured schemas.
   Merely having UPDATE on `rollout_storage` is insufficient. The command neither
   provisions roles nor changes permissions. Existing trusted-runtime DML grants
   include metadata writes; this command is not protection against a malicious
   database administrator or arbitrary direct SQL writes.
4. Export the actual public catalogue with `runner rollout-catalogue`, publish it to
   Core, reissue the external activation record with current exact bindings and a
   fresh activation ID after reviewed reconciliation, then start the new supported
   cohort, check readiness and resume admission.
   Existing completed command/result replay remains available. Newly submitted
   commands carrying the former digest refuse. Saved plans from the former
   execution profile require a new plan before Apply.

The operation has a 30-second deadline, including initialization/storage locks.
A busy worker row refuses immediately instead of waiting through worker adoption.
Retry after stopping the remaining conforming worker or settling its work. The
transaction changes only the catalogue binding and worker generations. A retry
whose desired catalogue is already current still verifies authority, identity,
structure, retained commands and quiescence, then leaves generations unchanged.
Conflicting expected digests and any failed verification roll back all changes.

Success writes only `{"status":"catalogue_ready"}` to stdout. Errors are fixed
public codes. Keys, DSNs, private tool configuration and SQL error bodies are never
printed by this operation. No workflows are started or resumed by maintenance.
Pending metadata-only control receipts can be delivered after a safely terminal
execution. The original receipt input and result are retained.

Refreshing state backend authentication for the same destination does not require
catalogue maintenance. Changing its server, trust or other destination identity
continues to refuse. Worker generation fencing protects application mutation and
external-effect admission; it does not claim to prevent autonomous DBOS bookkeeping
by every stale process. Stopping the old cohort remains part of the procedure.
