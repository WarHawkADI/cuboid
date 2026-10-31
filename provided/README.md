# The organisers' data script

Put their files here. This folder's other contents stay out of git. Their script becomes the data source in three steps:

1. Add `run.sh`, which runs their script. The compose service runs it in a `postgres:16-alpine` container, with this folder at `/provided` and `PGHOST`, `PGUSER`, `PGPASSWORD`, `PGDATABASE` set for the main database. Arguments pass through:

   `docker compose --profile provided run --rm provided-seed --tier small`

2. Map their tables in `config/metrics.yaml`: the sources, measures, dimensions and rollups. If the schema differs from the fallback's, the store app and seed do not apply, but the engine reads only that file.

3. Rebuild from the main database, so the first load uses one consistent snapshot: `make rebuild`.

Then re-measure with `make demo-check` and `make targets`, and list the script in `docs/DISCLOSURE.md`.

If their script targets MongoDB, load its output into Postgres. A MongoDB change-stream adapter does not exist in this repository.
