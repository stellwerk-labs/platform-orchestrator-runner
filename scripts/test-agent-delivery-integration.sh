#!/bin/sh
set -eu
umask 077

# This server is private to this invocation. Never reuse a live/shared NATS URL.
image="${AGENT_DELIVERY_NATS_IMAGE:-nats@sha256:ddb480f4b97d90f183123e96bbc7c96ab2a126883f7a380531cc208fc8ba9ca7}"
private_dir="$(mktemp -d "${TMPDIR:-/tmp}/stellwerk-agent-delivery.XXXXXX")"
container=""
cleanup() {
    if [ -n "$container" ]; then
        docker logs "$container" > "$private_dir/nats.log" 2>&1 || true
        docker rm --force "$container" >/dev/null
    fi
    if [ "${AGENT_DELIVERY_KEEP_LOGS:-0}" = 1 ]; then
        printf 'Private broker log: %s/nats.log\n' "$private_dir"
    else
        rm -rf "$private_dir"
    fi
}
trap cleanup EXIT INT TERM

command -v tofu >/dev/null
port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
container="$(docker run --detach --pull=never --label com.stellwerk.test=agent-delivery -p "127.0.0.1:$port:4222" "$image" -js -sd /data)"
address="$(docker port "$container" 4222/tcp)"
export AGENT_DELIVERY_NATS_URL="nats://$address"
export AGENT_DELIVERY_NATS_CONTAINER="$container"
export AGENT_DELIVERY_ISOLATED=1
# Agent qualification deliberately restarts this broker. Finish each package
# before the next so transport-only tests cannot race that owned fault.
GOWORK=off go test -race -p 1 -tags=integration ./internal/rolloutagent ./internal/gateway ./internal/natstransport ./internal/diode -run '^(TestAgent|TestNativeGateway|TestNativeConsumer|TestNativeControlResultSignedFileCarrierPreservesExactBytes$)' -count=1 -timeout=230s -v
