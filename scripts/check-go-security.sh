#!/usr/bin/env sh
set -eu
root="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
cd "$root"
release_go_version="go$(awk '$1 == "go" { print $2; exit }' go.mod)"
export GOTOOLCHAIN="$release_go_version" GOWORK=off GOENV=off CGO_ENABLED=0
unset GOFLAGS GOEXPERIMENT
checker=golang.org/x/vuln/cmd/govulncheck@v1.9.0
case "${1:-source}" in
  source)
    [ "$#" -le 1 ] || exit 2
    go run "$checker" ./...
    if [ -f shared/go.mod ]; then
      (cd shared && go run "$checker" ./...)
    fi
    ;;
  binary)
    [ "$#" -eq 2 ] && [ -f "$2" ] || exit 2
    case "$2" in /*) binary="$2" ;; *) binary="$PWD/$2" ;; esac
    # Run the scanner on the build host while inspecting the target executable.
    unset GOOS GOARCH GOAMD64 GOARM64
    # Keep text mode: the scanner's JSON mode reports findings with exit zero.
    go run "$checker" -mode=binary "$binary"
    ;;
  *)
    printf 'usage: check-go-security.sh [source | binary PATH]\n' >&2
    exit 2
    ;;
esac
