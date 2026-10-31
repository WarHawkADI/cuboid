package planner

import (
	"time"

	"cuboid/internal/config"
	"cuboid/internal/engine"
	"cuboid/internal/timeutil"
)

// ResultKey identifies one row of a query result: the tenant it came from,
// the trend bucket (0 when the query has no interval) and the group-by values
// in GroupBy order.
type ResultKey struct {
	Tenant int32
	Bucket int64
	K      [config.MaxDims]string
}

// Result maps result keys to measure vectors in the source's measure order.
type Result map[ResultKey][]int64

// Add accumulates v under k.
func (r Result) Add(k ResultKey, v []int64, sign int64) {
	cur, ok := r[k]
	if !ok {
		cur = make([]int64, len(v))
		r[k] = cur
	}
	for i := range v {
		cur[i] += sign * v[i]
	}
}

// DropZeros removes rows whose measures are all zero (an empty group is not a row).
func (r Result) DropZeros() Result {
	for k, v := range r {
		zero := true
		for _, x := range v {
			if x != 0 {
				zero = false
				break
			}
		}
		if zero {
			delete(r, k)
		}
	}
	return r
}

func inTenants(t int32, ts []int32) bool {
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}

func (pl *Plan) bucketOf(t time.Time, loc *time.Location) int64 {
	if pl.Query.Interval == timeutil.GrainNone {
		return 0
	}
	return timeutil.Trunc(t, pl.Query.Interval, loc).Unix()
}

// KeyOfFact returns the result key a mirror row contributes to, or false if
// the row is filtered out. Range and tenant checks are the caller's.
func (pl *Plan) KeyOfFact(f *engine.Fact, loc *time.Location) (ResultKey, bool) {
	for d, v := range pl.Query.Filters {
		if f.Dims[d] != v {
			return ResultKey{}, false
		}
	}
	k := ResultKey{Tenant: f.Tenant, Bucket: pl.bucketOf(f.Time, loc)}
	for i, d := range pl.Query.GroupBy {
		k.K[i] = f.Dims[d]
	}
	return k, true
}

// KeyOfCell returns the result key a rollup cell contributes to, or false if
// it is filtered out.
func (pl *Plan) KeyOfCell(ck engine.CellKey, loc *time.Location) (ResultKey, bool) {
	for d, v := range pl.Query.Filters {
		if ck.K[pl.FilterPos[d]] != v {
			return ResultKey{}, false
		}
	}
	k := ResultKey{Tenant: ck.Tenant, Bucket: pl.bucketOf(time.Unix(ck.Bucket, 0), loc)}
	for i, pos := range pl.Pos {
		k.K[i] = ck.K[pos]
	}
	return k, true
}

// EvalMemory evaluates a plan against in-memory cells and mirror facts. It is
// the planner's semantics without SQL; tests compare it with the reference,
// and the SQL evaluator is tested against it.
func (pl *Plan) EvalMemory(cells map[engine.CellKey][]int64, facts []*engine.Fact, loc *time.Location) Result {
	out := Result{}
	if pl.Rollup == nil {
		return out
	}
	q := pl.Query
	for ck, v := range cells {
		if ck.Rollup != pl.Rollup.ID || !inTenants(ck.Tenant, q.Tenants) {
			continue
		}
		b := time.Unix(ck.Bucket, 0)
		for _, s := range pl.Segments {
			if ck.Grain == s.Grain && !b.Before(s.From) && b.Before(s.To) {
				if k, ok := pl.KeyOfCell(ck, loc); ok {
					out.Add(k, v, 1)
				}
				break
			}
		}
	}
	for _, f := range facts {
		if !inTenants(f.Tenant, q.Tenants) {
			continue
		}
		for _, e := range pl.Edges {
			if !f.Time.Before(e.From) && f.Time.Before(e.To) {
				if k, ok := pl.KeyOfFact(f, loc); ok {
					out.Add(k, f.Measures, 1)
				}
				break
			}
		}
	}
	return out.DropZeros()
}
