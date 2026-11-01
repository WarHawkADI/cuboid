package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"cuboid/internal/config"
	"cuboid/internal/timeutil"
)

// Column is one mirrored column with its exact SQL type from the main DB.
type Column struct {
	Name string
	Type string
}

// Table describes how one source table is mirrored in AGG: every column
// except PII, with the main database's own types.
type Table struct {
	Source  *config.Source
	Columns []Column
	Key     []string
}

// Name is the mirror table's name.
func (t *Table) Name() string { return config.MirrorTable(t.Source.Name) }

// ColumnNames returns the mirrored column names in order.
func (t *Table) ColumnNames() []string {
	out := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		out[i] = c.Name
	}
	return out
}

// Type returns a column's SQL type.
func (t *Table) Type(col string) string {
	for _, c := range t.Columns {
		if c.Name == col {
			return c.Type
		}
	}
	return ""
}

// Catalog reads every column of the configured source tables from the main
// database's catalog. It returns table -> columns (all, including PII) and
// the mirror specs (PII excluded).
func Catalog(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, cfg *config.Config) (map[string][]string, map[string]*Table, error) {
	names := cfg.Sources.Keys()
	rows, err := q.Query(ctx, `
		SELECT c.relname, a.attname, format_type(a.atttypid, a.atttypmod)
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relname = ANY($1) AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY c.relname, a.attnum`, names)
	if err != nil {
		return nil, nil, fmt.Errorf("read main database catalog: %w", err)
	}
	defer rows.Close()
	all := map[string][]string{}
	specs := map[string]*Table{}
	for rows.Next() {
		var table, col, typ string
		if err := rows.Scan(&table, &col, &typ); err != nil {
			return nil, nil, err
		}
		all[table] = append(all[table], col)
		src := cfg.Sources.Get(table)
		if src.IsPII(col) {
			continue
		}
		t := specs[table]
		if t == nil {
			t = &Table{Source: src, Key: src.Key}
			specs[table] = t
		}
		t.Columns = append(t.Columns, Column{Name: col, Type: typ})
	}
	return all, specs, rows.Err()
}

