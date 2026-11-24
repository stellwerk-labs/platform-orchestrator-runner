#!/bin/sh
set -eu
source_dir="${1:?effective source directory required}"
output="${2:?absolute output executable required}"
architecture="${3:?target architecture required}"
case "$architecture" in amd64|arm64) ;; *) exit 2 ;; esac
case "$output" in /*) ;; *) exit 2 ;; esac
export GOTOOLCHAIN=go1.27.4 GOWORK=off GOENV=off CGO_ENABLED=0 GOMAXPROCS=2 GOMEMLIMIT=2GiB
unset GOROOT GOOS GOARCH GOAMD64 GOARM64 GOFLAGS GOEXPERIMENT GOPRIVATE GONOSUMDB GONOPROXY
export GOSUMDB=sum.golang.org GOPROXY=https://proxy.golang.org
go_binary="${OPENTOFU_GO:-$(go env GOROOT)/bin/go}"
export GOTOOLCHAIN=local
[ "$("$go_binary" env GOVERSION)" = go1.27.4 ]
export GOMODCACHE="$(dirname "$source_dir")/modules" GOOS=linux GOARCH="$architecture"
case "$architecture" in amd64) export GOAMD64=v1 ;; arm64) export GOARM64=v8.0 ;; esac
mkdir -p "$(dirname "$output")"
cd "$source_dir"
# Symbols are required by the binary scanner. This is not an official build.
"$go_binary" build -p=1 -mod=readonly -trimpath -buildvcs=false \
 -ldflags='-X github.com/opentofu/opentofu/version.rawVersion=1.13.9-stellwerk.remediation.20261123.1 -X github.com/opentofu/opentofu/version.dev=no' \
 -o "$output" ./cmd/tofu
