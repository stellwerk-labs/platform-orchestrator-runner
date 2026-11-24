# PostgreSQL rollout storage prerequisite

This profile stores Runner rollout commands, plans, outputs, logs, effect
receipts and frozen delivery records in site-local PostgreSQL. Payloads retain
existing age encryption. Terraform/OpenTofu state still uses its configured
state backend; PostgreSQL does not change that backend.

## Drain before routine worker replacement

Use the existing `ROLLOUT_CONFIG_FILE` and private API token file for local
operator controls. Flags precede the command:

```sh
runner rollout-status
runner rollout-ready
runner --wait --timeout=3m rollout-drain
```

Drain irreversibly closes stage admission for this process. It leaves a healthy
Apply or admitted task running until its native outcome is recorded. A successful
wait means `active_stages` reached `0` while `draining` is true and `accepting`
is false; the worker remains alive until the
supervisor explicitly stops it. If the wait times out, the gate stays closed and
the execution continues. Do not turn that timeout into an automatic process kill.

`rollout-status` reports `authority_alive`, `accepting`, `draining` and
`active_stages`. `rollout-ready` exits unsuccessfully during drain. Encrypted
event/result delivery and exact Ack continue while authority is alive. New
imports and local approval/cancel/repair requests receive transient availability
responses; another current slot may serve them. Work reaching a later stage
parks until explicit stop and normal supervised recovery, without a durable
business-failure checkpoint. Actual storage/generation failure still fail-stops
immediately, including during drain.

If polling sees a restarted, accepting worker at the same endpoint, it refuses
the result rather than draining that replacement implicitly. Drain is a
point-in-time precondition. A supervisor helper must identify the concrete
process/container/Pod incarnation it will stop; this command does not make the
later supervisor stop atomic.

The native parent also drains itself when it receives SIGTERM or SIGINT. It
keeps the independent DBOS root and Agent delivery alive until admitted stages
finish, then calls public Shutdown before stopping the Agent and private HTTP
server. Repeated graceful signals do not escalate into killing Apply. A real
host deadline is reported as an error after safe drain; it does not cancel an
admitted operation. Startup cancellation/deadlines still apply to Engine.Open.
Unexpected HTTP failures, storage/generation loss and the first Shutdown failure
remain nonzero outcomes, including when no stage is active.

Signals sent directly to an effect child, SIGKILL and a container supervisor's
final kill are faults. Supervisor packaging must send ordinary TERM only to the
native parent; systemd's default control-group TERM would kill its children
before this drain can protect them. Runtime profiles and their live Linux
qualification remain separate work.

After successful drain, stop and replace only that stable slot, verify its
replacement is ready, then proceed to the next slot. Preserve the same slot ID,
database, age key, inventory and queue budgets. Forced shutdown, a node failure
or killing a process before drain completes can leave an unknown effect and
requires the existing reconciliation handling. This control does not qualify
database failover or backup restoration; those require separate operations gates.

The shared execution queue admits multiple stable worker slots. Each slot holds
its own connection-bound lock and durable generation. Native command transitions
also lock their command row and check the current generation. A returning old
process cannot write through a supervised replacement. Pending workflows need a
supervisor to restart their stable slot; public DBOS recovery then re-enqueues
them onto the shared queue. Another eligible slot may adopt only after the prior
owner's durable generation has been superseded.

This source checkpoint is not a complete customer HA deployment. Production
packaging, supervised replacement, database failover/durability and quarantined
restore qualification remain required. A connection loss does not prove that an
external effect did not happen: irreversible effect claims survive, and an
unproven Apply is held rather than repeated.

## Durable connection profile

Native and DBOS connections require a writable primary, `fsync=on`,
`full_page_writes=on` and a nonempty mandatory synchronous standby policy.
Every connection is checked before slot activation or DBOS use, including
reconnects. The shared DSN forces `synchronous_commit=on`, preserving explicit
`remote_apply`; `off`, `local` and `remote_write` cannot weaken it. Opaque
startup `options` (including `PGOPTIONS`) are refused; use direct DSN parameters.
A missing configured standby blocks write acknowledgement rather than enabling
asynchronous fallback. The operator still owns replication, catch-up and
old-primary fencing. Connection checks do not qualify failover or restore.
The required isolated HA target provisions a real second synchronous standby.

## Explicit configuration

Add this object to the existing `ROLLOUT_CONFIG_FILE` JSON configuration:

```json
{
  "runner_uuid": "3cbdc8ce-60d5-4ccc-9900-c7b6ae874897",
  "postgres": {
    "dsn_file": "/run/secrets/rollout-postgres-dsn",
    "identity_file": "/run/secrets/rollout-artifact.agekey",
    "worker_id": "worker-0",
    "concurrency": 4,
    "worker_concurrency": 2,
    "application_schema": "stellwerk_rollouts",
    "system_schema": "stellwerk_dbos"
  }
}
```

