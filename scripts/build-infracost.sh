#!/bin/sh
set -eu
source_dir="${1:?private source directory required}"
output="${2:?absolute executable output required}"
architecture="${3:?target architecture required}"
case "$architecture" in amd64|arm64) ;; *) exit 2 ;; esac
case "$output" in /*) ;; *) exit 2 ;; esac
export GOTOOLCHAIN=go1.26.11 GOWORK=off GOENV=off CGO_ENABLED=0 GOMAXPROCS=2
go_binary="$(go env GOROOT)/bin/go"
export GOTOOLCHAIN=local
export GOMODCACHE="$(dirname "$source_dir")/modules"
export GOOS=linux GOARCH="$architecture" GOAMD64=v1 GOARM64=v8.0
unset GOFLAGS GOEXPERIMENT
cd "$source_dir"
# Symbol tables are required for accurate artifact vulnerability matching.
"$go_binary" build -p=2 -mod=readonly -trimpath \
 -ldflags='-X github.com/infracost/infracost/internal/version.Version=v0.10.49+stellwerk.1' \
 -o "$output" ./cmd/infracost
