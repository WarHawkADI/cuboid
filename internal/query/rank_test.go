package query

import (
	"sort"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// fakeRank holds rows by store, key and day, with months of 30 days, and
// answers what aggRank answers for a range of days [from, to): whole months
// from their cells, the rest from signed slices of rows.
type fakeRank struct {
	rows     map[rankKey]map[int]int64 // key -> day -> value
	from, to int
	whole    []int    // months taken from cells
	slices   [][3]int // day from, day to, sign
}

func (f *fakeRank) month(k rankKey, m int) int64 {
	var s int64
	for d, v := range f.rows[k] {
		if d/30 == m {
			s += v
		}
	}
	return s
}

func (f *fakeRank) sliceTotals() (map[rankKey]int64, error) {
	out := map[rankKey]int64{}
	for k, days := range f.rows {
		for d, v := range days {
			for _, s := range f.slices {
				if d >= s[0] && d < s[1] {
					out[k] += int64(s[2]) * v
				}
			}
		}
	}
	return out, nil
}

func (f *fakeRank) lists(depth int) ([]listRow, error) {
	tenants := map[int32]bool{}
	for k := range f.rows {
		tenants[k.tenant] = true
	}
	var out []listRow
	for t := range tenants {
		for i, m := range f.whole {
			var ks []rankKey
			for k := range f.rows {
				if k.tenant == t && f.month(k, m) > 0 {
					ks = append(ks, k)
				}
			}
			// Ties in any order: the database promises none.
			sort.Slice(ks, func(a, b int) bool { return f.month(ks[a], m) > f.month(ks[b], m) })
			if len(ks) > depth {
				ks = ks[:depth]
			}
			for _, k := range ks {
				out = append(out, listRow{k, i, f.month(k, m)})
			}
		}
	}
	return out, nil
}

func (f *fakeRank) cells(keys []rankKey) (map[rankKey]int64, error) {
	out := map[rankKey]int64{}
	for _, k := range keys {
		for _, m := range f.whole {
			out[k] += f.month(k, m)
		}
	}
	return out, nil
}

// brute ranks every key over the range, as the full query does.
func (f *fakeRank) brute(n int) []rankRow {
	var rows []rankRow
	for k, days := range f.rows {
		var s int64
		for d, v := range days {
			if d >= f.from && d < f.to {
				s += v
			}
		}
		if s != 0 {
			rows = append(rows, rankRow{k, s})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.v != b.v {
			return a.v > b.v
		}
		if a.tenant != b.tenant {
			return a.tenant < b.tenant
		}
		return a.key < b.key
	})
	if len(rows) > n {
		rows = rows[:n]
	}
	return rows
}

// split chooses, month by month, cells or rows: any choice must be exact.
func (f *fakeRank) split(t *rapid.T) {
	for m := f.from / 30; m*30 < f.to; m++ {
		ms, me := m*30, m*30+30
		lo, hi := max(ms, f.from), min(me, f.to)
		if rapid.Bool().Draw(t, "whole") {
			f.whole = append(f.whole, m)
			if lo > ms {
				f.slices = append(f.slices, [3]int{ms, lo, -1})
			}
			if hi < me {
				f.slices = append(f.slices, [3]int{hi, me, -1})
			}
		} else {
			f.slices = append(f.slices, [3]int{lo, hi, 1})
		}
	}
}

// Whatever the split, the search answers exactly what ranking every key
// answers: same keys, same values, same order, ties included.
func TestTopByCellsEqualsFullTop(t *testing.T) {
	var runs, answered int
	defer func() {
		t.Logf("the search answered %d of %d cases", answered, runs)
		if answered*2 < runs {
			t.Errorf("the search answered %d of %d cases: too few to test it", answered, runs)
		}
	}()
	rapid.Check(t, func(t *rapid.T) {
		runs++
		f := &fakeRank{rows: map[rankKey]map[int]int64{}}
		tenants := rapid.IntRange(1, 3).Draw(t, "tenants")
		keys := rapid.IntRange(0, 60).Draw(t, "keys")
		// Few distinct values, so ties at the cut are common.
		maxV := rapid.Int64Range(0, 6).Draw(t, "maxValue")
		for i := rapid.IntRange(0, 400).Draw(t, "rows"); i > 0; i-- {
			k := rankKey{int32(rapid.IntRange(1, tenants).Draw(t, "tenant")), int64(rapid.IntRange(0, keys).Draw(t, "key"))}
			if f.rows[k] == nil {
				f.rows[k] = map[int]int64{}
			}
			f.rows[k][rapid.IntRange(0, 149).Draw(t, "day")] += rapid.Int64Range(0, maxV).Draw(t, "value")
		}
		f.from = rapid.IntRange(0, 148).Draw(t, "from")
		f.to = rapid.IntRange(f.from+1, 150).Draw(t, "to")
		f.split(t)
		n := rapid.IntRange(1, 12).Draw(t, "n")

		got, info, ok, err := topByCells(f, n, len(f.whole), tenants)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return // the caller ranks every key
		}
		answered++
		want := f.brute(n)
		if len(got) != len(want) {
			t.Fatalf("got %d rows, want %d: %v vs %v", len(got), len(want), got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("row %d: got %v, want %v (%+v)", i, got[i], want[i], info)
			}
		}
	})
}

