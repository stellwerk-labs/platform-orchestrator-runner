#!/bin/sh
set -eu
root="$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)"
output="${1:?output required}"
: "${INFRACOST_SOURCE_DIR:?pinned Infracost source directory required}"
: "${INFRACOST_BUILD_DIR:?patched Infracost build inputs required}"
: "${INFRACOST_BINARY:?source-built Infracost executable required}"
case "$output" in /*) ;; *) output="$PWD/$output" ;; esac
go_binary_dir="$(go env GOROOT)/bin"
GOARCH_TARGET="$GOARCH"
# Authenticate exact target/source/declared licence bytes before narrow ignores.
(unset GOOS GOARCH GOAMD64 GOARM64; go run "$root/scripts/noticecheck" infracost "$output" "$INFRACOST_SOURCE_DIR" "$GOARCH_TARGET" "$INFRACOST_BINARY" "$root")
work="$(mktemp -d)"
trap 'rm -r -- "$work"' EXIT HUP INT TERM
ignore=github.com/infracost/infracost,go.mozilla.org/gopgagent,github.com/jmespath/go-jmespath
(cd "$INFRACOST_BUILD_DIR/source"; export PATH="$go_binary_dir:$PATH" GOTOOLCHAIN=local GOMODCACHE="$INFRACOST_BUILD_DIR/modules" GOFLAGS="-mod=readonly"; go-licenses save ./cmd/infracost --ignore "$ignore" --save_path "$work/classified")
(unset GOOS GOARCH GOAMD64 GOARM64; go run "$root/scripts/noticecheck" infracost-archive "$work/classified" "$output/corresponding-source.tar.gz")
(cd "$INFRACOST_BUILD_DIR/source"; export PATH="$go_binary_dir:$PATH" GOTOOLCHAIN=local GOMODCACHE="$INFRACOST_BUILD_DIR/modules" GOFLAGS="-mod=readonly"; go-licenses report ./cmd/infracost --ignore "$ignore") > "$output/licenses.csv"
printf '%s\n' 'go.mozilla.org/gopgagent,NOASSERTION,Apache-2.0' 'github.com/jmespath/go-jmespath,NOASSERTION,Apache-2.0' >> "$output/licenses.csv"
