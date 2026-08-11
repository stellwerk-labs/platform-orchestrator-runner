#!/usr/bin/env bash

set -euo pipefail

chart=${1:?chart path is required}
field=${2:?field name is required}

case "$field" in
  name | version) ;;
  *)
    echo "unsupported chart field: $field" >&2
    exit 2
    ;;
esac

awk -F ':[[:space:]]*' -v field="$field" '
  $1 == field {
    count++
    value = $2
    sub(/[[:space:]]+#.*/, "", value)
    gsub(/^[[:space:]]+|[[:space:]]+$/, "", value)
    gsub(/^"|"$/, "", value)
  }
  END {
    if (count != 1 || value == "") {
      exit 1
    }
    print value
  }
' "$chart"
