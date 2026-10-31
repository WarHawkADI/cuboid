package verify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"

	"cuboid/internal/cdc"
	"cuboid/internal/config"
	"cuboid/internal/engine"
	"cuboid/internal/pipeline"
	"cuboid/internal/planner"
	"cuboid/internal/query"
	"cuboid/internal/store"
)

// Verifier compares Cuboid's answers with direct SQL on the main database.
type Verifier struct {
	Main     *pgxpool.Pool // cuboid_verify: SELECT only, statement_timeout 5s, two connections
	Emitter  *cdc.Emitter
	Pipeline func() *pipeline.Pipeline
	Svc      *query.Service
	Store    *store.Store
	Loc      *time.Location
	Limit    *rate.Limiter
	one      sync.Mutex // one direct query at a time

	Checks   atomic.Int64
	Exact    atomic.Int64
	Mismatch atomic.Int64
	mu       sync.Mutex
	recent   []Summary
}

// Row is one result row as people read it.
type Row struct {
	Key    []string `json:"key"`
	Values []string `json:"values"`
}

// Result is one verification.
type Result struct {
	Exact      bool     `json:"exact"`
	Verdict    string   `json:"verdict"`
	Mode       string   `json:"mode"`
	FenceLSN   string   `json:"fence_lsn"`
	RMinus     int      `json:"r_minus"`
	RPlus      int      `json:"r_plus"`
	Retries    int      `json:"retries"`
	CuboidMS   float64  `json:"cuboid_ms"`
	DirectMS   float64  `json:"direct_ms"`
	DirectSQL  string   `json:"direct_sql"`
	Measures   []string `json:"measures"`
	Cuboid     []Row    `json:"cuboid"`
	Direct     []Row    `json:"direct"`
	Rows       int      `json:"rows_compared"`
	Mismatches []string `json:"mismatches,omitempty"`
}

// Summary is a verification as the monitoring page lists it.
type Summary struct {
	At       time.Time `json:"at"`
	Query    string    `json:"query"`
	Tenants  []int32   `json:"tenants"`
	Exact    bool      `json:"exact"`
	CuboidMS float64   `json:"cuboid_ms"`
	DirectMS float64   `json:"direct_ms"`
	RMinus   int       `json:"r_minus"`
	RPlus    int       `json:"r_plus"`
}

// Recent returns the latest verifications, newest first.
func (v *Verifier) Recent() []Summary {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]Summary, len(v.recent))
	for i := range v.recent {
		out[i] = v.recent[len(v.recent)-1-i]
	}
	return out
}

// HTTP adapts the verifier to the API's VerifyFunc.
func (v *Verifier) HTTP(ctx context.Context, t query.Target, req *query.Request) (any, *query.Problem) {
	if !v.Limit.Allow() {
		return nil, &query.Problem{Status: 429, Type: "rate_limited", Title: "Limited", Detail: "Checks against the main database are budgeted; try again in a few seconds."}
	}
	res, err := v.Check(ctx, t, req)
	if err != nil {
		return nil, &query.Problem{Status: 503, Type: "verify_failed", Title: "Check did not complete", Detail: err.Error()}
	}
	return res, nil
}

var errRingGap = errors.New("a transaction needed for reconciliation has left the buffer")

// Check runs a fenced verification, retrying if the transaction buffer no
// longer covers the reconciliation window.
func (v *Verifier) Check(ctx context.Context, t query.Target, req *query.Request) (*Result, error) {
	v.one.Lock()
	defer v.one.Unlock()
	var res *Result
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		res, err = v.fenced(ctx, t, req)
		if !errors.Is(err, errRingGap) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	v.record(t, req, res)
	return res, nil
}

