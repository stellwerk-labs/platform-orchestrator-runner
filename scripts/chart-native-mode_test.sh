#!/bin/sh
set -eu
# NATIVE_CHART_BASELINE must point to the frozen, dependency-complete parent chart.
: "${NATIVE_CHART_BASELINE:?set the frozen parent chart directory}"
exec go test -tags=chartintegration ./internal/chartpackaging -count=1 -timeout=180s
