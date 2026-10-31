-- MAIN database: the store platform's own database (fallback schema, Section 7.2).
-- Runs once, as the superuser, when the maindb container initialises.
-- Passwords are documented development defaults for a local, offline stack.

ALTER DATABASE shop SET timezone TO 'Asia/Kolkata';
SET timezone TO 'Asia/Kolkata';

CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

-- Roles: each program gets only what it needs.
CREATE ROLE app            LOGIN PASSWORD 'app';                       -- the store app (storesim)
CREATE ROLE cuboid_cdc     LOGIN REPLICATION PASSWORD 'cuboid_cdc';    -- logical replication + snapshot backfill
CREATE ROLE cuboid_verify  LOGIN PASSWORD 'cuboid_verify' CONNECTION LIMIT 3;  -- budgeted direct checks
CREATE ROLE naive_dash     LOGIN PASSWORD 'naive_dash';                -- the "direct" baseline only
ALTER ROLE cuboid_verify SET statement_timeout = '5s';
ALTER ROLE cuboid_verify SET default_transaction_read_only = on;

CREATE TABLE companies (
  company_id int PRIMARY KEY,
  name       text NOT NULL
);

CREATE TABLE tenants (                     -- a tenant is a store
  tenant_id  int PRIMARY KEY,
  company_id int NOT NULL REFERENCES companies,
  name       text NOT NULL
);

CREATE TABLE customers (
  tenant_id   int    NOT NULL,
  customer_id bigint NOT NULL,
  full_name   text,                        -- PII: never mirrored
  email       text,                        -- PII
  phone       text,                        -- PII
  city        text,
  region      text,
  created_at  timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, customer_id)
);

CREATE TABLE products (
  tenant_id   int    NOT NULL,
  product_id  bigint NOT NULL,
  name        text   NOT NULL,
  category    text   NOT NULL,
  price_paise bigint NOT NULL CHECK (price_paise >= 0),
  PRIMARY KEY (tenant_id, product_id)
);

CREATE TABLE orders (
  tenant_id      int    NOT NULL,
  order_id       bigint NOT NULL,
  customer_id    bigint NOT NULL,
  product_id     bigint NOT NULL,
  quantity       int    NOT NULL CHECK (quantity > 0),
  amount_paise   bigint NOT NULL CHECK (amount_paise >= 0),
  status         text   NOT NULL CHECK (status IN ('placed','shipped','delivered','cancelled','returned')),
  payment_method text   NOT NULL,
  channel        text   NOT NULL,
  created_at     timestamptz NOT NULL,
  updated_at     timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, order_id)
);

-- W9: Mitigata's own shape of data, served by the same engine through config.
CREATE TABLE soc_alerts (
  tenant_id      int    NOT NULL,            -- the client organisation
  alert_id       bigint NOT NULL,
  severity       text   NOT NULL CHECK (severity IN ('critical','high','medium','low','info')),
  source_product text   NOT NULL,
  tactic         text   NOT NULL,            -- MITRE ATT&CK tactic
  status         text   NOT NULL CHECK (status IN ('new','triaged','investigating','resolved','false_positive')),
  asset_id       bigint NOT NULL,
  risk_score     int    NOT NULL CHECK (risk_score BETWEEN 0 AND 100),
  analyst_note   text,                       -- free text may hold PII: never mirrored
  created_at     timestamptz NOT NULL,
  updated_at     timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, alert_id)
);

-- Retractions need the full old row on UPDATE and DELETE (ADR-003).
ALTER TABLE orders     REPLICA IDENTITY FULL;
ALTER TABLE soc_alerts REPLICA IDENTITY FULL;
ALTER TABLE products   REPLICA IDENTITY FULL;   -- rare updates; lets a recategorisation be detected exactly
ALTER TABLE customers  REPLICA IDENTITY FULL;
ALTER TABLE tenants    REPLICA IDENTITY FULL;

CREATE PUBLICATION cuboid_pub FOR TABLE orders, products, customers, tenants, companies, soc_alerts
  WITH (publish = 'insert, update, delete, truncate');

GRANT SELECT, INSERT, UPDATE, DELETE ON orders, customers, products, soc_alerts TO app;
GRANT SELECT ON tenants, companies TO app;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO cuboid_cdc, cuboid_verify, naive_dash;
GRANT EXECUTE ON FUNCTION pg_logical_emit_message(boolean, text, text) TO cuboid_cdc;
GRANT pg_read_all_stats TO cuboid_verify;
