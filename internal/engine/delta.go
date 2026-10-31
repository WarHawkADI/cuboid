package engine

import (
	"time"

	"cuboid/internal/config"
	"cuboid/internal/timeutil"
)

// CellKey addresses one rollup cell. Key values are raw dimension values;
// the flusher maps text values to dictionary ids.
type CellKey struct {
	Tenant int32
	Rollup int16
	Grain  timeutil.Grain
	Bucket int64 // bucket start, Unix seconds
	K      [config.MaxDims]string
}

// MirrorOp is the latest state of one source row within a flush window:
// an upsert of Row, or a delete when Row is nil.
type MirrorOp struct {
	Tenant int32
	Row    Row
	Old    Row
}

// Touch records which time range of a tenant's data changed, so cached
// answers for other ranges stay valid (range-aware cache validity).
type Touch struct {
	Min, Max time.Time
	All      bool // labels or membership changed: every range is affected
}

// Delta accumulates the coalesced effect of many transactions until a flush.
type Delta struct {
	Cells            map[CellKey][]int64
	Mirror           map[string]map[string]*MirrorOp // table -> row key -> op
	Touched          map[int32]*Touch
	DimRebuild       map[int32]map[string]bool // tenant -> dimension tables whose join columns changed
	Events           []SecurityEvent
	FutureTimestamps int
	Txns             int
}

// NewDelta returns an empty delta.
func NewDelta() *Delta {
	return &Delta{
		Cells:      map[CellKey][]int64{},
		Mirror:     map[string]map[string]*MirrorOp{},
		Touched:    map[int32]*Touch{},
		DimRebuild: map[int32]map[string]bool{},
	}
}

// Empty reports whether the delta holds nothing to write.
func (d *Delta) Empty() bool {
	return len(d.Cells) == 0 && len(d.Mirror) == 0 && len(d.DimRebuild) == 0
}

func (d *Delta) add(k CellKey, m []int64, sign int64) {
	v, ok := d.Cells[k]
	if !ok {
		v = make([]int64, len(m))
		d.Cells[k] = v
	}
	for i := range m {
		v[i] += sign * m[i]
	}
}

func (d *Delta) mirrorOp(table, key string, op *MirrorOp) {
	t := d.Mirror[table]
	if t == nil {
		t = map[string]*MirrorOp{}
		d.Mirror[table] = t
	}
	t[key] = op
}

func (d *Delta) touch(tenant int32, ts time.Time) {
	t := d.Touched[tenant]
	if t == nil {
		d.Touched[tenant] = &Touch{Min: ts, Max: ts}
		return
	}
	if ts.Before(t.Min) {
		t.Min = ts
	}
	if ts.After(t.Max) {
		t.Max = ts
	}
}

func (d *Delta) touchAll(tenant int32) {
	t := d.Touched[tenant]
	if t == nil {
		t = &Touch{}
		d.Touched[tenant] = t
	}
	t.All = true
}

func (d *Delta) markDimRebuild(tenant int32, table string) {
	m := d.DimRebuild[tenant]
	if m == nil {
		m = map[string]bool{}
		d.DimRebuild[tenant] = m
	}
	m[table] = true
	d.touchAll(tenant)
}

// NonZeroCells returns the cells whose delta is not all zeros. An update that
// changes no contributing column nets to zero and writes nothing.
func (d *Delta) NonZeroCells() map[CellKey][]int64 {
	out := make(map[CellKey][]int64, len(d.Cells))
	for k, v := range d.Cells {
		for _, x := range v {
			if x != 0 {
				out[k] = v
				break
			}
		}
	}
	return out
}

// Merge adds other into d (used to log deltas during an online rebuild).
func (d *Delta) MergeCells(cells map[CellKey][]int64) {
	for k, v := range cells {
		d.add(k, v, 1)
	}
}