Copy the actual server-issued Runner registration UUID into `runner_uuid`; the
example above is illustrative. All worker slots share that exact UUID. Keep
the existing organization, Runner slug, trust key, tools, capabilities and
private scratch directory settings. `worker_id` is stable across replacement
of this worker. Each worker has its own private scratch directory; all members
of the eventual site pool need the same retained age identity and tool catalogue.
The configured PostgreSQL profile never falls back to SQLite and never creates
a replacement age key. Only omitting PostgreSQL AND leaving `runner_uuid` empty retains the existing
development SQLite v1 profile. Either mismatched pair refuses before opening
storage. Native commands require signed pipeline/v2 plus that UUID in both
command and request; legacy versions cannot execute in this profile.

Provision the age identity once, before Native catalogue export, and retain it securely alongside backups. The exporter and runtime use the same bounded single bare-X25519 file policy. The public Native catalogue is a domain-separated HKDF/HMAC commitment keyed by this private identity, not by its public Recipient or Agent signing key. Mount
it read-only; the Runner reads it without changing its permissions. Secret
files must be regular, nonempty, bounded files with no access for other users.
Group-readable secret mounts are accepted. DSNs may use PostgreSQL URI or
keyword syntax. Supply the complete endpoint, database, role and TLS policy in
the secret file. No DSN or parser details appear in startup errors.

Use a dedicated logical-site database. DBOS and application schemas share that
database so native acceptance plus queue admission and approval/send transactions
remain atomic. Queue configuration uses public never-update registration and
exact persisted limits/identity verification; a worker cannot silently overwrite
the pool policy. Approval waits count toward workflow concurrency, and central
Runner capacity stays1pool-wide budget. A pooler must preserve PostgreSQL session
affinity; transaction-mode pooling does not preserve slot locks and is unsupported.

## Initialize, then verify and run

1. Start with a fresh private scratch directory and a fresh application schema.
   `rollouts.sqlite`, its WAL/SHM or a local `identity.agekey` refuse PostgreSQL
   startup in that directory. Existing spike histories are not migrated.
2. Using an initialization role, run `runner rollout-storage-init` with the
   explicit configuration. This initializes the native schema under a separate
   migration transaction lock and calls public `dbos.Migrate`; it launches no
   workflow and connects to no gateway. Unknown existing DBOS history cannot be
   attached to a new application schema.
3. Grant a runtime role USAGE on both schemas, DML on all their tables and
   USAGE/SELECT on their sequences. DBOS's public `dbos.PermissionStatements`
   describes its role grants. Provision the application grants separately;
   future migrations may need adjusted privileges.
4. Point `dsn_file` at the runtime role and run `runner rollout`. Normal startup
   does no DDL. It verifies initialized format, actual native columns/defaults/
   constraints, organization/site/trust, tool catalogue, workflow version and
   the artifact identity before DBOS launch with `SkipMigrations: true`.

Initialization is restartable after interruption. A readiness marker is set
only after DBOS migration succeeds; runtime refuses an incomplete initialization.
Missing/damaged schemas are not silently recreated by runtime. Wrong keys,
unsupported formats and mismatched site/catalogue/version refuse recovery.
Operators must not rename schemas or edit the journal to bypass refusal. Native format7 requires the keyed catalogue before storage access and rejects previous raw-catalogue profiles without rewriting metadata or ciphertext or recovering workflows. Identity or private task-environment rotation changes the immutable profile; there is no implicit migration or credential unlock. SQLite/v1 raw digests and history remain unchanged.

SQLite WAL compaction and plaintext migration do not apply to this format.
Fresh PostgreSQL payloads are encrypted from their first insert. Database/WAL,
backups and the retained identity remain sensitive, and backups need consistent
DBOS plus application schemas. Backup/PITR rollback and cloned sites require
quarantine and reconciliation before execution; this checkpoint does not supply
an activation or restore protocol.

## Qualification

On Linux, export the pinned shipped test tools before PostgreSQL qualification:

```sh
test_tools="$(mktemp -d)"
docker buildx build --target pipeline-test-export --output "type=local,dest=$test_tools" .
chmod 0755 "$test_tools/tofu" "$test_tools/infracost"
export PATH="$test_tools:$PATH"
export INFRACOST_TEST_BINARY="$test_tools/infracost"
```

The export contains Linux executables. Other host platforms require separately
qualified native OpenTofu and pinned `v0.10.49+stellwerk.1` Infracost executables,
with `INFRACOST_TEST_BINARY` set to the actual estimator path. The mandatory cost
comparison gate never substitutes an installed unqualified estimator or skips.