// The months taken whole, less the slices that remove, plus the slices that
// add, cover the range exactly once and nothing outside it.
func TestSplitRangeTilesTheRange(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, ist)
	rapid.Check(t, func(t *rapid.T) {
		at := func(label string) time.Time {
			return base.Add(time.Duration(rapid.Int64Range(0, 400*24*3600).Draw(t, label)) * time.Second)
		}
		from, to, now := at("from"), at("to"), at("now")
		if !from.Before(to) {
			from, to = to, from.Add(time.Second)
		}
		months, slices := splitRange(from, to, now, ist)
		for i := 0; i < 200; i++ {
			x := at("x")
			want := 0
			if !x.Before(from) && x.Before(to) {
				want = 1
			}
			got := 0
			for _, m := range months {
				if !x.Before(m) && x.Before(m.AddDate(0, 1, 0)) {
					got++
				}
			}
			for _, s := range slices {
				if !x.Before(s.from) && x.Before(s.to) {
					got += int(s.sign)
				}
			}
			if got != want {
				t.Fatalf("%s in [%s, %s): counted %d times, months %v, slices %v", x, from, to, got, months, slices)
			}
		}
	})
}

// last_30d: October's cell less a day of rows, and November's cell less the
// rows after now, which do not exist yet.
func TestLast30DaysIsTwoCellsAndADay(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 23, 28, 0, ist)
	months, slices := splitRange(now.AddDate(0, 0, -30), now, now, ist)
	if len(months) != 2 || !months[0].Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, ist)) {
		t.Fatalf("months %v, want October and November", months)
	}
	if len(slices) != 2 || slices[0].sign != -1 || slices[0].to.Sub(slices[0].from) > 25*time.Hour || !slices[1].from.Equal(now) {
		t.Fatalf("slices %+v, want a day of October and the rest of November, both removed", slices)
	}
}

// A walk must stay shallow when a few keys stand out among many, or it is
// only overhead.
func TestTopByCellsReadsFewKeys(t *testing.T) {
	f := &fakeRank{rows: map[rankKey]map[int]int64{}, from: 32, to: 62, whole: []int{1, 2}, slices: [][3]int{{30, 32, -1}, {62, 90, -1}}}
	for i := int64(1); i <= 5000; i++ {
		f.rows[rankKey{1, i}] = map[int]int64{int(30 + i%60): 1 + i%3}
	}
	for i := int64(1); i <= 20; i++ {
		f.rows[rankKey{1, 10000 + i}] = map[int]int64{40: 100 * i, 61: 5}
	}
	got, info, ok, err := topByCells(f, 10, len(f.whole), 1)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	want := f.brute(10)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d: got %v, want %v", i, got[i], want[i])
		}
	}
	if info.KeysRead > 100 {
		t.Errorf("read %d keys of 5020; a few dozen should do", info.KeysRead)
	}
}