// Checks reads the validated CHECK constraints of the configured source
// tables from the main database's catalog: table -> definitions.
func Checks(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, cfg *config.Config) (map[string][]string, error) {
	rows, err := q.Query(ctx, `
		SELECT c.relname, pg_get_constraintdef(k.oid)
		FROM pg_constraint k
		JOIN pg_class c ON c.oid = k.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relname = ANY($1) AND k.contype = 'c' AND k.convalidated`, cfg.Sources.Keys())
	if err != nil {
		return nil, fmt.Errorf("read main database constraints: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var table, def string
		if err := rows.Scan(&table, &def); err != nil {
			return nil, err
		}
		out[table] = append(out[table], def)
	}
	return out, rows.Err()
}

// MirrorDDL creates or extends a mirror table, with RLS.
func MirrorDDL(t *Table, tenantCol string) []string {
	name := QuoteIdent(t.Name())
	var cols []string
	for _, c := range t.Columns {
		cols = append(cols, fmt.Sprintf("%s %s", QuoteIdent(c.Name), c.Type))
	}
	keys := quoteAll(t.Key)
	stmts := []string{
		fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (%s, PRIMARY KEY (%s))", name, strings.Join(cols, ", "), strings.Join(keys, ", ")),
	}
	for _, c := range t.Columns {
		stmts = append(stmts, fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s", name, QuoteIdent(c.Name), c.Type))
	}
	if t.Source.Time != "" {
		stmts = append(stmts, fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (%s, %s)",
			QuoteIdent(t.Name()+"_time"), name, QuoteIdent(tenantCol), QuoteIdent(t.Source.Time)))
	}
	return withRLS(stmts, t.Name(), "")
}

// CellDDL creates or extends a fact source's rollup cell table.
func CellDDL(src *config.Source, suffix string) []string {
	table := config.CellTable(src.Name) + suffix
	name := QuoteIdent(table)
	stmts := []string{fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		tenant_id int NOT NULL, rollup smallint NOT NULL, grain smallint NOT NULL, bucket timestamptz NOT NULL,
		k1 bigint NOT NULL DEFAULT 0, k2 bigint NOT NULL DEFAULT 0, k3 bigint NOT NULL DEFAULT 0,
		PRIMARY KEY (tenant_id, rollup, grain, bucket, k1, k2, k3)) WITH (fillfactor = 90)`, name)}
	for _, m := range src.Measures.Values() {
		stmts = append(stmts, fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s bigint NOT NULL DEFAULT 0", name, QuoteIdent(m.Name)))
	}
	// A negative count is a bug, never a number: the flush fails loudly.
	cm := src.CountMeasure
	stmts = append(stmts, fmt.Sprintf(`DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = %s) THEN
			ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s >= 0);
		END IF; END $$`, quoteLiteral(table+"_count_nonneg"), name, QuoteIdent(table+"_count_nonneg"), QuoteIdent(cm)))
	// Rank rollups' month cells by value, so a top-N list reads each month's
	// largest cells first and stops long before the rest (ADR-011). One- and
	// zero-dimension rollups match the predicate too; they are a few cells.
	if src.Ranked {
		for _, m := range src.Measures.Values() {
			if !m.NeverNegative() {
				continue
			}
			stmts = append(stmts, fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (tenant_id, rollup, bucket, %s) INCLUDE (k1) WHERE grain = %d AND k2 = 0",
				QuoteIdent(table+"_rank_"+m.Name), name, QuoteIdent(m.Name), timeutil.GrainMonth))
		}
	}
	return withRLS(stmts, config.CellTable(src.Name), suffix)
}

// withRLS adds row-level security to a live table. Shadow tables get it in
// SwapIn instead: COPY FROM is not allowed under forced RLS, and only the
// writer can reach a shadow until the transaction that swaps it in.
func withRLS(stmts []string, table, suffix string) []string {
	if suffix != "" {
		return stmts
	}
	return append(stmts, rlsDDL(table)...)
}

func rlsDDL(table string) []string {
	name := QuoteIdent(table)
	return []string{
		fmt.Sprintf("ALTER TABLE %s ENABLE ROW LEVEL SECURITY", name),
		fmt.Sprintf("ALTER TABLE %s FORCE ROW LEVEL SECURITY", name),
		fmt.Sprintf("DROP POLICY IF EXISTS tenant_read ON %s", name),
		fmt.Sprintf("CREATE POLICY tenant_read ON %s FOR SELECT TO cuboid_reader USING (%s)", name, RLSPolicy),
		fmt.Sprintf("DROP POLICY IF EXISTS writer_all ON %s", name),
		fmt.Sprintf("CREATE POLICY writer_all ON %s TO cuboid_writer USING (true) WITH CHECK (true)", name),
		fmt.Sprintf("GRANT SELECT ON %s TO cuboid_reader", name),
	}
}

// EnsureTables creates every mirror and cell table for the config.
func (s *Store) EnsureTables(ctx context.Context, cfg *config.Config, specs map[string]*Table) error {
	tx, err := s.W.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, name := range cfg.Sources.Keys() {
		t := specs[name]
		if t == nil {
			return fmt.Errorf("source %s: table not found in the main database", name)
		}
		for _, stmt := range MirrorDDL(t, cfg.Tenancy.TenantColumn) {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}
	}
	for _, src := range cfg.FactSources() {
		for _, stmt := range CellDDL(src, "") {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}
	}
	return tx.Commit(ctx)
}

// RegisterRollups gives every configured rollup a persistent id and returns
// name -> ready.
func (s *Store) RegisterRollups(ctx context.Context, cfg *config.Config) (map[string]bool, error) {
	rows, err := s.W.Query(ctx, "SELECT name, id, ready FROM rollup_registry")
	if err != nil {
		return nil, err
	}
	existing := map[string]int16{}
	ready := map[string]bool{}
	for rows.Next() {
		var name string
		var id int16
		var r bool
		if err := rows.Scan(&name, &id, &r); err != nil {
			rows.Close()
			return nil, err
		}
		existing[name], ready[name] = id, r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	cfg.AssignIDs(existing)
	for _, r := range cfg.Rollups.Values() {
		def := fmt.Sprintf("%s %v %v", r.Source, r.Dims, r.GrainSpec)
		if _, err := s.W.Exec(ctx, `INSERT INTO rollup_registry (name, id, source, definition) VALUES ($1, $2, $3, $4)
			ON CONFLICT (name) DO NOTHING`, r.Name, r.ID, r.Source, def); err != nil {
			return nil, err
		}
	}
	return ready, nil
}

// SetReady marks rollups as fully built.
func SetReady(ctx context.Context, tx pgx.Tx, names []string, ready bool) error {
	_, err := tx.Exec(ctx, "UPDATE rollup_registry SET ready = $2 WHERE name = ANY($1)", names, ready)
	return err
}

func quoteAll(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = QuoteIdent(x)
	}
	return out
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// SwapIn replaces each live table with its shadow copy inside tx: the old
// table is dropped and the shadow's indexes and constraints take the live
// names. Readers see either the old tables or the new ones, never a mix.
func SwapIn(ctx context.Context, tx pgx.Tx, live []string, suffix string) error {
	for _, l := range live {
		sh := l + suffix
		stmts := []string{
			fmt.Sprintf("ALTER TABLE %s RENAME TO %s", QuoteIdent(l), QuoteIdent(l+"__old")),
			fmt.Sprintf("ALTER TABLE %s RENAME TO %s", QuoteIdent(sh), QuoteIdent(l)),
			fmt.Sprintf("DROP TABLE %s", QuoteIdent(l+"__old")),
		}
		stmts = append(stmts, rlsDDL(l)...)
		for _, s := range stmts {
			if _, err := tx.Exec(ctx, s); err != nil {
				return fmt.Errorf("swap %s: %w", l, err)
			}
		}
		rows, err := tx.Query(ctx, `SELECT c.relname FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
			WHERE i.indrelid = $1::regclass`, l)
		if err != nil {
			return err
		}
		var idx []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			idx = append(idx, n)
		}
		rows.Close()
		for _, n := range idx {
			if nn := strings.Replace(n, sh, l, 1); nn != n {
				if _, err := tx.Exec(ctx, fmt.Sprintf("ALTER INDEX %s RENAME TO %s", QuoteIdent(n), QuoteIdent(nn))); err != nil {
					return err
				}
			}
		}
		rows, err = tx.Query(ctx, `SELECT conname FROM pg_constraint WHERE conrelid = $1::regclass AND contype = 'c'`, l)
		if err != nil {
			return err
		}
		var cons []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			cons = append(cons, n)
		}
		rows.Close()
		for _, n := range cons {
			if nn := strings.Replace(n, sh, l, 1); nn != n {
				if _, err := tx.Exec(ctx, fmt.Sprintf("ALTER TABLE %s RENAME CONSTRAINT %s TO %s", QuoteIdent(l), QuoteIdent(n), QuoteIdent(nn))); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// DropShadows removes shadow cell tables left by an interrupted rebuild from
// the mirror.
func (s *Store) DropShadows(ctx context.Context, cfg *config.Config) error {
	for _, src := range cfg.FactSources() {
		if _, err := s.W.Exec(ctx, "DROP TABLE IF EXISTS "+QuoteIdent(config.CellTable(src.Name)+"_shadow")); err != nil {
			return err
		}
	}
	return nil
}
