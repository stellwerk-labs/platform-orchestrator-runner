#!/bin/sh
# User-supplied pod metadata and scheduling values must pass the values schema
# and reach every pod the chart renders: the remote agent Deployment and the
# native worker StatefulSet.
set -eu

chart=${1:-./charts/platform-orchestrator-kubernetes-agent-runner}
workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

cat >"$workdir/values.yaml" <<'EOF'
platformOrchestrator:
  orgId: test-org
  runnerId: test-runner
gateway:
  mode: simple
  url: https://api.example.test/runner-gateway
  privateKeyExistingSecret: runner-identity
native:
  configExistingSecret: native-config
  privateFilesExistingSecret: native-private
podAnnotations:
  example.test/owner: platform-team
podLabels:
  example.test/cost-center: cc-42
nodeSelector:
  kubernetes.io/arch: arm64
affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
        - matchExpressions:
            - key: example.test/pool
              operator: In
              values: [runners]
EOF

check() {
  file=$1
  for expected in 'example.test/owner: platform-team' 'example.test/cost-center: cc-42' \
    'kubernetes.io/arch: arm64' 'key: example.test/pool'; do
    grep -q "$expected" "$file" || { echo "$file is missing '$expected'" >&2; exit 1; }
  done
}

helm template runner "$chart" -f "$workdir/values.yaml" \
  --show-only templates/deployment.yaml >"$workdir/deployment.yaml"
check "$workdir/deployment.yaml"

helm template runner "$chart" -f "$workdir/values.yaml" --set executionMode=native \
  --show-only templates/native-workers.yaml >"$workdir/native-workers.yaml"
check "$workdir/native-workers.yaml"

# Kubernetes stores annotations, labels and node selectors as strings.
if helm template runner "$chart" -f "$workdir/values.yaml" \
  --set podAnnotations.example\\.test/replicas=2 >"$workdir/invalid.yaml" 2>&1; then
  echo "non-string pod annotation unexpectedly rendered" >&2
  exit 1
fi
