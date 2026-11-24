#!/bin/sh
set -eu
umask 077

# This unsupported release is an owned fault fixture, never a production image.
image="nats@sha256:d402af4147fc7ba334ecc45b1cb14a6f46f32ad6a36873fbd1a6a37ce42452dd"
private_dir="$(mktemp -d "${TMPDIR:-/tmp}/stellwerk-unsupported-nats.XXXXXX")"
container=""
cleanup() {
    if [ -n "$container" ]; then
        docker logs "$container" > "$private_dir/nats.log" 2>&1 || true
        docker rm --force "$container" >/dev/null
    fi
    if [ "${UNSUPPORTED_NATS_KEEP_LOGS:-0}" = 1 ]; then
        printf 'Private unsupported broker log: %s/nats.log\n' "$private_dir"
    else
        rm -rf "$private_dir"
    fi
}
trap cleanup EXIT INT TERM
tests="$(GOWORK=off go test -tags=integration ./internal/natstransport -list '^TestUnsupportedNATSPreservesNativeCustodyAndLegacyDelivery$')"
case "$tests" in
    *TestUnsupportedNATSPreservesNativeCustodyAndLegacyDelivery*) ;;
    *) printf 'Required unsupported native broker regression is missing\n' >&2; exit 1 ;;
esac
container="$(docker run --detach --pull=never --label com.stellwerk.test=unsupported-native-nats -p 127.0.0.1::4222 "$image" -js -sd /data)"
export UNSUPPORTED_NATS_URL="nats://$(docker port "$container" 4222/tcp)"
export UNSUPPORTED_NATS_ISOLATED=1
GOWORK=off go test -race -tags=integration ./internal/natstransport -run '^TestUnsupportedNATSPreservesNativeCustodyAndLegacyDelivery$' -count=1 -timeout=60s -v
