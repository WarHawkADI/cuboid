package pipeline

import (
	"sync"
	"time"

	"cuboid/internal/engine"
)

// Ring keeps the raw changes of recently applied transactions, so a fenced
// verification can reconcile transactions whose commit order differs from
// their visibility to the direct query's snapshot.
type Ring struct {
	mu   sync.Mutex
	keep time.Duration
	txns []ringEntry
}

type ringEntry struct {
	at  time.Time
	txn *engine.Txn
}

// NewRing keeps transactions for the given duration.
func NewRing(keep time.Duration) *Ring { return &Ring{keep: keep} }

// Add records a transaction.
func (r *Ring) Add(t *engine.Txn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.txns = append(r.txns, ringEntry{at: now, txn: t})
	cut := 0
	for cut < len(r.txns) && now.Sub(r.txns[cut].at) > r.keep {
		cut++
	}
	if cut > 0 {
		r.txns = append([]ringEntry(nil), r.txns[cut:]...)
	}
}

// Between returns transactions with commit LSN in (lo, hi], and whether the
// ring still covers everything after lo.
func (r *Ring) Between(lo, hi uint64) ([]*engine.Txn, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	covered := len(r.txns) == 0 || r.txns[0].txn.CommitLSN <= lo || time.Since(r.txns[0].at) < r.keep
	var out []*engine.Txn
	for _, e := range r.txns {
		if e.txn.CommitLSN > lo && e.txn.CommitLSN <= hi {
			out = append(out, e.txn)
		}
	}
	return out, covered
}

// Recent returns transactions committed within the last d.
func (r *Ring) Recent(d time.Duration) []*engine.Txn {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*engine.Txn
	for _, e := range r.txns {
		if time.Since(e.at) <= d {
			out = append(out, e.txn)
		}
	}
	return out
}
