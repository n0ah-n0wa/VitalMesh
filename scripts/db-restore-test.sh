#!/bin/sh
# A realistic restore test: point-in-time recovery of the VitalMesh schema,
# performed against a real PostgreSQL 16 server, verified, and timed
# (SPECIFICATIONS.md sections 79 and 80; docs/DISASTER_RECOVERY.md).
#
# WHAT THIS PROVES, AND WHAT IT DOES NOT
#
# Production runs PostgreSQL 16 on RDS. RDS's automated backups are a
# managed wrapper around exactly the machinery this test drives directly: a
# base backup, continuously archived write-ahead log, and recovery to a
# chosen instant with recovery_target_time. Running it proves
#
#   - the schema, its constraints and its triggers survive a base backup
#     and a WAL replay;
#   - a change committed after the recovery target is genuinely absent
#     afterwards, which is the property a point-in-time restore exists for;
#   - everything committed before the target is present, to the row;
#   - the verification queries in docs/DISASTER_RECOVERY.md actually
#     distinguish a good restore from a bad one;
#   - the application can read the restored database.
#
# It proves nothing about RDS itself: not the restore API, not how long AWS
# takes to provision an instance, not the IAM or parameter-group path, and
# not the durability of S3-backed backups. The timings printed at the end
# are PostgreSQL's own recovery times for a small database on one machine
# and are not an RTO for production. docs/DISASTER_RECOVERY.md states which
# figures are measured and which are assumed.
#
#   sh scripts/db-restore-test.sh          run, then clean up
#   sh scripts/db-restore-test.sh --keep   leave the containers for a look
#
# Needs Docker and the api-gateway image (make docker-build): the schema is
# created by the real migrator, so the test runs against the schema the
# application actually uses.
set -eu

PREFIX="vitalmesh-dr"
NET="$PREFIX-net"
PRIMARY="$PREFIX-primary"
RESTORED="$PREFIX-restored"
V_DATA="$PREFIX-data"
V_WAL="$PREFIX-wal"
V_BASE="$PREFIX-base"
PG_IMAGE="postgres:16-alpine"
GATEWAY_IMAGE="${GATEWAY_IMAGE:-vitalmesh/api-gateway:dev}"
PGUSER="vitalmesh"
PGPASS="vitalmesh-restore-test-not-a-secret"
PGDB="vitalmesh"

KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

MSYS_NO_PATHCONV=1
export MSYS_NO_PATHCONV

