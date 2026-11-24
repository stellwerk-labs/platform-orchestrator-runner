# Native delivery custody

The native agent freezes one versioned delivery beside each existing rollout_events row before its first new publication. The encrypted record holds the exact progress envelope and optional deployment-result envelope, including event identity, timestamp and payload. It also binds the encrypted content to its event row. A transaction selects the stored command/event and current authoritative terminal revision; caller-supplied progress cannot authorize a result. Existing no-effect receipt checks remain in that transaction.

Retries load the recorded pair before consulting newer progress. An absent result stays absent. A previously frozen result remains present even if diagnostics advance progress. The stable deployment-result identity has one owner. Current post-task failure is nonterminal, so repair can complete and produce its first successful result without repeating Apply. Supporting replacement of an already terminal ordinary result would require an explicit DP transition protocol; this batch does not add one.

The agent publishes exact envelopes through the existing authenticated Gateway /events endpoint, which preserves their bytes. It sends progress followed by the optional result and then acknowledges only that local event row. A lost response leaves the row pending and repeats the same pair. Existing stop-at-first-error scheduling remains. Old outbox rows freeze on first new delivery; this cannot reconstruct unknown bytes published by older software. Retain rollout_deliveries with its event/command identity until a separately agreed GC/replay/backup policy allows removal.

Ack, Reject and Retry failures are explicit unconfirmed outcomes. Diagnostics contain only operation, bounded class, HTTP status and a static code. Remote error bodies, tokens, receipts, signed inputs and results are excluded. An action error is not proof that the broker retained the command: a lost Reject response may follow successful termination. There is no durable refusal/absence proof in this batch.

Run `make test-agent-delivery-integration` with installed Docker, Python3, OpenTofu and the cached pinned NATS image. The script creates one labelled disposable broker, binds a loopback port that survives its restart and cleans up only its own container. It refuses a missing required broker instead of silently skipping. `AGENT_DELIVERY_KEEP_LOGS=1` preserves its private broker log. Tests use actual Gateway TLS authentication, receipts, real file-backed NATS and real OpenTofu, including response loss after persistence and broker/engine restart. Separate process tests SIGKILL immediately after delivery commit and verify encrypted DB/WAL recovery.

This qualifies Gateway publication and local custody, not DP application or global exactly-once execution. Accepted-import replay is tested before the outer command expiry. Native late-expiry delivery, durable admission/refusal, transient refusal classification and exact recovery after MaxDeliver belong to subsequent batches. Legacy job /results still creates its own timestamp.

The legacy SQLite development profile retains its original wire identity. The separately qualified native PostgreSQL profile supports multiple worker slots with shared DBOS coordination and generation fencing; see [PostgreSQL storage](postgres-rollout-storage.md). The Agent delivery target covers legacy execution plus native Gateway/consumer isolation. The separate HA target runs signed native commands through actual CLI workers. Neither target activates a live site, qualifies database failover, or changes existing unknown-effect fences.


## Native registration authority

A native Agent uses the server-issued registration UUID from its explicit
PostgreSQL configuration. Its human Runner slug remains the URL and Core lookup
identity. The JWT signs both that slug and `runner_uuid`; Gateway resolves the
key and UUID together from the same Core response/cache entry. Only
`type: native-agent` selects native authority. Backfilled UUIDs on the 5 legacy
Runner types do not select it. Disabling native inventory retains the key/UUID
needed to finish delivery and acknowledge receipts.

For an explicitly configured static native Gateway, set
`RUNNER_GATEWAY_STATIC_RUNNER_UUID` alongside the existing static organization,
slug and public-key file. Empty UUID retains legacy static authority. Never map
a human URL slug beginning with the reserved `0-` prefix.

| Identity | Native value | Purpose |
| --- | --- | --- |
| Human Runner ID | Existing slug | URL, Core lookup and signed inner command |
| Transport Runner ID | `0-<canonical registration UUID>` | NATS subject, consumer, receipt and outer envelope |
| Transport message ID | `stellwerk-native-v1:<SHA256>` | Stream-wide dedup scoped to org, native token, direction, type and original inner ID |
| Inner command/event ID | Unchanged | Existing signature, execution and projection identity |

