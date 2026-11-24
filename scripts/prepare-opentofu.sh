#!/bin/sh
set -eu
root="$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)"
output="${1:?private build directory required}"
case "$output" in /*) ;; *) output="$PWD/$output" ;; esac
export GOWORK=off GOENV=off GOSUMDB=sum.golang.org GOPROXY=https://proxy.golang.org GOMAXPROCS=2 GOMEMLIMIT=2GiB
unset GOROOT GOOS GOARCH GOAMD64 GOARM64 GOFLAGS GOEXPERIMENT GOPRIVATE GONOSUMDB GONOPROXY
checker_go="$(GOTOOLCHAIN=go1.26.11 go env GOROOT)/bin/go"
export OPENTOFU_GO="$(GOTOOLCHAIN=go1.27.4 go env GOROOT)/bin/go"
export GOTOOLCHAIN=local
"$checker_go" run -p=1 "$root/scripts/noticecheck" prepare-opentofu "$output" "$root"
"$OPENTOFU_GO" version -m "$OPENTOFU_GO" > "$output/toolchain-buildinfo.txt"
sha256sum "$OPENTOFU_GO" > "$output/toolchain-binary.sha256"