failures=0
ok()   { printf '  ok    %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; failures=$((failures + 1)); }
note() { printf '        %s\n' "$1"; }
step() { printf '\n%s\n' "$1"; }

now_ms() { date +%s%3N; }
since() { s="$1"; e="$(now_ms)"; d=$((e - s)); printf '%d.%01ds' "$((d / 1000))" "$(((d % 1000) / 100))"; }

cleanup() {
    if [ "$KEEP" -eq 1 ]; then
        printf '\nkept: containers %s %s and volumes %s %s %s\n' "$PRIMARY" "$RESTORED" "$V_DATA" "$V_WAL" "$V_BASE"
        return
    fi
    docker rm -f "$PRIMARY" "$RESTORED" >/dev/null 2>&1 || true
    docker volume rm -f "$V_DATA" "$V_WAL" "$V_BASE" >/dev/null 2>&1 || true
    docker network rm "$NET" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

# One query against a named container, failing on the first error. The
# result keeps its internal spaces: a timestamp has one, and stripping every
# space once turned a recovery target into a value PostgreSQL refused.
sql() { docker exec "$1" psql -U "$PGUSER" -d "$PGDB" -v ON_ERROR_STOP=1 -qtAc "$2"; }
val() { sql "$1" "$2" | head -1 | awk '{ gsub(/\r/, ""); $1 = $1; print }'; }

# pg_isready is not enough, twice over, and both cost this test a false
# failure before they were understood:
#
#   - the official entrypoint starts a temporary server to run
#     initialisation and then shuts it down before starting the real one.
#     pg_isready succeeds against that temporary server, so the next query
#     lands on "the database system is shutting down" and comes back empty;
#   - a server replaying WAL accepts connections before it is promoted, so
#     readiness says yes while the database is still read-only.
#
# So: wait for a real query to succeed twice in a row, which no shutting-down
# server does, and wait for promotion separately where it matters.
wait_serving() {
    i=0
    consecutive=0
    while [ "$i" -lt "$2" ]; do
        if docker exec "$1" psql -U "$PGUSER" -d "$PGDB" -qtAc 'SELECT 1' >/dev/null 2>&1; then
            consecutive=$((consecutive + 1))
            [ "$consecutive" -ge 2 ] && return 0
        else
            consecutive=0
        fi
        sleep 1
        i=$((i + 1))
    done
    return 1
}

# Recovery finished and the server was promoted, so it is writable and the
# restore can be judged.
wait_promoted() {
    i=0
    while [ "$i" -lt "$2" ]; do
        if [ "$(val "$1" 'SELECT pg_is_in_recovery()' 2>/dev/null)" = "f" ]; then return 0; fi
        sleep 1
        i=$((i + 1))
    done
    return 1
}

seed_patient() {
    sql "$PRIMARY" "INSERT INTO patients (external_reference, date_of_birth, sex) VALUES ('$1', DATE '1985-04-12', 'FEMALE')" >/dev/null
    sql "$PRIMARY" "INSERT INTO measurements (patient_id, type, value, unit, recorded_at, source) SELECT p.id, 'HEART_RATE', 60 + (g % 40), 'bpm', now() - (g || ' minutes')::interval, 'restore-test' FROM patients p, generate_series(1, $2) g WHERE p.external_reference = '$1'" >/dev/null
}

# ---------------------------------------------------------------- preflight
step "Preflight"
command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 2; }
ok "docker is present"
docker image inspect "$GATEWAY_IMAGE" >/dev/null 2>&1 || { echo "$GATEWAY_IMAGE is missing; run: make docker-build" >&2; exit 2; }
ok "the api-gateway image is available, so the real migrator creates the schema"

docker rm -f "$PRIMARY" "$RESTORED" >/dev/null 2>&1 || true
docker volume rm -f "$V_DATA" "$V_WAL" "$V_BASE" >/dev/null 2>&1 || true
docker network rm "$NET" >/dev/null 2>&1 || true
docker network create "$NET" >/dev/null
for v in "$V_DATA" "$V_WAL" "$V_BASE"; do docker volume create "$v" >/dev/null; done
docker run --rm -v "$V_WAL:/wal" -v "$V_BASE:/base" "$PG_IMAGE" sh -c 'chown 70:70 /wal /base && chmod 700 /wal /base'
ok "network and volumes created, archive and backup owned by postgres"

# ------------------------------------------------------- the primary server
step "A running server with continuous archiving"
docker run -d --name "$PRIMARY" --network "$NET" \
    -e POSTGRES_USER="$PGUSER" -e POSTGRES_PASSWORD="$PGPASS" -e POSTGRES_DB="$PGDB" \
    -v "$V_DATA:/var/lib/postgresql/data" -v "$V_WAL:/wal" -v "$V_BASE:/base" \
    "$PG_IMAGE" \
    -c wal_level=replica \
    -c archive_mode=on \
    -c "archive_command=test ! -f /wal/%f && cp %p /wal/%f" \
    -c archive_timeout=15 \
    -c max_wal_senders=4 >/dev/null
if wait_serving "$PRIMARY" 120; then
    ok "PostgreSQL $(val "$PRIMARY" 'SHOW server_version') accepting connections"
else
    fail "the primary never became ready"
    docker logs "$PRIMARY" 2>&1 | tail -20
    exit 1
fi
if [ "$(val "$PRIMARY" 'SHOW archive_mode')" = "on" ]; then ok "archive_mode is on"; else fail "archive_mode is not on"; fi

# ------------------------------------------------------------------ schema
step "The real schema, from the real migrator"
if docker run --rm --network "$NET" \
    -e DATABASE_URL="postgres://$PGUSER:$PGPASS@$PRIMARY:5432/$PGDB?sslmode=disable" \
    "$GATEWAY_IMAGE" migrate up >/dev/null 2>&1; then
    ok "migrations applied"
else
    fail "migrate up failed"
    exit 1
fi
SCHEMA_VERSION="$(val "$PRIMARY" 'SELECT version FROM schema_migrations')"
note "schema version $SCHEMA_VERSION"

step "A known dataset, before the backup"
seed_patient "dr-base-1" 20
seed_patient "dr-base-2" 20
# An operator and a finished job, so the job-state check after the restore
# has something to be right about rather than an empty table.
sql "$PRIMARY" "INSERT INTO users (email, password_hash, role) VALUES ('dr-test@vitalmesh.local', 'not-a-real-hash', 'OPERATOR')" >/dev/null
sql "$PRIMARY" "INSERT INTO processing_jobs (patient_id, parameters, algorithm_version, created_by, status, started_at, completed_at) SELECT p.id, '{}'::jsonb, '1.0.0', u.id, 'COMPLETED', now(), now() FROM patients p, users u WHERE p.external_reference = 'dr-base-1' AND u.email = 'dr-test@vitalmesh.local'" >/dev/null
ok "2 patients, 40 readings, 1 operator and 1 completed job committed before the base backup"

# ------------------------------------------------------------- base backup
step "Base backup"
t0="$(now_ms)"
# Over the local socket, where initdb's pg_hba grants replication to the
# superuser, so no host-based replication rule is needed.
if docker exec "$PRIMARY" pg_basebackup -U "$PGUSER" -D /base -Fp -Xstream -c fast >/dev/null 2>&1; then
    BACKUP_TIME="$(since "$t0")"
    ok "pg_basebackup completed in $BACKUP_TIME"
else
    fail "pg_basebackup failed"
    docker exec "$PRIMARY" pg_basebackup -U "$PGUSER" -D /base -Fp -Xstream 2>&1 | tail -5
    exit 1
fi
note "backup size $(docker exec "$PRIMARY" du -sh /base | cut -f1)"

# ---------------------------------------------- data after the backup
step "More data, then the recovery target, then the change to undo"
seed_patient "dr-pre-1" 20
seed_patient "dr-pre-2" 20
ok "2 more patients and 40 more readings committed after the backup"

PATIENTS_EXPECTED="$(val "$PRIMARY" 'SELECT count(*) FROM patients')"
MEASUREMENTS_EXPECTED="$(val "$PRIMARY" 'SELECT count(*) FROM measurements')"
RECOVERY_TARGET="$(val "$PRIMARY" 'SELECT now()')"
note "recovery target $RECOVERY_TARGET"
note "at the target: $PATIENTS_EXPECTED patients, $MEASUREMENTS_EXPECTED readings"

# A gap, so the target instant is unambiguous, then the damage.
sleep 2
seed_patient "dr-marker-AFTER-TARGET" 20
ok "the change to be undone is committed after the target: 1 patient, 20 readings"
note "at the moment of loss: $(val "$PRIMARY" 'SELECT count(*) FROM patients') patients, $(val "$PRIMARY" 'SELECT count(*) FROM measurements') readings"

# The WAL holding the target has to reach the archive before the server dies.
sql "$PRIMARY" 'SELECT pg_switch_wal()' >/dev/null
archived=""
i=0
while [ "$i" -lt 60 ]; do
    archived="$(val "$PRIMARY" "SELECT coalesce(last_archived_wal, '') FROM pg_stat_archiver")"
    failed="$(val "$PRIMARY" 'SELECT failed_count FROM pg_stat_archiver')"
    if [ "${failed:-0}" != "0" ]; then fail "the archiver reported $failed failures"; break; fi
    if [ -n "$archived" ]; then break; fi
    sleep 1
    i=$((i + 1))
done
if [ -n "$archived" ]; then ok "write-ahead log is reaching the archive, last $archived"; else fail "nothing was archived"; fi
note "$(docker exec "$PRIMARY" sh -c 'ls /wal | wc -l' | tr -d ' ') segments in the archive"

# -------------------------------------------------------------------- loss
step "Total loss of the server and its storage"
docker rm -f "$PRIMARY" >/dev/null
docker volume rm -f "$V_DATA" >/dev/null
ok "the container and its data volume are gone; only the base backup and the archive remain"

# ----------------------------------------------------------------- restore
step "Point-in-time recovery to the target"
t0="$(now_ms)"
# recovery.signal puts the server into archive recovery (PostgreSQL 12 and
# later); the settings sit in postgresql.auto.conf beside it.
docker run --rm -v "$V_BASE:/base" -e TARGET="$RECOVERY_TARGET" "$PG_IMAGE" sh -c '
set -e
rm -f /base/postmaster.pid
{
  echo "restore_command = '"'"'cp /wal/%f %p'"'"'"
  echo "recovery_target_time = '"'"'$TARGET'"'"'"
  echo "recovery_target_action = '"'"'promote'"'"'"
} >> /base/postgresql.auto.conf
touch /base/recovery.signal
chown -R 70:70 /base
chmod 700 /base
'
ok "recovery target and restore_command written into the restored directory"

docker run -d --name "$RESTORED" --network "$NET" \
    -e POSTGRES_USER="$PGUSER" -e POSTGRES_PASSWORD="$PGPASS" -e POSTGRES_DB="$PGDB" \
    -v "$V_BASE:/var/lib/postgresql/data" -v "$V_WAL:/wal" \
    "$PG_IMAGE" -c archive_mode=off >/dev/null
if wait_serving "$RESTORED" 180 && wait_promoted "$RESTORED" 120; then
    RESTORE_TIME="$(since "$t0")"
    ok "the restored server replayed to the target and was promoted in $RESTORE_TIME"
else
    fail "the restored server never finished recovery"
    docker logs "$RESTORED" 2>&1 | tail -30
    exit 1
fi
note "$(docker logs "$RESTORED" 2>&1 | grep -c 'restored log file' || true) segments replayed from the archive"
if [ "$(val "$RESTORED" 'SELECT pg_is_in_recovery()')" = "f" ]; then ok "the server was promoted and is writable"; else fail "the server is still in recovery"; fi

# ------------------------------------------------------------ verification
step "Verifying the restore (docs/DISASTER_RECOVERY.md, Verifying a restore)"
v="$(val "$RESTORED" 'SELECT version FROM schema_migrations')"
if [ "$v" = "$SCHEMA_VERSION" ]; then ok "schema version is $v, as before the loss"; else fail "schema version is $v, expected $SCHEMA_VERSION"; fi
if [ "$(val "$RESTORED" 'SELECT dirty FROM schema_migrations')" = "f" ]; then ok "the schema is not dirty"; else fail "the schema is marked dirty"; fi

marker="$(val "$RESTORED" "SELECT count(*) FROM patients WHERE external_reference LIKE 'dr-marker%'")"
if [ "$marker" = "0" ]; then
    ok "the change made after the target is absent: the point of a point-in-time restore"
else
    fail "$marker marker row(s) survived; recovery did not stop at the target"
fi

p="$(val "$RESTORED" 'SELECT count(*) FROM patients')"
m="$(val "$RESTORED" 'SELECT count(*) FROM measurements')"
if [ "$p" = "$PATIENTS_EXPECTED" ]; then ok "$p patients, exactly what was committed before the target"; else fail "$p patients, expected $PATIENTS_EXPECTED"; fi
if [ "$m" = "$MEASUREMENTS_EXPECTED" ]; then ok "$m readings, exactly what was committed before the target"; else fail "$m readings, expected $MEASUREMENTS_EXPECTED"; fi

orphans="$(val "$RESTORED" 'SELECT count(*) FROM measurements m LEFT JOIN patients p ON p.id = m.patient_id WHERE p.id IS NULL')"
if [ "$orphans" = "0" ]; then ok "no reading without its patient: referential integrity intact"; else fail "$orphans orphaned readings"; fi

stuck="$(val "$RESTORED" "SELECT count(*) FROM processing_jobs WHERE status IN ('PENDING','PROCESSING')")"
if [ "$stuck" = "0" ]; then ok "no processing job left in a non-terminal state"; else fail "$stuck non-terminal job(s)"; fi
done_jobs="$(val "$RESTORED" "SELECT count(*) FROM processing_jobs WHERE status = 'COMPLETED'")"
if [ "$done_jobs" = "1" ]; then ok "the completed job survived"; else fail "$done_jobs completed job(s), expected 1"; fi
accounts="$(val "$RESTORED" "SELECT count(*) FROM users WHERE email = 'dr-test@vitalmesh.local'")"
if [ "$accounts" = "1" ]; then ok "the operator account survived"; else fail "the operator account did not survive"; fi

# A restore that silently dropped the constraints would pass every count above.
if docker exec "$RESTORED" psql -U "$PGUSER" -d "$PGDB" -qtAc "INSERT INTO measurements (patient_id, type, value, unit, recorded_at, source) SELECT id, 'HEART_RATE', 9999, 'bpm', now(), 'restore-test-invalid' FROM patients LIMIT 1" >/dev/null 2>&1; then
    fail "an out-of-range reading was accepted: the validation trigger did not survive"
else
    ok "an out-of-range reading is still rejected: constraints and triggers survived"
fi
if docker exec "$RESTORED" psql -U "$PGUSER" -d "$PGDB" -qtAc "INSERT INTO patients (external_reference, date_of_birth, sex) VALUES ('dr-base-1', DATE '1990-01-01', 'MALE')" >/dev/null 2>&1; then
    fail "a duplicate external reference was accepted: the unique index did not survive"
else
    ok "a duplicate external reference is still rejected: indexes survived"
fi

if docker run --rm --network "$NET" \
    -e DATABASE_URL="postgres://$PGUSER:$PGPASS@$RESTORED:5432/$PGDB?sslmode=disable" \
    "$GATEWAY_IMAGE" migrate version 2>&1 | grep -q "version=$SCHEMA_VERSION dirty=false"; then
    ok "the application reads the restored database and agrees on the schema version"
else
    fail "the application could not read the restored database"
fi

# --------------------------------------------------- logical backup path
step "The second procedure: a logical dump and restore"
t0="$(now_ms)"
docker exec "$RESTORED" pg_dump -U "$PGUSER" -d "$PGDB" -Fc -f /tmp/dr.dump
DUMP_TIME="$(since "$t0")"
ok "pg_dump (custom format) completed in $DUMP_TIME"
note "dump size $(docker exec "$RESTORED" du -h /tmp/dr.dump | cut -f1)"
sql "$RESTORED" 'CREATE DATABASE restore_check' >/dev/null
t0="$(now_ms)"
RESTORE_DUMP_TIME="n/a"
if docker exec "$RESTORED" pg_restore -U "$PGUSER" -d restore_check --no-owner /tmp/dr.dump >/dev/null 2>&1; then
    RESTORE_DUMP_TIME="$(since "$t0")"
    ok "pg_restore into a fresh database completed in $RESTORE_DUMP_TIME"
else
    fail "pg_restore failed"
fi
pc="$(docker exec "$RESTORED" psql -U "$PGUSER" -d restore_check -qtAc 'SELECT count(*) FROM patients' | head -1 | awk '{ gsub(/\r/, ""); $1 = $1; print }')"
mc="$(docker exec "$RESTORED" psql -U "$PGUSER" -d restore_check -qtAc 'SELECT count(*) FROM measurements' | head -1 | awk '{ gsub(/\r/, ""); $1 = $1; print }')"
if [ "$pc" = "$PATIENTS_EXPECTED" ] && [ "$mc" = "$MEASUREMENTS_EXPECTED" ]; then
    ok "the logical copy holds the same $pc patients and $mc readings"
else
    fail "the logical copy holds $pc patients and $mc readings"
fi

# -------------------------------------------------------------------- done
step "Measured"
note "base backup            $BACKUP_TIME"
note "point-in-time recovery $RESTORE_TIME to promotion (writable)"
note "logical dump           $DUMP_TIME"
note "logical restore        $RESTORE_DUMP_TIME"
note "dataset                $PATIENTS_EXPECTED patients, $MEASUREMENTS_EXPECTED readings, schema $SCHEMA_VERSION"
note "PostgreSQL's own times for a small database on one machine; not an RTO"
note "for RDS (see docs/DISASTER_RECOVERY.md)"

printf '\n'
if [ "$failures" -ne 0 ]; then
    printf 'restore test: %d check(s) failed\n' "$failures"
    exit 1
fi
printf 'restore test: point-in-time recovery verified\n'
