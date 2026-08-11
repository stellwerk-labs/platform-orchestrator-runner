#!/usr/bin/env bash

set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
parser="$repo_root/scripts/chart-metadata.sh"
fixture=$(mktemp)
trap 'rm -f "$fixture"' EXIT

cat >"$fixture" <<'YAML'
apiVersion: v2
name: runner-chart
version: 0.2.0
dependencies:
  - name: nats
    version: "2.14.0"
YAML

test "$(bash "$parser" "$fixture" name)" = "runner-chart"
test "$(bash "$parser" "$fixture" version)" = "0.2.0"

if bash "$parser" "$fixture" dependency >/dev/null 2>&1; then
  echo "unsupported fields must fail" >&2
  exit 1
fi

printf '%s\n' "chart metadata parser tests passed"
