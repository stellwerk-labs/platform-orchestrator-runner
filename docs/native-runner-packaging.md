# Optional Native Runner deployment

The chart defaults to the existing `remote` Job launcher. `executionMode: native`
selects stable PostgreSQL worker slots. It requires a coordinated Native image,
not the chart's historical default `appVersion`. The examples in `examples/native/`
are trial inputs, not an assertion that a released image already qualifies this
profile. No database dependency, migration controller or automatic upgrade is added.

## Site configuration and custody

Use an external session-affine PostgreSQL database for both application and DBOS
schemas. Do not use transaction-mode PgBouncer. All slots share the same enrolled
organization, Runner slug, server-issued UUID, retained age identity, signing trust,
backend destination, installed tool catalogue and immutable queue budgets.
Each slot has its own private scratch and stable worker ID.

`examples/native/runtime.json` is an existing Runner configuration. Replace every
placeholder and its illustrative UUID with the actual registration. The empty PG
`worker_id` is bound from `ROLLOUT_WORKER_ID`: the chart uses the stable Pod name;
systemd uses its instance name. Explicit matching IDs work; conflicts, malformed
IDs and nonempty binding without PostgreSQL refuse before trust/storage access.
With no variable, existing JSON behavior is unchanged. Offline `rollout-catalogue`
does not use the binding and still works before enrollment.

Installed tool/profile changes require the explicit [catalogue maintenance
procedure](native-rollout-catalogue-updates.md), with admission/carriers paused,
the old cohort stopped and the schema-owner credential. `rollout-catalogue-update`
uses the same PG-only worker-ID binding, so a shared JSON with empty `worker_id`
requires a valid `ROLLOUT_WORKER_ID`. Initialization is not catalogue repair.

Pre-provision the configuration Secret with key `rollout.json`. Provision runtime
files separately, matching the exact JSON paths: runtime-role `postgres-dsn`,
retained `site.agekey`, externally issued `activation.json`, `verification-key`,
`agent-private-key.pem`, `operator-token` and optional Gateway CA/backend/pricing files. The operator token requires at least
32 characters. No Secret value belongs in Helm values, argv or Git. Never generate
a replacement age key to join or repair an existing site.

Mounts use `0440` and the default nonroot UID/GID/fsGroup `1000`. The storage
reader follows projected symlinks and validates the opened regular file. DSN and
age identity files have their existing size/permission checks; do not assume every
other trust file has the same validator. A same-UID init container creates owned
`0700` `/work/native` and `/work/home` children inside per-Pod emptyDir. `/tmp` is
also writable. PostgreSQL plus retained keys are durable authority, never scratch.
Changing user/group settings requires actual mounted-file access proof.

Native pods receive fixed config/slot/HOME variables. Legacy `extraEnvVars`,
PRIVATE_KEY and remote Job/NATS settings are not imported. Existing additional
volumes/mounts remain available for explicitly installed trusted tools/providers.
Do not shadow the reserved config/private/work/tmp mounts. The stock image has
OpenTofu and Infracost, no Python or Terraform. BYO Terraform/task images must have
matching approved installed inventory. Default `tools: []` does not promise a
receipt-task executable. State remains in its configured backend; Core registration
supports Kubernetes, S3, GCS and AzureRM. The explicit inline Kubernetes backend
profile is described in `native-kubernetes-backend.md`; it is not required for all
backends. Replace/remove the example Kubernetes path according to the real profile.

## Fresh initialization and runtime roles

1. Export the catalogue with the actual image and retained identity, then create
   the Core registration and insert its UUID into both configurations.
2. Provision separate initialization config/files Secrets. `initialization.json`
   uses fixed ID `storage-init`; its `postgres-dsn` is the DDL-capable role.
   It needs retained age/trust/backend/catalogue inputs, no Gateway authentication.
   Runtime Secrets contain the DML-only DSN and never initialization credentials.
3. Install native mode with `native.replicas=0` and explicit initialization enabled
   using `initialization-values.yaml`. Verify every existing site worker, Agent,
   publisher and supervisor is actually stopped. A rendered replica count does
   not prove that. This is a normal non-hook Job with `backoffLimit: 0`.
4. Observe successful `rollout-storage-init`, verify runtime grants, issue the
   external activation record below, disable initialization and apply `values.yaml`
   with 2 workers. Wait for authenticated readiness on each exact slot. Never use normal startup to perform DDL.

An existing completed/failed initialization Job is not an automatic retry loop.
Inspect its outcome before explicitly removing/recreating it for a fresh approved
initialization operation. Never initialize, reset, re-key or migrate the retained
SQLite test-drive site with these examples. Incompatible storage/profile refuses;
retained history and unknown-effect claims must remain intact.

## Ingress profiles

