-- MAIN database server: the store platform's own databases (fallback schema, Section 7.2).
-- Runs once, as the superuser, when the maindb container initialises.
-- Passwords are documented development defaults for a local, offline stack.
--
-- Each store's orders, products, customers and alerts sit in their own database
-- (ADR-012). This database, shop, is the platform's: the registry of stores and
-- companies. store_template is the schema every store database is created from:
-- the seed runs CREATE DATABASE store_<id> TEMPLATE store_template, then pins
-- every table to that one store with a CHECK constraint.

ALTER DATABASE shop SET timezone TO 'Asia/Kolkata';
SET timezone TO 'Asia/Kolkata';

CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

-- Roles are shared by every database on the server: each program gets only what it needs.
CREATE ROLE app            LOGIN PASSWORD 'app';                       -- the store app (storesim)
CREATE ROLE cuboid_cdc     LOGIN REPLICATION PASSWORD 'cuboid_cdc';    -- logical replication + snapshot backfill
CREATE ROLE cuboid_verify  LOGIN PASSWORD 'cuboid_verify' CONNECTION LIMIT 8;  -- budgeted direct checks, one store at a time
CREATE ROLE naive_dash     LOGIN PASSWORD 'naive_dash';                -- the "direct" baseline only
ALTER ROLE cuboid_verify SET statement_timeout = '5s';
ALTER ROLE cuboid_verify SET default_transaction_read_only = on;

CREATE TABLE companies (
  company_id int PRIMARY KEY,
  name       text NOT NULL
);

CREATE TABLE tenants (                     -- a tenant is a store; its data is in database store_<tenant_id>
  tenant_id  int PRIMARY KEY,
  company_id int NOT NULL REFERENCES companies,
  name       text NOT NULL
);

-- A bulk data load sets this; Cuboid and the store app wait, and Cuboid then
-- takes one consistent snapshot of each store instead of streaming millions of rows.
CREATE TABLE seed_status (id int PRIMARY KEY DEFAULT 1 CHECK (id = 1), loading boolean NOT NULL DEFAULT false);
INSERT INTO seed_status DEFAULT VALUES;

ALTER TABLE tenants REPLICA IDENTITY FULL;

CREATE PUBLICATION cuboid_pub FOR TABLE tenants, companies
  WITH (publish = 'insert, update, delete, truncate');

GRANT SELECT ON tenants, companies, seed_status TO app;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO cuboid_cdc, cuboid_verify, naive_dash;
GRANT EXECUTE ON FUNCTION pg_logical_emit_message(boolean, text, text) TO cuboid_cdc;
GRANT pg_read_all_stats TO cuboid_verify;

-- The schema of one store's database.
CREATE DATABASE store_template IS_TEMPLATE true;
\connect store_template
SET timezone TO 'Asia/Kolkata';

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

CREATE PUBLICATION cuboid_pub FOR TABLE orders, products, customers, soc_alerts
  WITH (publish = 'insert, update, delete, truncate');

GRANT SELECT, INSERT, UPDATE, DELETE ON orders, customers, products, soc_alerts TO app;
GRANT TRUNCATE ON orders TO app;   -- the truncate-and-reload scenario (B-2 auto-rebuild demo)
GRANT SELECT ON ALL TABLES IN SCHEMA public TO cuboid_cdc, cuboid_verify, naive_dash;
GRANT EXECUTE ON FUNCTION pg_logical_emit_message(boolean, text, text) TO cuboid_cdc;
