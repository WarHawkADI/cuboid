-- Static aggregate-store tables. Mirror and rollup-cell tables are generated
-- from config/metrics.yaml and the main database's catalog (internal/mirror).
-- Idempotent: runs at every start as cuboid_writer.

-- One row per source database: the platform's, and each store's (ADR-012).
-- A source's position commits in the same transaction as the data it brings.
DROP TABLE IF EXISTS cdc_state;
CREATE TABLE IF NOT EXISTS cdc_sources (
  source              text PRIMARY KEY,  -- the source database
  tenant_id           int UNIQUE,        -- the store whose database it is; NULL for the platform's
  slot                text,
  applied_lsn         pg_lsn NOT NULL DEFAULT '0/0',
  applied_commit_time timestamptz,
  heartbeat_time      timestamptz,       -- send time of the latest heartbeat applied and flushed
  backfilled_at       timestamptz,       -- when the source was last loaded from a snapshot
  flushes             bigint NOT NULL DEFAULT 0
);

-- Dimension values, dictionary-encoded. Id 0 is reserved for NULL.
CREATE TABLE IF NOT EXISTS dict (
  id    bigserial PRIMARY KEY,
  dim   text NOT NULL,
  value text NOT NULL,
  UNIQUE (dim, value)
);

CREATE TABLE IF NOT EXISTS rollup_registry (
  name       text PRIMARY KEY,
  id         smallint UNIQUE NOT NULL,
  source     text NOT NULL,
  ready      boolean NOT NULL DEFAULT false,
  definition text NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS data_versions (
  tenant_id  int PRIMARY KEY,
  version    bigint NOT NULL,
  changed_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS api_keys (
  prefix     text PRIMARY KEY,
  hash       bytea NOT NULL,
  scope      text NOT NULL CHECK (scope IN ('store', 'company', 'operator')),
  scope_id   int,
  created_at timestamptz NOT NULL DEFAULT now(),
  revoked_at timestamptz
);

CREATE TABLE IF NOT EXISTS audit_log (
  at          timestamptz NOT NULL,
  scope       text NOT NULL,
  scope_id    int,
  tenant_ids  int[] NOT NULL,
  key_prefix  text NOT NULL,
  endpoint    text NOT NULL,
  query       text NOT NULL,
  status      int NOT NULL,
  result_rows int NOT NULL,
  cache       text NOT NULL,
  duration_ms real NOT NULL
);
CREATE INDEX IF NOT EXISTS audit_log_at ON audit_log (at);

CREATE TABLE IF NOT EXISTS security_events (
  id         bigserial PRIMARY KEY,
  at         timestamptz NOT NULL DEFAULT now(),
  kind       text NOT NULL,
  severity   text NOT NULL,
  tenant_id  int,
  key_prefix text,
  detail     text NOT NULL
);

CREATE TABLE IF NOT EXISTS verifications (
  id         bigserial PRIMARY KEY,
  at         timestamptz NOT NULL DEFAULT now(),
  mode       text NOT NULL,
  tenant_ids int[] NOT NULL,
  query      text NOT NULL,
  exact      boolean NOT NULL,
  fence_lsn  pg_lsn,
  cuboid_ms  real,
  direct_ms  real,
  r_minus    int,
  r_plus     int,
  retries    int
);

-- Sums exact integers without passing through numeric, which sum(bigint) does:
-- 100,000 groups take half the time. int8pl is checked: an overflow is an error,
-- never a wrong number.
CREATE OR REPLACE AGGREGATE exact_sum(bigint) (sfunc = int8pl, stype = bigint, initcond = '0');

GRANT SELECT ON cdc_sources, dict, rollup_registry, data_versions, api_keys TO cuboid_reader;
