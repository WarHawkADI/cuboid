-- AGG database: Cuboid's separate aggregate store (ADR-004).
-- Runs once, as the superuser, when the aggdb container initialises.
-- cuboid_writer owns and writes everything; cuboid_reader serves queries and
-- has no BYPASSRLS, so every tenant table is filtered by row-level security.

CREATE ROLE cuboid_writer LOGIN PASSWORD 'cuboid_writer';
CREATE ROLE cuboid_reader LOGIN PASSWORD 'cuboid_reader' NOBYPASSRLS;
ALTER DATABASE agg OWNER TO cuboid_writer;
ALTER DATABASE agg SET timezone TO 'Asia/Kolkata';
GRANT ALL ON SCHEMA public TO cuboid_writer;
GRANT USAGE ON SCHEMA public TO cuboid_reader;
