# Environment observations

`MODE=plan_only` together with the exact marker
`OBSERVATION_PROTOCOL=environment-observation/v1` selects an isolated execution
path for Terraform and OpenTofu. An unsupported marker or a marker on any other
mode fails before runner creation. Plain `MODE=plan_only` keeps ordinary behavior.
It requires the configured backend to implement the optional `ObservationRunner`
interface. Unknown modes and unknown IaC backends fail before any deployment.
Existing deploy, destroy and plan_only workflows keep their existing behavior.

The observation downloads the exact deployment bundle supplied by Core and uses
the ordinary SSH agent and provider credentials. Core owns baseline selection,
authority, idle admission and persistent per-Runner capacity. The Runner creates
no separate semaphore and cannot decide whether a deployment is a valid baseline.

The path runs init, verifies the existing artifact manifest, and executes a full
normal plan with `-input=false -refresh=true -lock=true -lock-timeout=0s`. It does
not run apply, refresh-only, output, state push or state refresh commands. A random
0700 directory holds the sensitive binary plan and is removed on success, error
and context cancellation. SIGKILL and host failure cannot execute deferred cleanup;
the Job filesystem must remain ephemeral. Observation-configured runners refuse
the ordinary Init/Plan/Apply/Destroy/Version/Output entry points.

Init/plan stdout and all stderr are discarded inside Runner. Show stdout is
limited to 32 MiB and never logged or persisted. Child environment variables
`TF_CLI_ARGS*`, `TF_LOG*` and `TOFU_LOG*` are removed so inherited targeting,
refresh overrides and diagnostic-file logging cannot affect the observation.
Failures expose a fixed stage message, without provider diagnostics or command
error text. This applies to bundle/factory/SSH setup failures too.

The parser accepts JSON plan format 1.2, validates a numeric semantic tool version,
requires `errored=false`, rejects duplicate JSON keys and trailing/prefixed data,
and rejects deferred changes, non-passing checks, imports and unsupported action
combinations. Terraform must explicitly report `complete=true`. The tested OpenTofu
1.10.0, bundled 1.13.2 and local 1.13.5 builds omit that field; completeness follows from successful unrestricted full
plan execution without deferred changes. Unknown formats or incomplete results
produce failure, never a successful clean result.

Only the optional generated `observation_result` transport field is returned:
schema version, complete flag, tool/version, observed resource changes, proposed
resource changes and an output change count. Each change contains a per-run opaque
sequence ID, a validated provider resource type and actions. No-op entries are
excluded. Resource addresses, for_each keys, attribute names/values, output names
or values, expressions and diagnostics never enter the summary. Each resource
array is bounded to 2000 input entries; output changes are bounded to one million.
Oversize observations fail instead of returning a partial summary.

New summaries reserve `resource_N` IDs (positive decimal ordinal) to correlate
one resource instance across both change arrays within that result. The Runner
uses the resource address, resource type and optional deposed instance key only
in a transient map; none of those private identity values or their hashes is
emitted. Input order can differ between the arrays. Duplicate instances within
either array fail validation. Entries without an address retain separate
`observed_N` or `proposed_N` IDs and cannot be paired. Historical independent IDs
must not be paired by array position or resource type, and IDs are never stable
across observations.


Run `make test-observation-integration` with installed `terraform` and `tofu`.
`IAC_TEST_BINARIES=/path/to/tofu,terraform` selects Runner integration binaries.
The external change fixture downloads official hashicorp/local 2.5.2 into its
temporary Terraform configuration. It adds no Runner module dependency. Tests
exercise real filesystem changes, local backend flock contention, a real HTTP
state backend, in-flight cancellation, state write counters and executor
stdout/log/result canaries. HTTP and local backends are qualified locally;
production S3/GCS/Azure state backends and deployed Job images require independent
qualification before enablement.

Protocol schema source: DP `openapi/spec.yaml`, copied and generated with the
existing pinned oapi-codegen v2.8.0. The handwritten client wrapper preserves its
old deployment mode aliases after upstream adds overlapping mode enums.