Run against a disposable PostgreSQL endpoint, using a private secret file:

```sh
RUNNER_POSTGRES_TEST_DSN_FILE=/private/test-dsn make test-rollout-postgres
```

The explicit gate fails when its fixture is missing. It uses unique schemas and
actual DBOS/OpenTofu, preserves command/artifact/log/delivery confidentiality,
checks exact saved-plan use and approval transaction behavior, and terminates
only its own application backend to prove that a stale reconnect cannot write
through a replacement slot. The complete process qualification is a separate
required target with invocation-owned PostgreSQL/NATS and a built real Runner:

```sh
make test-rollout-ha-integration
```

It requires the pinned PostgreSQL/NATS images already present (CI pulls the exact
digests), actual shipped OpenTofu, the pinned estimator selected by
`INFRACOST_TEST_BINARY`, and Docker. It never reuses a live service or
journal. Tests run separate CLI processes and preserve their private fault logs
when `HA_TEST_KEEP_LOGS=1` is set. This is not a claim of PostgreSQL HA, arbitrary
survivor takeover or exactly-once external infrastructure effects.

## Worker failure and pending recovery

Unexpected native storage loss or a superseded generation disables readiness,
new imports/process admission and authoritative publication. The host stops DBOS
using its public Shutdown API before cancelling its parent or draining transport
loops. This retains pending work without recording a failed deployment or an
error checkpoint; ordinary application exit reports a bounded fatal reason to
the supervisor. There is no library os.Exit or blind infrastructure retry loop.

Ordinary cancelled human requests and the exact expected environment uniqueness
conflicts remain domain outcomes. Workflow storage deadlines, unexpected SQL
errors and an aborted COMMIT returning a ROLLBACK tag stop the worker. No raw SQL,
arguments, DSN or private database diagnostics are published. Original effect
claims and committed exact results determine recovery, never elapsed time.

Format2 introduces the slot/command/effect ownership journal. Runtime refuses a
format1 PostgreSQL prerequisite database rather than rewriting its history.
Existing retained SQLite development history and unknown-effect fences remain
unchanged; no automatic backend migration or key rotation is supplied.

## Retained authority and format boundaries

Native PostgreSQL format7 binds its existing site row to the registration UUID
and runtime `stellwerk.runner.rollout/v2` before workflow recovery. Same-slug
configuration or copied keys from another UUID do not grant that authority.
Its existing encrypted identity check also binds the selected private Kubernetes
destination/trust; same-target inline credential refresh is allowed. Earlier
Native formats, including format6 and the previous raw-catalogue format5, refuse without metadata or ciphertext rewrite.
See [the supported backend profile](native-kubernetes-backend.md).
Old unbound PostgreSQL format2 experiments refuse both runtime open and storage
initialization; neither repairs or migrates them. Use fresh schemas and retained
identity explicitly. Never initialize blank PostgreSQL history as a continuation
of an existing SQLite site or its unresolved effects.

Native saved plans use UUID-bound retained-plan/v3. Legacy retained-plan/v1 and
provider-aware retained-plan/v2 remain supported only with an empty UUID; those
existing schemas/subjects/ciphertext are not reinterpreted. Source and Apply-child
UUIDs must match before scratch creation or effect admission. Existing source
lineage, single-use, state-base, recipe and material checks remain required.

The isolated HA target now uses native UUID-bound Gateway authentication,
disjoint transport subjects, scoped message IDs and encrypted receipts around
signed v2 commands. Format3 journals with already frozen human-slug deliveries
refuse both open and initialization without rewriting ciphertext or metadata.
The [native delivery gates](native-delivery.md#activation-gates) still require
published shared-contract dependencies, upgraded/scoped legacy publishers,
signer-scoped diode transport and authoritative frozen DP target validation
before any native Environment assignment or live activation.

## Native command format7

Format7 additionally requires the complete accepted execution target and token-free Native commands. It refuses format6 Open and initialization before worker activation or DBOS recovery, even if that older profile has a valid keyed catalogue. No in-place rewrite, migration, reset or automatic state adoption is provided. Retain the old encrypted history, private identity, claims and qualified format6 binary for controlled reconciliation. A new schema is not continuation of an unresolved old site. SQLite/v1 is unchanged.

The target's Core typed state-configuration SHA is opaque at the site. It is not the independently retained private Kubernetes destination/trust identity. Standalone Engine local state remains available for isolated runtime tests, but it is not a Core-registered backend. See [the Native command contract](native-command-authority.md).

The optional chart Native profile and plain Linux/systemd examples are described
in [Native Runner packaging](native-runner-packaging.md). They preserve the remote
chart default, external PostgreSQL authority and explicit initialization/runtime
roles. Packaging source alone does not qualify an image or service lifecycle.