Connected/edge workers use the existing JSON Gateway URL and CA, without direct
NATS publication. Every replica of the actual connected Gateway must set its
existing `RUNNER_GATEWAY_DEPLOYMENT_AUTHORITY_DP_URL` to the trusted DP base URL;
normal Core registration/authentication remains enabled. This chart does not deploy
that connected Gateway and has no unused value purporting to configure it.

For the chart's protected static Gateway, also set `protected-nats.enabled: true`
and configure its token Secret reference at
`protected-nats.container.env.NATS_AUTH_TOKEN.valueFrom.secretKeyRef.name`. With
the default token profile, use the Secret named by `airgap.nats.existingSecret`.
Supply `gateway.mode: airgap`, the existing org/slug/public-key/NATS/receipt Secrets
and canonical nonzero `airgap.runnerUuid`. The chart adds
`RUNNER_GATEWAY_STATIC_RUNNER_UUID` only when
set. The worker JSON must use the matching protected Gateway URL/CA and identity.
Do not combine static Native identity with connected deployment authority or
retained-key forwarding. Update/isolate every old Gateway ingress replica before
claiming this profile is active. Existing signer-scoped commands, controls,
encrypted log artifacts and disconnected courier qualification remain required;
this chart does not provide the complete offline transport choreography.

`outbox.enabled` is a legacy remote-Job feature and refuses Native mode. Native
PostgreSQL already retains its own delivery outbox.

## Drain and replace one stable slot

The StatefulSet uses `OnDelete`. Never treat a chart upgrade as an automatic
rolling upgrade or scale-down. Preserve its release/fullname and stable slot names.
A headless Service provides StatefulSet identity, with no ports, operator Service,
Ingress or public health endpoint. Readiness executes authenticated loopback
`runner --timeout=3s rollout-ready`; there is no effect-killing liveness probe.

For each slot, using the intended kubecontext and namespace:

1. Retain Pod UID, resourceVersion and the `native-worker` containerID/restartCount
   from `kubectl get pod <slot> -o json`. Address this exact Pod/container, not a
   Service. Keep the old image/config/catalogue until its active work settles.
2. Run `kubectl exec <slot> -c native-worker -- /opt/runner/runner --wait
   --timeout=24h2m rollout-drain` as one command. A timeout keeps the Pod alive and
   closed to new stages. Investigate; do not convert it to a forced kill.
3. Re-read the Pod/container identity, then run authenticated `rollout-status`.
   Require unchanged UID/containerID/restartCount, `draining: true`,
   `accepting: false` and `active_stages: 0`. If identity changed or admission
   reopened, stop and inspect that replacement explicitly.
4. Re-read immediately before deletion and retain the current resourceVersion.
   Create `delete-options.json` as below, substituting exact UID/resourceVersion.
   Send the raw API request, preserving the configured grace:

   ```json
   {"apiVersion":"v1","kind":"DeleteOptions","preconditions":{"uid":"POD_UID","resourceVersion":"RESOURCE_VERSION"}}
   ```

   ```sh
   kubectl delete --raw /api/v1/namespaces/NAMESPACE/pods/SLOT -f delete-options.json
   ```

   A conflict means re-read and investigate, never force deletion. This raw-client
   invocation follows kubectl's body-forwarding implementation, but still needs the
   actual disposable-cluster stale-precondition qualification before release.
5. Wait for the same slot's replacement and authenticated readiness before the next
   slot. Preserve all command/effect/plan-use/history claims. Do not force delete,
   rename the pool, alter keys/catalogue/budgets/format, or auto-scale down as a retry.

These checks do not make external drain and stop atomic. A container can restart
inside the same Pod between reads. The direct parent signal handler is the remaining
safety mechanism: ordinary TERM closes admission and waits for admitted stages,
keeping delivery alive. Forced shutdown, node failure and final kill can leave
unknown effects; reconcile rather than repeat Apply.

Kubernetes grace defaults to and must remain at least `86520s`: maximum accepted
Apply `86400s` plus `120s` bounded bookkeeping/shutdown/scheduling allowance.
This requires the actual qualified execution-budget implementation in the selected
image. It bounds one admitted active stage, not a whole pipeline or extended signed
authority. No preStop consumes the same grace window. A grace expiry is a fault,
not permission to retry infrastructure.

## External activation and managed restore

Native PG startup requires `activation_file` outside journal/scratch/backup bundles.
Its private strict JSON record is at most 4 KiB (no duplicate/unknown/trailing data):