func token() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// fenced implements Section 10.2.
func (v *Verifier) fenced(ctx context.Context, t query.Target, req *query.Request) (*Result, error) {
	act := v.Svc.Active()
	cfg := act.Cfg
	q := req.Query
	q.Tenants = t.Tenants
	plan, err := act.Planner.Plan(&q)
	if err != nil {
		return nil, err
	}
	p := v.Pipeline()
	if p == nil {
		return nil, errors.New("the change stream is not running")
	}
	tok := token()
	reached := p.AwaitFence(tok)
	defer p.CancelFence(tok)

	// 1. The fence enters the WAL now, at P.
	fence, err := v.Emitter.Emit(ctx, cdc.PrefixFence, tok)
	if err != nil {
		return nil, err
	}

	// 3 (concurrently). When the stream reaches P, AGG holds exactly the
	// transactions committed before P: evaluate Cuboid there.
	type agg struct {
		res planner.Result
		ms  float64
		err error
	}
	aggc := make(chan agg, 1)
	go func() {
		select {
		case fr := <-reached:
			start := time.Now()
			r, err := v.Svc.EvalAt(ctx, t, plan, func() { close(fr.Release) })
			aggc <- agg{res: r, ms: msSince(start), err: err}
		case <-time.After(15 * time.Second):
			aggc <- agg{err: errors.New("the stream did not reach the fence within 15 s")}
		case <-ctx.Done():
			aggc <- agg{err: ctx.Err()}
		}
	}()

	// 2. The direct query in a REPEATABLE READ snapshot S on MAIN.
	sql, args := DirectSQL(cfg, &q, v.Loc)
	start := time.Now()
	snap, direct, walAfter, err := v.direct(ctx, sql, args, cfg.Sources.Get(q.Source))
	directMS := msSince(start)
	if err != nil {
		return nil, err
	}
	a := <-aggc
	if a.err != nil {
		return nil, a.err
	}

	// 4. Wait until every transaction visible in S has been streamed.
	deadline := time.Now().Add(15 * time.Second)
	for p.Applied() < walAfter {
		if time.Now().After(deadline) {
			return nil, errors.New("the stream did not catch up with the direct query's snapshot within 15 s")
		}
		time.Sleep(50 * time.Millisecond)
	}
	ring := p.Ring()
	var rMinus, rPlus []*engine.Txn
	recent, covered := ring.Between(0, uint64(walAfter))
	if !covered {
		return nil, errRingGap
	}
	for _, txn := range recent {
		visible := snap.visible(txn.Xid)
		if txn.CommitLSN < uint64(fence) && !visible {
			rMinus = append(rMinus, txn)
		}
		if txn.CommitLSN > uint64(fence) && visible {
			rPlus = append(rPlus, txn)
		}
	}
	reconciled := a.res
	if len(rMinus)+len(rPlus) > 0 {
		reconciled = clone(a.res)
		err := p.Do(ctx, func() error {
			if err := applyEffect(p.Engine, plan, rMinus, -1, reconciled, v.Loc); err != nil {
				return err
			}
			return applyEffect(p.Engine, plan, rPlus, +1, reconciled, v.Loc)
		})
		if err != nil {
			return nil, err
		}
		reconciled.DropZeros()
	}
	res := compare(cfg.Sources.Get(q.Source), reconciled, direct)
	res.Mode = "fenced"
	res.FenceLSN = fence.String()
	res.RMinus, res.RPlus = len(rMinus), len(rPlus)
	res.CuboidMS, res.DirectMS = a.ms, directMS
	res.DirectSQL = Printable(sql, args, v.Loc)
	return res, nil
}

type snapshot struct {
	xmin, xmax uint64
	xip        map[uint64]bool
}

func parseSnapshot(s string) (snapshot, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return snapshot{}, fmt.Errorf("unexpected snapshot %q", s)
	}
	var sn snapshot
	var err error
	if sn.xmin, err = strconv.ParseUint(parts[0], 10, 64); err != nil {
		return sn, err
	}
	if sn.xmax, err = strconv.ParseUint(parts[1], 10, 64); err != nil {
		return sn, err
	}
	sn.xip = map[uint64]bool{}
	for _, x := range strings.Split(parts[2], ",") {
		if x == "" {
			continue
		}
		n, err := strconv.ParseUint(x, 10, 64)
		if err != nil {
			return sn, err
		}
		sn.xip[n] = true
	}
	return sn, nil
}

// visible reports whether a committed transaction (32-bit stream xid) is
// visible in the snapshot (64-bit xid8), mapping it with the snapshot's epoch.
func (s snapshot) visible(xid32 uint32) bool {
	epoch := s.xmax >> 32
	x := epoch<<32 | uint64(xid32)
	if x > s.xmax && epoch > 0 {
		x -= 1 << 32
	}
	if x < s.xmin {
		return true
	}
	if x >= s.xmax {
		return false
	}
	return !s.xip[x]
}

func (v *Verifier) direct(ctx context.Context, sql string, args []any, src *config.Source) (snapshot, planner.Result, pglogrepl.LSN, error) {
	tx, err := v.Main.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return snapshot{}, nil, 0, err
	}
	defer tx.Rollback(context.Background())
	var snapText string
	if err := tx.QueryRow(ctx, "SELECT pg_current_snapshot()::text").Scan(&snapText); err != nil {
		return snapshot{}, nil, 0, err
	}
	snap, err := parseSnapshot(snapText)
	if err != nil {
		return snapshot{}, nil, 0, err
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return snapshot{}, nil, 0, fmt.Errorf("direct query on the main database: %w", err)
	}
	res := planner.Result{}
	nm := src.Measures.Len()
	ngroup := len(rows.FieldDescriptions()) - 2 - nm
	for rows.Next() {
		var tenant int32
		var bucket int64
		keys := make([]*string, ngroup)
		vals := make([]int64, nm)
		dest := []any{&tenant, &bucket}
		for i := range keys {
			dest = append(dest, &keys[i])
		}
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return snapshot{}, nil, 0, err
		}
		k := planner.ResultKey{Tenant: tenant, Bucket: bucket}
		for i, s := range keys {
			if s == nil {
				k.K[i] = engine.Null
			} else {
				k.K[i] = *s
			}
		}
		res.Add(k, vals, 1)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return snapshot{}, nil, 0, err
	}
	var lsn string
	if err := tx.QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&lsn); err != nil {
		return snapshot{}, nil, 0, err
	}
	wal, err := pglogrepl.ParseLSN(lsn)
	if err != nil {
		return snapshot{}, nil, 0, err
	}
	return snap, res.DropZeros(), wal, tx.Commit(ctx)
}

