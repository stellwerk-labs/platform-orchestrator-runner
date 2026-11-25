#!/bin/sh
set -eu
umask 077
test -x "${INFRACOST_TEST_BINARY:?pinned Infracost binary required}"

# Every service and journal belongs to this invocation. No live endpoint is read.
nats_image="${HA_TEST_NATS_IMAGE:-nats@sha256:ddb480f4b97d90f183123e96bbc7c96ab2a126883f7a380531cc208fc8ba9ca7}"
postgres_image="${HA_TEST_POSTGRES_IMAGE:-postgres@sha256:81e77a4833487b8132fe4b00bedd39dc3f74b8641e387f19312a48f7de218c2e}"
private_dir="$(mktemp -d "${TMPDIR:-/tmp}/stellwerk-rollout-ha.XXXXXX")"
nats_container=""
postgres_container=""
postgres_standby=""
postgres_network=""
cleanup() {
    if [ -n "$nats_container" ]; then
        docker logs "$nats_container" > "$private_dir/nats.log" 2>&1 || true
        docker rm --force "$nats_container" >/dev/null
    fi
    if [ -n "$postgres_standby" ]; then
        docker logs "$postgres_standby" > "$private_dir/postgres-standby.log" 2>&1 || true
        docker rm --force --volumes "$postgres_standby" >/dev/null
    fi
    if [ -n "$postgres_container" ]; then
        docker logs "$postgres_container" > "$private_dir/postgres.log" 2>&1 || true
        docker rm --force --volumes "$postgres_container" >/dev/null
    fi
    if [ -n "$postgres_network" ]; then
        docker network rm "$postgres_network" >/dev/null
    fi
    if [ "${HA_TEST_KEEP_LOGS:-0}" = 1 ]; then
        printf 'Private HA qualification logs: %s\n' "$private_dir"
    else
        rm -rf "$private_dir"
    fi
}
trap cleanup EXIT INT TERM
command -v tofu >/dev/null
python3 - "$private_dir" <<'PY'
from pathlib import Path
import secrets,sys
path=Path(sys.argv[1])/"postgres.env"
path.write_text("POSTGRES_USER=runner_ha_test\nPOSTGRES_DB=runner_ha_test\nPOSTGRES_PASSWORD="+secrets.token_hex(24)+"\n")
path.chmod(0o600)
PY
nats_container="$(docker run --detach --pull=never --label com.stellwerk.test=rollout-ha -p 127.0.0.1::4222 "$nats_image" -js -sd /data)"
postgres_container="$(docker run --detach --pull=never --label com.stellwerk.test=rollout-ha --env-file "$private_dir/postgres.env" -p 127.0.0.1::5432 "$postgres_image")"
export AGENT_DELIVERY_NATS_URL="nats://$(docker port "$nats_container" 4222/tcp)"
export AGENT_DELIVERY_NATS_CONTAINER="$nats_container"
export AGENT_DELIVERY_ISOLATED=1
postgres_address="$(docker port "$postgres_container" 5432/tcp)"
python3 - "$private_dir" "$postgres_address" <<'PY'
from pathlib import Path
import sys
root=Path(sys.argv[1])
values=dict(line.split('=',1) for line in (root/'postgres.env').read_text().splitlines())
host,port=sys.argv[2].rsplit(':',1)
path=root/'postgres.dsn'
path.write_text('host='+host+' port='+port+' user='+values['POSTGRES_USER']+' password='+values['POSTGRES_PASSWORD']+' dbname='+values['POSTGRES_DB']+' sslmode=disable')
path.chmod(0o600)
PY
export RUNNER_POSTGRES_TEST_DSN_FILE="$private_dir/postgres.dsn"
ready=0
for attempt in $(seq 1 100); do
    if docker exec "$postgres_container" pg_isready --host 127.0.0.1 --quiet --username runner_ha_test --dbname runner_ha_test; then
        ready=1
        break
    fi
    sleep 0.2
done
[ "$ready" = 1 ]
. "$(dirname "$0")/prepare-ha-postgres.sh"
export HA_RUNNER_BINARY="$private_dir/runner"
GOWORK=off go build -o "$HA_RUNNER_BINARY" ./cmd/runner
GOWORK=off go test -race -tags=integration ./cmd/runner -run '^TestNative' -count=1 -timeout=90s -v
GOWORK=off go test -race -tags=integration ./internal/rollout -run '^TestPostgres' -count=1 -timeout=15m -v
ha_tests="$(GOWORK=off go test -tags=integration ./internal/rolloutagent -list '^TestHA')"
case "$ha_tests" in
    *TestHA*) ;;
    *) printf 'HA process qualification tests are missing\n' >&2; exit 1 ;;
esac
GOWORK=off go test -race -tags=integration ./internal/rolloutagent -run '^TestHA' -count=1 -timeout=230s -v