```json
{"version":1,"activation_id":"32 lowercase random hex characters","state":"active","reconciliation":"fresh","org_id":"ACTUAL_ORG","runner_uuid":"ACTUAL_UUID","application_schema":"ACTUAL_APP_SCHEMA","system_schema":"ACTUAL_DBOS_SCHEMA","runtime_version":"stellwerk.runner.rollout/v2","dsn_sha256":"ACTUAL_PRIVATE_DSN_FILE_SHA256","identity_sha256":"ACTUAL_AGE_FILE_SHA256","catalogue_sha256":"ACTUAL_OFFLINE_TOOLS_DIGEST"}
```

The operator fences any previous cohort, completes fresh initialization or records
actual reconciliation, obtains exact DSN/key file SHA256 values and the actual
`rollout-catalogue` tools digest, and generates a fresh random 16-byte activation ID.
Install the reviewed record atomically as private 0400/0600 standalone input or
`activation.json` in the existing 0440 projected runtime Secret. Both slots use the
same record, DSN/key bytes and catalogue. Initialization must not receive or issue it.
Only active plus fresh/reconciled may launch; fresh is solely for a genuinely new
journal. Missing, revoked, quarantined, unreconciled or mismatched authority refuses
before Engine/DBOS, import, HTTP or delivery startup, without disclosing bindings.

For bound catalogue/config/DSN/key/runtime changes, drain and stop/fence all old
workers, perform offline/admin maintenance, obtain current exact bindings, issue a
reviewed replacement record with a fresh ID, then restart. Even a DSN/key byte change
requires reissue. Stale records refuse; catalogue update never renews activation.
For restore, stop supervisors and fence every worker, Core/DP publisher, Gateway,
NATS/file/diode courier and flush/replay carrier before cloning the backup. Revoke
or remove activation and launch configuration, retain original later-history DB,
key, receipts and external state, and exclude active authority from restore bundles.
DP keeps `RESTORE_QUARANTINE=true`; Core/Gateway remain externally fenced. Inspect
without workflow Launch. Unresolved later effects remain held, never cleared or
converted to NoEffect. Only actual reconciliation permits active/reconciled reissue.
This startup-only file is not watched. Stop/fence before revoking; a secret older-DB
replacement with an unchanged active file is not automatically detectable.

## Plain Linux/systemd

Provision user/group `stellwerk-runner`, root-owned group-readable `0440` site files
and runner-owned per-slot `/var/lib/stellwerk/native/worker-0` parents. Adapt
`linux-runtime.json` for each slot and install it at
`/etc/stellwerk/native/worker-0/rollout.json`. The JSON listen address is loopback;
separate processes on the same host need distinct ports. Set the correct per-slot
scratch path. Run the separate initialization configuration once with DDL authority;
never mount it into ordinary service startup.

Install `stellwerk-runner@.service`, then supervise stable instance `worker-0`.
`KillMode=mixed` sends the initial TERM only to the main Runner, whose signal path
waits for active work. `TimeoutStopSec=infinity` disables the ordinary systemd stop
timer; a long pending stop is investigated, not silently converted to SIGKILL.
Mixed mode also automatically final-kills remaining cgroup children when the main
process exits, even with infinite timeout. Successful drain should leave no effect
child; a crash/premature main exit can kill children and requires unknown-effect
reconciliation. Do not use `KillMode=process/none` or disable final cleanup.

Drain the exact instance with its mounted config/credential, verify process
incarnation and closed/idle status, then stop/restart that instance. A process change
invalidates the old drain result. Actual Linux systemd qualification is required;
a macOS unit-file parse cannot prove service behavior.

## Qualification boundaries

Prepared tests exercise the real loader and real Helm renders with parsed
Kubernetes resources, including complete legacy parity against a frozen parent
chart. They do not prove the actual image, mounts, session-role grants or lifecycle.
Before customer use, qualify the coordinated Linux image with 2 stable slots,
external disposable PG, real supported shared state backend and Core/IAM/Gateway
registration. Include saved Plan across slot loss, one real Apply, PID1 TERM during
Apply, stale delete preconditions, undrained replacement protection and force-stop
uncertainty. Check UID/group/regular target modes and readonly root. Run systemd
separately on an actual Linux service manager. Keep normal module/image/signature,
provenance/notices, backup/restore, plugin and disconnected transport obligations.

References: [StatefulSet OnDelete](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/#update-strategies),
[Pod termination](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#termination-of-pods),
[headless Service semantics](https://kubernetes.io/docs/concepts/services-networking/service/#headless-services),
[Service validation permits no headless ports](https://github.com/kubernetes/kubernetes/blob/v1.35.0/pkg/apis/core/validation/validation.go#L6067),
[kubectl raw DELETE body](https://github.com/kubernetes/kubectl/blob/v0.35.0/pkg/cmd/delete/delete.go#L297),
[systemd mixed mode](https://www.freedesktop.org/software/systemd/man/latest/systemd.kill.html#KillMode=),
[systemd stop deadline](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html#TimeoutStopSec=).