References: [Terraform JSON format](https://developer.hashicorp.com/terraform/internals/json-format),
[OpenTofu JSON format](https://opentofu.org/docs/internals/json-format/).

The wire mode intentionally remains the already-supported read-only plan_only.
The immutable public stable b7ae8ca3 entry point rejects unknown modes through
configuration validation; its executor would fall through to Apply only if given
an unsupported configuration directly. This is a defense-in-depth compatibility
choice, not a proven shipped CLI apply vulnerability. The real legacy binary
fixture proves that marked plan_only produces no state or local-exec side effect,
and has no observation_result. Core must reject that missing structured result
as unavailable. It also proves the old CLI rejects MODE=observation.

Run the opt-in legacy fixture with OBSERVATION_LEGACY_RUNNER_BINARY pointing to a
binary compiled from git archive of immutable public b7ae8ca3. The optional real
remote wire test uses OBSERVATION_KUBE_CONFIG, OBSERVATION_KUBE_CONTEXT (explicit
Kind context) and OBSERVATION_NATS_URL. It exercises real JetStream delivery, HTTP
gateway authentication, executeCommand, Kubernetes Job creation and durable ack.
Its Jobs remain suspended to avoid pulling or starting images; both marked and
plain plan_only environments are preserved through the remote agent.

The normal Docker `artifact-integration` target also runs the HTTP-backend
observation regression with the bundled OpenTofu, with network access disabled.
It checks locking, cancellation and unchanged backend state. This functional
check does not clear the engine's separate vendor security findings; see
[the maintenance notes](releases/security-maintenance.md).

## Site-local durable Runner

The native DBOS Runner accepts the same signed `stellwerk.pipeline/v1` envelope
with the dedicated `strategy: observation` and `request.mode: observation`. This
is a private native transport, separate from the compatible Job wire above. Both
the signer and local catalog must explicitly advertise `environment-observation/v1`.
The command requires a frozen compiled bundle and ordinary deployment callback
token. Recipes, task steps, approvals, cost policies, source/base plans, output
recipients and metadata extraction are forbidden before durable admission.

A dedicated observation stage invokes the guarded `ObservationRunner.Observe`,
without entering deployment preparation hooks, plan retention or Apply. It honors
the local backend configuration and ignores inherited `TF_DATA_DIR` and
`TF_WORKSPACE` in addition to the existing CLI/debug overrides. A successful
summary is encrypted in the ordinary immutable result store, then delivered by
the normal deployment-result callback. Recovery reuses that recorded result.
No plan, raw logs, outputs, metadata or effect claim is recorded. The ordinary
environment admission remains held until terminal local completion.

Native local cancellation is deliberately conservative: once the observation
stage has atomically entered `observing`, `Engine.Cancel` refuses the request
with `observation_in_progress`. It does not claim process termination or release
the environment. Cancellation that wins before this transition prevents the
observer from starting any subprocess. Core keeps its own observation cancellation
request and admission accounting until the actual terminal result is settled.

Private work directories and temporary binary plans are removed after execution,
including normal errors and cancellation. Abrupt process/host termination can
leave private work files until operational cleanup, as with the existing native
execution path; these directories must remain restricted to the Runner identity.

Offline native proof uses an existing official local provider binary:

```sh
IAC_TEST_LOCAL_PROVIDER_BINARY=/absolute/path/terraform-provider-local \
IAC_TEST_LOCAL_PROVIDER_VERSION=2.9.0 \
GOTOOLCHAIN=go1.26.11 go test -tags=integration ./internal/rollout \
  -run '^TestNativeObservation' -count=1 -timeout=180s
```

The test creates a filesystem provider mirror with no direct registry fallback.
It deploys a fixture-owned `local_file`, edits that exact file externally, and
proves a real drift summary with unchanged state bytes and external file content.
It also checks immutable replay, secret canaries, zero retained/effect/log rows and
an empty local work directory after completion.
