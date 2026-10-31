package cache

import (
	"testing"
	"time"

	"cuboid/internal/engine"
)

func entry(versions map[int32]int64, from, to time.Time) *Entry {
	return &Entry{Versions: versions, From: from, To: to, Value: "answer", Created: time.Now()}
}

func TestPartitionsDoNotShare(t *testing.T) {
	c := New(4, NewHistory(16))
	c.Put("store:1", "q", entry(nil, time.Time{}, time.Time{}))
	if _, ok := c.Get("store:2", "q"); ok {
		t.Fatal("store 2 read store 1's entry through the same query key")
	}
	if _, ok := c.Get("store:1", "q"); !ok {
		t.Fatal("store 1 lost its own entry")
	}
}

func TestOnePartitionCannotEvictAnother(t *testing.T) {
	c := New(2, NewHistory(16))
	c.Put("store:1", "keep", entry(nil, time.Time{}, time.Time{}))
	for _, k := range []string{"a", "b", "c", "d"} {
		c.Put("store:2", k, entry(nil, time.Time{}, time.Time{}))
	}
	if _, ok := c.Get("store:1", "keep"); !ok {
		t.Fatal("store 2's churn evicted store 1's entry")
	}
}

func TestNewVersionOutsideTheRangeKeepsTheEntry(t *testing.T) {
	h := NewHistory(16)
	c := New(4, h)
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	c.Put("store:1", "january", entry(map[int32]int64{1: h.Version(1)}, from, to))

	h.Invalidate([]int32{2})
	if _, ok := c.Get("store:1", "january"); !ok {
		t.Fatal("another store's change invalidated this entry")
	}
	h.Invalidate([]int32{1})
	if _, ok := c.Get("store:1", "january"); ok {
		t.Fatal("a change to every range of this store should invalidate")
	}
}

func TestExpiredEntryIsInvalidButPeekable(t *testing.T) {
	c := New(4, NewHistory(16))
	e := entry(nil, time.Time{}, time.Time{})
	e.Expires = time.Now().Add(-time.Second)
	c.Put("store:1", "moving", e)
	if _, ok := c.Get("store:1", "moving"); ok {
		t.Error("an expired moving preset must not be served as fresh")
	}
	if _, ok := c.Peek("store:1", "moving"); !ok {
		t.Error("stale fallback needs Peek to still see it")
	}
}

func TestRecordKeepsHistoryBounded(t *testing.T) {
	h := NewHistory(2)
	for v := int64(1); v <= 5; v++ {
		h.Record(map[int32]int64{1: v}, map[int32]*engine.Touch{1: {All: true}})
	}
	if h.Version(1) != 5 {
		t.Errorf("version %d, want 5", h.Version(1))
	}
	if n := len(h.byT[1].changes); n != 2 {
		t.Errorf("kept %d changes, want 2", n)
	}
}
