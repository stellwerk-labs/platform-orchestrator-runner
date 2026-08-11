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
