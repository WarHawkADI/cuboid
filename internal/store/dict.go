package store

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
)

// DimValue is one dimension value to encode.
type DimValue struct{ Dim, Value string }

// Dict caches the dimension dictionary. Ids are allocated only by the
// database (a sequence), so the flusher and set-based rebuild SQL never
// disagree on an id.
type Dict struct {
	mu   sync.RWMutex
	ids  map[DimValue]int64
	vals map[int64]string
}

// NewDict returns an empty dictionary cache.
func NewDict() *Dict { return &Dict{ids: map[DimValue]int64{}, vals: map[int64]string{}} }

type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Load reads the whole dictionary.
func (d *Dict) Load(ctx context.Context, q queryer) error {
	rows, err := q.Query(ctx, "SELECT id, dim, value FROM dict")
	if err != nil {
		return fmt.Errorf("load dictionary: %w", err)
	}
	defer rows.Close()
	ids := map[DimValue]int64{}
	vals := map[int64]string{}
	for rows.Next() {
		var id int64
		var dv DimValue
		if err := rows.Scan(&id, &dv.Dim, &dv.Value); err != nil {
			return err
		}
		ids[dv], vals[id] = id, dv.Value
	}
	if err := rows.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	d.ids, d.vals = ids, vals
	d.mu.Unlock()
	return nil
}

// Lookup returns the id of a value, if known.
func (d *Dict) Lookup(dim, value string) (int64, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	id, ok := d.ids[DimValue{dim, value}]
	return id, ok
}

// Value returns the value of an id, if known.
func (d *Dict) Value(id int64) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	v, ok := d.vals[id]
	return v, ok
}

// ValueOrLoad returns the value of an id, reading it from the database when
// a just-committed flush added it after this cache was last updated.
func (d *Dict) ValueOrLoad(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, id int64) (string, error) {
	if v, ok := d.Value(id); ok {
		return v, nil
	}
	var dv DimValue
	if err := q.QueryRow(ctx, "SELECT dim, value FROM dict WHERE id = $1", id).Scan(&dv.Dim, &dv.Value); err != nil {
		return "", fmt.Errorf("dictionary id %d: %w", id, err)
	}
	d.Remember(map[DimValue]int64{dv: id})
	return dv.Value, nil
}

// Resolve returns ids for every value, inserting unknown values inside tx.
// New ids are returned but not cached: call Remember after tx commits.
func (d *Dict) Resolve(ctx context.Context, tx pgx.Tx, want map[DimValue]bool) (map[DimValue]int64, error) {
	out := make(map[DimValue]int64, len(want))
	var dims, vals []string
	for dv := range want {
		if id, ok := d.Lookup(dv.Dim, dv.Value); ok {
			out[dv] = id
			continue
		}
		dims = append(dims, dv.Dim)
		vals = append(vals, dv.Value)
	}
	if len(dims) == 0 {
		return out, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO dict (dim, value) SELECT * FROM unnest($1::text[], $2::text[])
		ON CONFLICT (dim, value) DO NOTHING`, dims, vals); err != nil {
		return nil, fmt.Errorf("insert dictionary values: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT d.id, d.dim, d.value FROM dict d
		JOIN unnest($1::text[], $2::text[]) AS u(dim, value) ON d.dim = u.dim AND d.value = u.value`, dims, vals)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var dv DimValue
		if err := rows.Scan(&id, &dv.Dim, &dv.Value); err != nil {
			return nil, err
		}
		out[dv] = id
	}
	return out, rows.Err()
}

// Remember adds committed ids to the cache.
func (d *Dict) Remember(ids map[DimValue]int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for dv, id := range ids {
		d.ids[dv] = id
		d.vals[id] = dv.Value
	}
}