Native JWTs never retry without their UUID. A new Gateway rejects missing or
foreign UUIDs. An old Gateway may ignore the extra claim, but its legacy-slug
outer envelope cannot be imported by the native Agent. Existing SQLite v1
commands still require legacy transport. Native HTTP events are restricted to
`rollout-progress` and `deployment-result`, with protocol and exact envelope
scope checked before publication.

Native consumers use create-only configuration with an explicit native marker.
A conflicting preexisting durable is refused, preserving its pending messages
and cursor. Before bootstrap/create/bind, native startup requires a connected
broker reporting a canonical stable version of at least2.10.0; unknown,
disconnected, malformed, negative-component and prerelease values refuse.
Older servers can ignore create-only actions and update retained configuration.
Legacy consumers retain their existing old-broker behavior.

Every eligible native broker/reconnect peer must support that capability. The
version read is not atomic with a later request or proof about another stream
leader. Mixed-old cohorts are unsupported; operators must qualify the complete
cohort. The pinned2.14.0 configuration is qualified here, and a higher version
passing the minimum is not a qualification claim. No automatic upgrade occurs.

Run the separate required CI fault target after caching its immutable fixture:

```sh
docker pull nats@sha256:d402af4147fc7ba334ecc45b1cb14a6f46f32ad6a36873fbd1a6a37ce42452dd
make test-unsupported-native-nats
```

This target owns a loopback-only2.9.25 broker and proves native refusal leaves
stream/consumer configuration, creation identity, pending delivery and cursors
unchanged, while actual legacy publish/fetch/Ack remains available. It has fixed
test selection and never silently skips or borrows a shared broker. The old
image is a test fault fixture, never part of the production image.
Native PostgreSQL format4 introduced refusal of old format3 journals containing
already frozen slug-based deliveries; it does not rewrite their encrypted
records or reinitialize storage. Use fresh schemas with explicit retained
identity, never overwrite a site's old history to bypass this refusal.
Current format7 retains the private Kubernetes destination/trust binding introduced by format5 and additionally requires the private-identity-derived Native catalogue commitment. Prior raw-catalogue formats refuse without rewriting metadata or ciphertext. The delivery journal remains UUID scoped. Provision the retained age identity before offline export; see [the catalogue contract](offline-runner-catalogue.md).

## Activation gates

This is a private-source qualification checkpoint. The shared hmessaging owner
adds the canonical token/message-ID helpers without a new module dependency.
A real published owner version and honest consumer pin are required before
ordinary CI or a release can build this source without the private workspace.

Native rollout workers currently use `gateway_url` and UUID-authenticated HTTP;
their rollout configuration does not grant raw broker publication. Generic
Gateway/importer NATS configuration accepts operator-supplied credentials and
does not itself enforce a tenant trust classification. Keep raw publication to
shared central streams restricted to trusted platform publishers/normalizers.
Untrusted customer sites or steps must use the UUID-authenticated Gateway or the
future signer-scoped import boundary that recomputes trusted transport identity.
Site-local brokers may be controlled by that site's operator, with no other
site's traffic sharing their dedup domain.

Subject ACLs alone do not isolate dedup in a shared central stream: a raw
publisher authorized only for siteA's subject can still choose siteB's exact
`Nats-Msg-Id` and suppress B before DP checks run. A later target/inbox refusal
cannot undo this broker effect. If direct raw publication from mutually
untrusted sites is required, separate broker accounts/streams by trust domain;
this checkpoint adds no such isolation. Before activation, qualify the actual
credential/ingress deployment: untrusted identities cannot publish directly to
a shared central stream, or their dedup domains are isolated. The current
2site HTTP tests qualify the trusted normalizer, not arbitrary raw-publisher
ACLs or that later deployment gate.

Upgrade or remove stream permissions from every old Gateway and unscoped
legacy relay before native activation. Updated legacy HTTP ingress reserves
the native message-ID prefix, but an older publisher sharing the stream can
still manufacture a native-prefixed dedup ID. Broker credentials/ACL inventory
is an operator prerequisite; source checks alone cannot police old binaries.

