#!/usr/bin/env bash
# Development path without Docker: two local Postgres clusters with the same
# settings and ports as docker-compose.yml. Usage: tools/devdb.sh up|down|reset
# PGBIN points at a PostgreSQL 16 bin directory; DEVDB_DATA holds the clusters.
set -euo pipefail
PGBIN="${PGBIN:-$HOME/sdk/pgsql16/bin}"
DATA="${DEVDB_DATA:-$HOME/sdk/pgdata}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MAIN_OPTS="-c port=5433 -c wal_level=logical -c max_replication_slots=10 -c max_wal_senders=10 -c max_slot_wal_keep_size=4GB -c shared_preload_libraries=pg_stat_statements -c track_io_timing=on -c timezone=Asia/Kolkata -c shared_buffers=1GB -c effective_cache_size=3GB -c work_mem=32MB -c maintenance_work_mem=512MB -c max_connections=300 -c logical_decoding_work_mem=256MB -c max_wal_size=4GB"
AGG_OPTS="-c port=5434 -c timezone=Asia/Kolkata -c shared_buffers=1GB -c effective_cache_size=3GB -c work_mem=64MB -c maintenance_work_mem=1GB -c max_connections=200 -c max_wal_size=4GB -c max_parallel_workers_per_gather=2"

start() { "$PGBIN/pg_ctl" -D "$DATA/$1" -w -t 60 -l "$DATA/$1.log" -o "$2" start > "$DATA/$1.ctl.log" 2>&1; }  # the server must not inherit our stdout

case "${1:-up}" in
up)
  fresh=0
  if [ ! -d "$DATA/main" ]; then
    mkdir -p "$DATA"
    "$PGBIN/initdb" -D "$DATA/main" -U postgres --auth=trust -E UTF8 --locale=C >/dev/null
    "$PGBIN/initdb" -D "$DATA/agg" -U postgres --auth=trust -E UTF8 --locale=C >/dev/null
    fresh=1
  fi
  start main "$MAIN_OPTS"
  start agg "$AGG_OPTS"
  if [ "$fresh" = 1 ]; then
    "$PGBIN/createdb" -h localhost -p 5433 -U postgres shop
    "$PGBIN/createdb" -h localhost -p 5434 -U postgres agg
    "$PGBIN/psql" -q -h localhost -p 5433 -U postgres -d shop -v ON_ERROR_STOP=1 -f "$ROOT/migrations/main/001_schema.sql"
    "$PGBIN/psql" -q -h localhost -p 5434 -U postgres -d agg -v ON_ERROR_STOP=1 -f "$ROOT/migrations/agg/000_roles.sql"
  fi
  ;;
down)
  "$PGBIN/pg_ctl" -D "$DATA/main" -m fast stop || true
  "$PGBIN/pg_ctl" -D "$DATA/agg" -m fast stop || true
  ;;
reset)
  "$0" down || true
  rm -rf "$DATA/main" "$DATA/agg"
  "$0" up
  ;;
*) echo "usage: $0 up|down|reset"; exit 2 ;;
esac
