# Postgres notes

Verified against PostgreSQL 16.4. Each gotcha below was found by running Cuboid, not by reading about it.

## Settings on the main database
`wal_level=logical`, `max_replication_slots=10`, `max_wal_senders=10`, `max_slot_wal_keep_size=4GB` (a stuck slot cannot fill the disk), `shared_preload_libraries=pg_stat_statements`, `track_io_timing=on`, `timezone=Asia/Kolkata`.

## Gotchas

### A message's LSN and a commit's LSN measure different ends of a record
`pgoutput` reports a logical decoding message at the **end** of its WAL record (`buf->endptr`), and a commit at the **start** of its record (`buf->origptr`). A commit written directly after a heartbeat therefore has exactly the heartbeat's LSN.

Cuboid skips redelivered transactions with `commit_lsn <= applied`. When `applied` was set from a heartbeat at its own LSN, that new transaction looked already applied and was dropped, silently. It lost 2 orders in about three minutes of writes at roughly 50 orders a second, and was found by the fenced check: answers permanently off by one order, with nothing to reconcile.

The fix records a message's position as `lsn - 1` (`internal/pipeline/pipeline.go`, `messagePosition`). The fence comparison in the verifier counts a commit at exactly the fence's LSN as after the fence. Test: `internal/pipeline/position_test.go`.

### CHECK constraints run before ON CONFLICT
`INSERT ... ON CONFLICT DO UPDATE` evaluates `CHECK` constraints on the **proposed** row first. With `CHECK (orders >= 0)`, a retraction (a delta of -1) fails even when the existing cell holds 100. Cuboid uses `UPDATE ... FROM unnest(...)` plus `INSERT ... WHERE NOT EXISTS` in one statement. The flusher is the only writer, so there is no insert race, and a retraction with no cell to retract from still fails loudly.

### COPY FROM is refused under forced row-level security
`COPY FROM` returns `0A000 COPY FROM not supported with row-level security` for a table with `FORCE ROW LEVEL SECURITY`, even for its owner. Shadow tables therefore get RLS in the transaction that swaps them in (`store.SwapIn`). Only the writer can reach a shadow before that.

### Exported snapshots need the exporting connection idle
The snapshot from `CREATE_REPLICATION_SLOT ... EXPORT_SNAPSHOT` lives only until that connection's next command, so the connection stays open and idle for the whole backfill. Backfill workers import it with `SET TRANSACTION SNAPSHOT`.

### Commit order is not visibility order
A transaction can have its commit record before a fence and still be invisible to a snapshot taken just after, because a commit reaches the WAL before it leaves the proc array. The fenced verifier reconciles both directions using `pg_current_snapshot()`. Stream xids are 32-bit and snapshot xids are `xid8`; they are mapped using the snapshot's epoch.

### Role and function details
- `pg_logical_emit_message(boolean, text, text)` is the Postgres 16 signature. Postgres 17 adds a fourth `flush` argument, so the `GRANT` in `migrations/main/001_schema.sql` is specific to 16.
- `date_trunc(field, timestamptz, zone)` needs Postgres 14 or later.
- `set_config(name, value, true)` is transaction-local, which is what makes a pooled connection safe to reuse across tenants.

### One database per store (ADR-012)
- **A template with a connection cannot be copied.** `CREATE DATABASE store_42 TEMPLATE store_template` fails with "source database is being accessed by other users" while anyone is connected to the template. Cuboid connects to it only to read the store schema at startup; the seed retries.
- **A database with a replication slot cannot be dropped.** The seed ends every Cuboid session and drops its slots, and keeps doing so until none has appeared for 3 seconds, before it drops store databases: Cuboid may be creating one meanwhile.
- **COPY FROM is refused under forced row-level security, so a store's load copies into a temporary table.** A store reloads in one transaction, into the live mirror, which forces RLS. The rows land in a `TEMP` table first and move with `INSERT ... SELECT` in the same transaction.
- **Every WAL sender reads the whole server's WAL.** A logical slot belongs to one database, but its sender decodes every record on the server and skips other databases'. With a slot per store, decoding work grows with the number of stores. `logical_decoding_work_mem` is set to 16 MB, since it is per sender.
- **Stores' dictionary values are seeded before their load.** Stores loading at once would otherwise insert the same dimension values in their long transactions and wait on one another. Each load inserts its values in a short transaction first, in a fixed order.

## Windows note for the native development path
`pg_ctl start` leaves the server holding the caller's stdout. A pipeline such as `pg_ctl ... | tail` never finishes, so `tools/devdb.sh` redirects it to a file.
