#!/bin/sh
set -eu

chart=${1:-./charts/platform-orchestrator-kubernetes-agent-runner}
workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

common_values="--set platformOrchestrator.orgId=test-org --set platformOrchestrator.runnerId=test-runner --set gateway.privateKeyExistingSecret=runner-identity"

# shellcheck disable=SC2086
helm template runner "$chart" $common_values \
  --set gateway.mode=simple \
  --set gateway.url=https://api.example.test/runner-gateway >"$workdir/simple.yaml"
grep -q 'RUNNER_GATEWAY_URL: "https://api.example.test/runner-gateway"' "$workdir/simple.yaml"
if grep -q 'name: NATS_URL' "$workdir/simple.yaml"; then
  echo "simple-mode runner must not receive a NATS endpoint" >&2
  exit 1
fi

# shellcheck disable=SC2086
helm template runner "$chart" $common_values \
  --set gateway.mode=edge \
  --set gateway.url=https://api.example.test/runner-gateway \
  --set outbox.enabled=true \
  --set outbox.storageClassName=rwx >"$workdir/edge.yaml"
grep -q 'app.kubernetes.io/component: outbox-flusher' "$workdir/edge.yaml"
grep -q 'accessModes: \["ReadWriteMany"\]' "$workdir/edge.yaml"

# shellcheck disable=SC2086
helm template runner "$chart" $common_values \
  --set gateway.mode=airgap \
  --set protected-nats.enabled=true \
  --set protected-nats.container.env.NATS_AUTH_TOKEN.valueFrom.secretKeyRef.name=nats-auth \
  --set airgap.gatewaySecret=gateway-auth \
  --set airgap.nats.existingSecret=nats-auth >"$workdir/airgap.yaml"
grep -q 'app.kubernetes.io/component: protected-gateway' "$workdir/airgap.yaml"
grep -q 'name: runner-protected-nats' "$workdir/airgap.yaml"

# shellcheck disable=SC2086
if helm template runner "$chart" $common_values \
  --set gateway.mode=edge \
  --set gateway.url=https://api.example.test/runner-gateway >"$workdir/invalid-edge.yaml" 2>&1; then
  echo "edge mode without the durable outbox unexpectedly rendered" >&2
  exit 1
fi

if helm template runner "$chart" \
  --set platformOrchestrator.orgId=test-org \
  --set platformOrchestrator.runnerId=test-runner \
  --set gateway.url=https://api.example.test/runner-gateway >"$workdir/invalid-key.yaml" 2>&1; then
  echo "configured runner without an identity Secret unexpectedly rendered" >&2
  exit 1
fi

# Native diode reuses the registered UUID for both directions. Remote defaults
# above remain the legacy path.
cat >"$workdir/native.yaml" <<'EOF'
executionMode: native
native:
  replicas: 0
  configExistingSecret: native-config
  privateFilesExistingSecret: native-private
protected-nats:
  enabled: true
  container:
    env:
      NATS_AUTH_TOKEN:
        valueFrom:
          secretKeyRef:
            name: nats-auth
platformOrchestrator:
  orgId: test-org
  runnerId: test-runner
gateway:
  mode: airgap
  privateKeyExistingSecret: runner-identity
airgap:
  runnerUuid: 33333333-3333-4333-8333-333333333333
  gatewaySecret: gateway-auth
  nats:
    url: nats://owned-broker:4222
    existingSecret: nats-auth
diode:
  enabled: true
  existingClaim: courier
  signingKeyExistingSecret: diode-sign
  verificationKeyExistingSecret: diode-verify
EOF
helm template runner "$chart" -f "$workdir/native.yaml" \
  --set diode.role=central >"$workdir/native-central.yaml"
grep -q 'name: DIODE_AUTHORITATIVE_ORG, value: "test-org"' "$workdir/native-central.yaml"
grep -q 'name: DIODE_NATIVE_RUNNER_UUID, value: "33333333-3333-4333-8333-333333333333"' "$workdir/native-central.yaml"
grep -q 'po.v1.orgs.test-org.runners.0-33333333-3333-4333-8333-333333333333.commands' "$workdir/native-central.yaml"
helm template runner "$chart" -f "$workdir/native.yaml" \
  --set diode.role=protected >"$workdir/native-protected.yaml"
grep -q 'po.v1.orgs.test-org.runners.0-33333333-3333-4333-8333-333333333333.events.>' "$workdir/native-protected.yaml"
if grep -q 'name: DIODE_AUTHORITATIVE_ORG' "$workdir/native-protected.yaml"; then
  echo "forward importer must retain the generic command contract" >&2
  exit 1
fi
for value in '' 00000000-0000-0000-0000-000000000000; do
  if helm template runner "$chart" -f "$workdir/native.yaml" \
    --set diode.role=central --set "airgap.runnerUuid=$value" >"$workdir/native-invalid.yaml" 2>&1; then
    echo "Native diode with missing/zero UUID unexpectedly rendered" >&2
    exit 1
  fi
done
if helm template runner "$chart" -f "$workdir/native.yaml" \
  --set diode.role=central --set diode.commandSubject=foreign.commands >"$workdir/native-invalid.yaml" 2>&1; then
  echo "Native diode with foreign command route unexpectedly rendered" >&2
  exit 1
fi

# Two sites share the organization and central streams, but never a durable.
for site_uuid in 33333333-3333-4333-8333-333333333333 44444444-4444-4444-8444-444444444444; do
  helm template runner "$chart" -f "$workdir/native.yaml" \
    --set "airgap.runnerUuid=$site_uuid" --set diode.role=central >"$workdir/site-central.yaml"
  grep -q "name: DIODE_DURABLE, value: \"diode-forward-commands-test-org-$site_uuid\"" "$workdir/site-central.yaml"
  grep -q "name: DIODE_NATIVE_RUNNER_UUID, value: \"$site_uuid\"" "$workdir/site-central.yaml"
  grep -q 'name: DIODE_AUTHORITATIVE_ORG, value: "test-org"' "$workdir/site-central.yaml"
  grep -q "po.v1.orgs.test-org.runners.0-$site_uuid.commands" "$workdir/site-central.yaml"
  helm template runner "$chart" -f "$workdir/native.yaml" \
    --set "airgap.runnerUuid=$site_uuid" --set diode.role=protected >"$workdir/site-protected.yaml"
  grep -q "name: DIODE_DURABLE, value: \"diode-reverse-events-test-org-$site_uuid\"" "$workdir/site-protected.yaml"
  grep -q "po.v1.orgs.test-org.runners.0-$site_uuid.events.>" "$workdir/site-protected.yaml"
done
# The legacy branch keeps the exact existing durable names and generic policy.
for role in central protected; do
  helm template runner "$chart" -f "$workdir/native.yaml" \
    --set executionMode=remote --set gateway.mode=simple \
    --set gateway.url=https://api.example.test/runner-gateway \
    --set "diode.role=$role" \
    --set diode.commandSubject=po.v1.orgs.test-org.runners.test-runner.commands >"$workdir/legacy-$role.yaml"
  if grep -q 'name: DIODE_NATIVE_RUNNER_UUID' "$workdir/legacy-$role.yaml"; then
    echo "legacy importer must retain its generic policy" >&2
    exit 1
  fi
done
grep -q 'name: DIODE_DURABLE, value: "diode-forward-commands"' "$workdir/legacy-central.yaml"
grep -q 'name: DIODE_DURABLE, value: "diode-reverse-events"' "$workdir/legacy-protected.yaml"
