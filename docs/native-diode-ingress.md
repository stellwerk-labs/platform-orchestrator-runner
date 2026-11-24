# Native reverse diode ingress

A central importer for each customer-controlled site binds its existing Ed25519
verification key to exactly 1 organization and 1 server-issued Native registration
UUID. Provision the key, scope and owned spool/processed ledger together. A site
key cannot select another site's publication subject or stream deduplication ID.
The central NATS credentials remain central-only; customer sites cannot publish
raw events or use an unbound generic importer into the shared Native event stream.
The downstream DP still validates deployment authority against its frozen target.
This boundary does not prove that an authorized site executed honestly.

For a central Native reverse process, require all of these settings before launch:

```sh
: "${DIODE_AUTHORITATIVE_ORG:?registered organization is required}"
: "${DIODE_NATIVE_RUNNER_UUID:?canonical nonzero registration UUID is required}"
: "${DIODE_VERIFY_KEY_FILE:?centrally pinned site verification key is required}"
: "${DIODE_INPUT_DIR:?absolute owned input directory is required}"
: "${DIODE_PROCESSED_DIR:?absolute owned processed directory is required}"
: "${DIODE_QUARANTINE_DIR:?absolute owned quarantine directory is required}"
: "${DIODE_LEDGER_PATH:?absolute owned durable ledger path is required}"
export DIODE_AUTHORITATIVE_ORG DIODE_NATIVE_RUNNER_UUID DIODE_VERIFY_KEY_FILE
export DIODE_INPUT_DIR DIODE_PROCESSED_DIR DIODE_QUARANTINE_DIR DIODE_LEDGER_PATH
export DIODE_EXPECT_ATTACHMENT=log
exec /opt/runner/runner diode-import
```

Use existing NATS URL/authentication/CA settings for the central broker. Set each
site's directories and ledger independently. Missing 1 binding, invalid organization
or noncanonical/zero UUID refuses startup before directory or broker effects.
Both bindings select the Native reverse policy and require log attachment checks.
Both absent deliberately preserve the generic forward/legacy contract, whose
verification-key holder must be trusted to choose subjects, IDs and headers.
An unbound launch cannot detect that the operator intended Native mode.

A verified Native frame must carry one of `rollout-progress`, `deployment-result`,
`rollout-control-result` or `log-object-ready`. Its strict bounded envelope must
match the provisioned organization and `0-<UUID>` route and the connected Gateway's
existing event/deployment identity rules. Its signed subject and ID must equal the
values derived from that configured scope, type and event ID; its creation time must
match the signed event and it must have no expiry. Imported headers are discarded.
The publisher generates its own broker metadata. The exact signed frame, `Data`
and log ciphertext remain unchanged. Native log metadata, ciphertext checks and
immutable-object conflicts still run before publication and processed-ledger use.
Rejected frame quarantine records are diagnostic writes, not accepted ledger entries.

The existing chart supports this through `executionMode: native` together with
`diode.enabled: true`. Set `platformOrchestrator.orgId`, `airgap.runnerUuid`, and
site-specific signing/verification Secrets and PVC. The Native branch requires a
canonical nonzero UUID and valid organization, derives UUID command/event routes,
and configures both bindings on the central reverse importer. Native forward and
reverse exporter durables include the configured organization and UUID so sites
can coexist on shared streams; remote exporter durable names remain unchanged. An explicitly supplied
`diode.commandSubject` must equal the derived command route. The protected forward
importer remains generic because it carries signed commands. Keep the registered
domain `platformOrchestrator.runnerId` for the protected Gateway's domain identity.
A relay-only central release can set the existing `native.replicas: 0`; the existing
Native config/private Secret references are still required by the chart.
`executionMode: remote` remains the existing trusted legacy relay contract.

## Existing Native exporter cutover

The command stream uses WorkQueue retention and refuses overlapping consumer
filters. An existing `diode-forward-commands` durable with this site's exact UUID
command filter prevents the new org/UUID-scoped durable from binding, even after
the old exporter disconnects. Do not delete streams or other sites' consumers.

For an existing installation, perform a controlled cutover:

1. Pause producers for this site's command route. Verify the old durable's exact
   filter and ownership against the configured organization and registration UUID.
2. Let its pending and ack-pending counts reach `0`, then stop the old exporter.
3. Recheck that same durable's ownership, exact filter and both zero counts.
   Retire only that verified consumer. Preserve the stream, PVC, courier spool
   and processed ledgers; leave other sites' disjoint consumers in place.
4. Start the new exporter and verify its org/UUID-scoped durable binds the exact
   expected filter. Resume producers only after that binding is confirmed.

If ownership, filter or drained state cannot be verified, stop the cutover and
resolve it administratively. The application performs no automatic deletion.

Source/component qualification of this importer and chart does not qualify a physical
diode, the complete offline recipe, HA, or a particular customer's installation.
The later offline fixture must check immutable courier frame/`Data`/ciphertext custody
separately from canonical broker subject/ID/creation metadata and dropped headers.