Native direct deployment-token URL routes refuse native slugs and the reserved
transport namespace before bundle, result, key or log access. This guard is
partial: a valid native Deployment token supplied through a valid legacy URL
alias still needs the separate authoritative frozen-Deployment target gate
before every such object/result access. DP target projection/dispatch,
signer-scoped diode transport and native Environment assignment remain separate
required integration gates. Do not activate native sites before they are done.

Native command format7 binds all7accepted target fields and removes the unused legacy Deployment token. The outer envelope remains protocol1/type `execute-rollout-v1`; the UUID route and scoped message identity remain separate from inner domain IDs. The strict frozen-command decoder now supplies Native NoEffect command authority while existing permanent effect/unknown provenance still decides whether a receipt is eligible. This source contract does not open DP producer/dispatch, direct HTTP, controls/logs or custody gates. See [Native command authority](native-command-authority.md).

## Direct Deployment access profiles

A Gateway's Agent key cache authenticates the current Runner registration. It does
not establish ownership of a historical Deployment. Before enabling Native
producers, explicitly select and qualify one of these Gateway profiles for every
entry point that can reach the Native object stores and event stream.

| Profile | Configuration | Direct Deployment-token routes |
| --- | --- | --- |
| Existing legacy-only | Existing connected configuration without the new authority URL, or static identity without UUID | Existing behavior retained; this profile is not qualified for shared Native material |
| Connected mixed | `CONTROL_PLANE_URL` plus `RUNNER_GATEWAY_DEPLOYMENT_AUTHORITY_DP_URL` | Require an affirmative stored legacy receipt before bundle read, result publication, log write or enabled retained-key forwarding |
| Static Native protected | Existing complete static org/Runner/key configuration plus `RUNNER_GATEWAY_STATIC_RUNNER_UUID` | Bundle, results and logs refuse locally; retained-key forwarding must be disabled and returns 404 |

Connected mixed mode uses the DP base URL, independently of the retained-plan
feature and `STELLWERK_RETAINED_PLAN_DP_URL`. For example, an operator can set
`RUNNER_GATEWAY_DEPLOYMENT_AUTHORITY_DP_URL=http://platform-orchestrator-data-plane:8080`
on a trusted internal service network. External connections require verified
HTTPS and an appropriately provisioned system trust store. There is no skip-TLS
switch. URLs must be absolute HTTP/HTTPS bases without userinfo, query, fragment,
encoded path or dot segments. A canonical deployment-controlled base path is
supported. Invalid settings fail before the Gateway connects to NATS.

The lookup is `GET /v1/orgs/{org}/runners/{runner}/deployments/{deployment}/legacy-access`
with exactly one existing `X-Deployment-Token`. DP authenticates the request before
storage and returns the 5 JSON fields `legacy`, `organization_id`, `deployment_id`,
`runner_id`, and `environment_uuid` only for the exact stored legacy Deployment.
Native and malformed nonnull targets, wrong scope and missing rows cannot produce
a receipt. This internal compatibility read is always mounted, even with retained
plans disabled, and does not grant human-user access or a new CRUD API.

The Gateway accepts only a bounded affirmative JSON receipt matching the request
and canonical nonzero Environment UUID. It uses a 5-second total client timeout,
request cancellation and a 4096-byte response bound. Redirects, unavailable or
malformed responses fail closed. There is no retry, cache or fallback to current
Runner metadata. Incoming Authorization/Cookie headers are not forwarded; only the
existing Deployment token is sent to the configured DP endpoint. Log URLs must
match the receipt's Environment before reading the body or touching object storage.

Static Native configuration rejects both a central deployment-authority URL and
retained-key forwarding. Its direct refusals need no Core/DP HTTP and do not change
Agent authentication, whole-command transport or local execution. The older
protected remote-Job chart remains a legacy operator; setting an identity does not
convert it into a Native worker. Chart opt-in wiring and Native worker packaging
are separately qualified deliverables.

A legacy-only Gateway sharing raw object credentials and the token salt with a
Native realm can still be a bypass. Upgrade every reachable entry point into that
realm or isolate the legacy cohort before activation. Existing disconnected
legacy behavior remains available in its legacy-only realm. Arbitrary shared mixed
offline legacy/Native object access is not qualified by this configuration, and no
live state or key migration is implied. Native producer activation still requires
actual Core/IAM enrollment, frozen target and complete deployment qualification.
