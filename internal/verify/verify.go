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
	"golang.org/x/time/rate"

	"cuboid/internal/cdc"
	"cuboid/internal/config"
	"cuboid/internal/engine"
	"cuboid/internal/pipeline"
	"cuboid/internal/planner"
	"cuboid/internal/query"
	"cuboid/internal/store"
)

// Source is one source database a check reads: its fence goes into that
// database's WAL and its direct query runs in a snapshot of it.
type Source struct {
	Name      string
	Tenants   []int32 // the stores whose data it holds, of those checked
	Emitter   *cdc.Emitter
	VerifyDSN string // cuboid_verify on that database: SELECT only, statement_timeout 5s
}

// Verifier compares Cuboid's answers with direct SQL on the source databases.
type Verifier struct {
	// Sources returns the source databases holding the given stores.
	Sources  func(tenants []int32) ([]Source, error)
	Pipeline func() *pipeline.Pipeline
	Svc      *query.Service
	Store    *store.Store
	Loc      *time.Location
	Limit    *rate.Limiter
	// OnMismatch is told about every failed check and its stores: it is always a bug.
	OnMismatch func(tenants []int32, detail string)
	one        sync.Mutex // one direct query at a time

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

// errRingGap means this attempt cannot reconcile: a needed transaction left the
// buffer, or a rebuild holds the flusher. Trying again is safe.
var errRingGap = errors.New("Cuboid could not take a clean check at this moment; a rebuild may be running")

// Check runs a fenced verification, retrying if the transaction buffer no
// longer covers the reconciliation window.
func (v *Verifier) Check(ctx context.Context, t query.Target, req *query.Request) (*Result, error) {
	v.one.Lock()
	defer v.one.Unlock()
	var res *Result
	var err error
	attempt := 0
	for ; attempt < 5; attempt++ {
		res, err = v.fenced(ctx, t, req)
		if !errors.Is(err, errRingGap) {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return nil, err
	}
	res.Retries = attempt
	v.record(t, req, res)
	return res, nil
}

func token() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// part is one source database's side of a check.
type part struct {
	src      Source
	token    string
	reached  <-chan FenceReached
	fence    pglogrepl.LSN
	snap     snapshot
	direct   planner.Result
	walAfter pglogrepl.LSN
}

// FenceReached is the pipeline's signal, aliased here for brevity.
type FenceReached = pipeline.FenceReached

// fenced implements Section 10.2, once per source database (ADR-012). Each
// store's database gets its own fence and its own snapshot. Each stream holds
// at its fence, so one aggregate-store snapshot taken while all are held
// holds every store exactly as of its fence: a company's answer is checked
// whole, against the sum of its stores' direct queries.
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
		return nil, errors.New("Cuboid is starting, so it cannot fence the streams right now. Try again in a moment.")
	}
	sources, err := v.Sources(t.Tenants)
	if err != nil {
		return nil, err
	}
	began := time.Now()
	parts := make([]*part, len(sources))
	for i, src := range sources {
		pt := &part{src: src, token: token()}
		pt.reached = p.AwaitFence(pt.token)
		defer p.CancelFence(pt.token)
		parts[i] = pt
	}

	// 1 and 2, per source: the fence enters its WAL at P, then the direct
	// query runs in a REPEATABLE READ snapshot S of that database.
	start := time.Now()
	var sql string
	var args []any
	for _, pt := range parts {
		if pt.fence, err = pt.src.Emitter.Emit(ctx, cdc.PrefixFence, pt.token); err != nil {
			return nil, err
		}
		qs := q
		qs.Tenants = pt.src.Tenants
		sql, args = DirectSQL(cfg, &qs, v.Loc)
		if pt.snap, pt.direct, pt.walAfter, err = v.direct(ctx, pt.src, sql, args, cfg.Sources.Get(q.Source)); err != nil {
			return nil, err
		}
	}
	directMS := msSince(start)

	// 3. Wait until every stream reaches its fence and holds there; then the
	// aggregate store, flushed, holds each source exactly to its fence.
	var releases []chan struct{}
	released := false
	release := func() {
		if !released {
			released = true
			for _, r := range releases {
				close(r)
			}
		}
	}
	defer release()
	timeout := time.After(15 * time.Second)
	for _, pt := range parts {
		select {
		case fr := <-pt.reached:
			if fr.Busy {
				return nil, errRingGap
			}
			releases = append(releases, fr.Release)
		case <-timeout:
			return nil, fmt.Errorf("the stream of %s did not reach its fence within 15 s", pt.src.Name)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := p.Flush(ctx); err != nil {
		return nil, err
	}
	start = time.Now()
	cub, err := v.Svc.EvalAt(ctx, t, req, plan, release)
	if err != nil {
		return nil, err
	}
	cuboidMS := msSince(start)

	// 4. Per source, wait until every transaction visible in S has been
	// streamed, then reconcile the transactions whose commit order differs
	// from their visibility to S.
	ring := p.Ring()
	var rMinus, rPlus []*engine.Txn
	direct := planner.Result{}
	for _, pt := range parts {
		deadline := time.Now().Add(15 * time.Second)
		for p.Applied(pt.src.Name) < pt.walAfter {
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("the stream of %s did not catch up with the direct query's snapshot within 15 s", pt.src.Name)
			}
			time.Sleep(50 * time.Millisecond)
		}
		// Transactions out of order with the fence commit around it, so a
		// margin before this check began is enough.
		recent, covered := ring.Between(0, uint64(pt.walAfter), began.Add(-5*time.Second))
		if !covered {
			return nil, errRingGap
		}
		for _, txn := range recent {
			if txn.Source != pt.src.Name {
				continue
			}
			visible := pt.snap.visible(txn.Xid)
			if txn.CommitLSN < uint64(pt.fence) && !visible {
				rMinus = append(rMinus, txn)
			}
			// A commit at exactly the fence's LSN comes straight after it in
			// the WAL (see pipeline.messagePosition), so it counts as after.
			if txn.CommitLSN >= uint64(pt.fence) && visible {
				rPlus = append(rPlus, txn)
			}
		}
		for k, val := range pt.direct {
			direct.Add(k, val, 1)
		}
	}
	reconciled := cub
	if len(rMinus)+len(rPlus) > 0 {
		reconciled = clone(cub)
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
	res := compare(cfg.Sources.Get(q.Source), reconciled, direct.DropZeros())
	res.Mode = "fenced"
	res.FenceLSN = parts[0].fence.String()
	res.RMinus, res.RPlus = len(rMinus), len(rPlus)
	res.CuboidMS, res.DirectMS = cuboidMS, directMS
	res.DirectSQL = Printable(sql, args, v.Loc)
	if len(parts) > 1 {
		res.DirectSQL = fmt.Sprintf("-- run in each of %d store databases, for its own store; this is the last:\n", len(parts)) + res.DirectSQL
	}
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

func (v *Verifier) direct(ctx context.Context, sd Source, sql string, args []any, src *config.Source) (snapshot, planner.Result, pglogrepl.LSN, error) {
	conn, err := pgx.Connect(ctx, sd.VerifyDSN)
	if err != nil {
		return snapshot{}, nil, 0, fmt.Errorf("connect to %s for the direct query: %w", sd.Name, err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
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
		return snapshot{}, nil, 0, fmt.Errorf("direct query on %s: %w", sd.Name, err)
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
		if v.OnMismatch != nil {
			v.OnMismatch(t.Tenants, fmt.Sprintf("stores %v, %s, rollup-vs-direct: %s, fence %s, reconciled -%d +%d", t.Tenants, req.Canonical, strings.Join(res.Mismatches, "; "), res.FenceLSN, res.RMinus, res.RPlus))
		}
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