// applyEffect adds sign × each transaction's effect on the query result,
// row by row, with the engine's own resolution of rows into facts.
func applyEffect(e *engine.Engine, pl *planner.Plan, txns []*engine.Txn, sign int64, res planner.Result, loc *time.Location) error {
	q := pl.Query
	src := pl.Source
	for _, txn := range txns {
		for _, c := range txn.Changes {
			if c.Table != src.Name {
				continue
			}
			for _, row := range []struct {
				r engine.Row
				s int64
			}{{c.Old, -1}, {c.New, +1}} {
				if row.r == nil {
					continue
				}
				f, err := e.Resolve(src, row.r)
				if err != nil {
					return err
				}
				if !containsTenant(q.Tenants, f.Tenant) || f.Time.Before(q.From) || !f.Time.Before(q.To) {
					continue
				}
				if k, ok := pl.KeyOfFact(f, loc); ok {
					res.Add(k, f.Measures, sign*row.s)
				}
			}
		}
	}
	return nil
}

func containsTenant(ts []int32, t int32) bool {
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}

func clone(r planner.Result) planner.Result {
	out := planner.Result{}
	for k, v := range r {
		out.Add(k, v, 1)
	}
	return out
}

func msSince(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }

// compare checks every row and every value.
func compare(src *config.Source, cub, dir planner.Result) *Result {
	res := &Result{Exact: true, Verdict: "Exact match", Measures: src.Measures.Keys()}
	keys := map[planner.ResultKey]bool{}
	for k := range cub {
		keys[k] = true
	}
	for k := range dir {
		keys[k] = true
	}
	ordered := make([]planner.ResultKey, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.Tenant != b.Tenant {
			return a.Tenant < b.Tenant
		}
		if a.Bucket != b.Bucket {
			return a.Bucket < b.Bucket
		}
		return fmt.Sprint(a.K) < fmt.Sprint(b.K)
	})
	render := func(k planner.ResultKey, v []int64) Row {
		r := Row{Key: []string{strconv.Itoa(int(k.Tenant))}}
		if k.Bucket != 0 {
			r.Key = append(r.Key, time.Unix(k.Bucket, 0).UTC().Format(time.RFC3339))
		}
		for _, s := range k.K {
			if s != "" {
				if s == engine.Null {
					s = "(none)"
				}
				r.Key = append(r.Key, s)
			}
		}
		for i, m := range src.Measures.Values() {
			x := int64(0)
			if v != nil {
				x = v[i]
			}
			r.Values = append(r.Values, fmt.Sprint(renderValue(m, x)))
		}
		return r
	}
	for _, k := range ordered {
		c, d := cub[k], dir[k]
		if fmt.Sprint(c) != fmt.Sprint(d) {
			res.Exact = false
			if len(res.Mismatches) < 20 {
				res.Mismatches = append(res.Mismatches, fmt.Sprintf("%v: Cuboid %v, direct %v", render(k, nil).Key, c, d))
			}
		}
		if len(res.Cuboid) < 50 {
			res.Cuboid = append(res.Cuboid, render(k, c))
			res.Direct = append(res.Direct, render(k, d))
		}
	}
	res.Rows = len(ordered)
	if !res.Exact {
		res.Verdict = "Mismatch"
	}
	return res
}

func renderValue(m *config.Measure, v int64) any {
	if m.Scale > 0 {
		s := strconv.FormatInt(v, 10)
		neg := strings.HasPrefix(s, "-")
		s = strings.TrimPrefix(s, "-")
		for len(s) <= m.Scale {
			s = "0" + s
		}
		s = s[:len(s)-m.Scale] + "." + s[len(s)-m.Scale:]
		if neg {
			s = "-" + s
		}
		return s
	}
	return v
}

func (v *Verifier) record(t query.Target, req *query.Request, res *Result) {
	v.Checks.Add(1)
	if res.Exact {
		v.Exact.Add(1)
	} else {
		v.Mismatch.Add(1)
	}
	s := Summary{At: time.Now(), Query: req.Canonical, Tenants: t.Tenants, Exact: res.Exact, CuboidMS: res.CuboidMS, DirectMS: res.DirectMS, RMinus: res.RMinus, RPlus: res.RPlus}
	v.mu.Lock()
	v.recent = append(v.recent, s)
	if len(v.recent) > 200 {
		v.recent = v.recent[len(v.recent)-200:]
	}
	v.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	v.Store.W.Exec(ctx, `INSERT INTO verifications (mode, tenant_ids, query, exact, fence_lsn, cuboid_ms, direct_ms, r_minus, r_plus, retries)
		VALUES ($1, $2, $3, $4, $5::pg_lsn, $6, $7, $8, $9, $10)`,
		res.Mode, t.Tenants, req.Canonical, res.Exact, res.FenceLSN, res.CuboidMS, res.DirectMS, res.RMinus, res.RPlus, res.Retries)
}
