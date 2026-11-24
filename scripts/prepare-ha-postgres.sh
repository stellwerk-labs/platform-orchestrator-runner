#!/bin/sh
# Sourced only after the invocation-owned primary is healthy.
postgres_network="$(docker network create --label com.stellwerk.test=rollout-ha "stellwerk-ha-$(basename "$private_dir")")"
docker network connect --alias ha-primary "$postgres_network" "$postgres_container"
python3 - "$private_dir/postgres.env" <<'PYSQL' | docker exec -i "$postgres_container" psql -X -v ON_ERROR_STOP=1 -U runner_ha_test -d runner_ha_test
from pathlib import Path
import sys
values=dict(line.split('=',1) for line in Path(sys.argv[1]).read_text().splitlines())
print("CREATE ROLE ha_replica LOGIN REPLICATION PASSWORD '"+values['POSTGRES_PASSWORD']+"';")
PYSQL
docker exec "$postgres_container" sh -ec 'printf "host replication ha_replica all scram-sha-256\n" >> "$PGDATA/pg_hba.conf"'
docker exec "$postgres_container" psql -X -v ON_ERROR_STOP=1 -U runner_ha_test -d runner_ha_test -c 'SELECT pg_reload_conf()' >/dev/null
cat > "$private_dir/standby-bootstrap.sh" <<'BOOTSTRAP'
#!/bin/sh
set -eu
pg_basebackup -h ha-primary -U ha_replica -D "$PGDATA" -R -X stream --checkpoint=fast -d "application_name=sync_peer"
exec postgres
BOOTSTRAP
chmod 0444 "$private_dir/standby-bootstrap.sh"
postgres_standby="$(docker run --detach --pull=never --label com.stellwerk.test=rollout-ha \
    --network "$postgres_network" --env-file "$private_dir/postgres.env" -p 127.0.0.1::5432 \
    --mount "type=bind,source=$private_dir/standby-bootstrap.sh,target=/standby-bootstrap.sh,readonly" \
    --entrypoint /bin/sh "$postgres_image" -ec '
        chown postgres:postgres "$PGDATA"
        chmod 0700 "$PGDATA"
        export PGPASSWORD="$POSTGRES_PASSWORD"
        exec gosu postgres /bin/sh /standby-bootstrap.sh
    ')"
ready=0
for attempt in $(seq 1 100); do
    streaming="$(docker exec "$postgres_container" psql -X -tA -U runner_ha_test -d runner_ha_test \
        -c "SELECT count(*) FROM pg_stat_replication WHERE application_name='sync_peer' AND state='streaming'")"
    if [ "$streaming" = 1 ]; then
        ready=1
        break
    fi
    sleep 0.2
done
[ "$ready" = 1 ]
docker exec "$postgres_container" psql -X -v ON_ERROR_STOP=1 -U runner_ha_test -d runner_ha_test \
    -c "ALTER SYSTEM SET synchronous_standby_names = 'FIRST 1 (sync_peer)'" >/dev/null
docker exec "$postgres_container" psql -X -v ON_ERROR_STOP=1 -U runner_ha_test -d runner_ha_test -c 'SELECT pg_reload_conf()' >/dev/null
ready=0
for attempt in $(seq 1 100); do
    synchronized="$(docker exec "$postgres_container" psql -X -tA -U runner_ha_test -d runner_ha_test \
        -c "SELECT count(*) FROM pg_stat_replication WHERE application_name='sync_peer' AND sync_state='sync'")"
    if [ "$synchronized" = 1 ]; then
        ready=1
        break
    fi
    sleep 0.2
done
[ "$ready" = 1 ]
postgres_standby_address="$(docker port "$postgres_standby" 5432/tcp)"
python3 - "$private_dir" "$postgres_standby_address" <<'PYDSN'
from pathlib import Path
import sys
root=Path(sys.argv[1])
values=dict(line.split('=',1) for line in (root/'postgres.env').read_text().splitlines())
host,port=sys.argv[2].rsplit(':',1)
path=root/'postgres-standby.dsn'
path.write_text('host='+host+' port='+port+' user='+values['POSTGRES_USER']+' password='+values['POSTGRES_PASSWORD']+' dbname='+values['POSTGRES_DB']+' sslmode=disable')
path.chmod(0o600)
PYDSN
export RUNNER_POSTGRES_STANDBY_TEST_DSN_FILE="$private_dir/postgres-standby.dsn"
