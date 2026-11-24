#!/bin/sh
set -eu
root="$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)"
output="${1:?output directory required}"
: "${OPENTOFU_BUILD_DIR:?authenticated effective OpenTofu build required}"
: "${OPENTOFU_BINARY:?actual source-built OpenTofu executable required}"
: "${OPENTOFU_GO:?absolute Go1.27.4 executable required}"
case "$output" in /*) ;; *) output="$PWD/$output" ;; esac
arch="${GOARCH:?explicit target required}"
checker_go="$(GOTOOLCHAIN=go1.26.11 go env GOROOT)/bin/go"
(unset GOOS GOARCH GOAMD64 GOARM64; GOTOOLCHAIN=local "$checker_go" run -p=1 "$root/scripts/noticecheck" opentofu "$output" "$OPENTOFU_BUILD_DIR" "$arch" "$OPENTOFU_BINARY" "$root")
work="$(mktemp -d)"
trap 'rm -r -- "$work"' EXIT HUP INT TERM
export GOTOOLCHAIN=local GOWORK=off GOENV=off GOSUMDB=sum.golang.org GOMAXPROCS=2 GOMEMLIMIT=2GiB
unset GOROOT GOOS GOARCH GOAMD64 GOARM64 GOFLAGS GOEXPERIMENT GOPRIVATE GONOSUMDB GONOPROXY
[ "$("$OPENTOFU_GO" env GOVERSION)" = go1.27.4 ]
# Pin and authenticate the existing collector version under this vendor compiler.
# TMPDIR may be nested in Runner or vendor source; never inherit that module.
printf 'module stellwerk.invalid/opentofu-license-acquisition\n\ngo 1.26.11\n' > "$work/go.mod"
metadata="$(cd "$work"; "$OPENTOFU_GO" mod download -json github.com/google/go-licenses/v2@v2.0.1)"
printf '%s\n' "$metadata" | grep -F 'h1:ti+9bi5o7DKbeeg5eBb/uZTgsaPNoJaLCh93cRcXsW8=' >/dev/null
(cd "$work"; GOBIN="$work/bin" "$OPENTOFU_GO" install github.com/google/go-licenses/v2@v2.0.1)
export PATH="$(dirname "$OPENTOFU_GO"):$work/bin:$PATH"
export GOMODCACHE="$OPENTOFU_BUILD_DIR/modules" GOOS=linux GOARCH="$arch" CGO_ENABLED=0 GOFLAGS='-mod=readonly -p=1'
ignore=github.com/opentofu/opentofu,github.com/jmespath/go-jmespath
(cd "$OPENTOFU_BUILD_DIR/source"; go-licenses save ./cmd/tofu --ignore "$ignore" --save_path "$work/classified")
(cd "$OPENTOFU_BUILD_DIR/source"; go-licenses report ./cmd/tofu --ignore "$ignore") > "$output/licenses.csv"
printf '%s\n' 'github.com/jmespath/go-jmespath,NOASSERTION,Apache-2.0' >> "$output/licenses.csv"
(unset GOOS GOARCH GOAMD64 GOARM64 GOFLAGS; GOTOOLCHAIN=local "$checker_go" run -p=1 "$root/scripts/noticecheck" opentofu-archive "$work/classified" "$output/corresponding-source.tar.gz" "$OPENTOFU_BUILD_DIR" "$root")
