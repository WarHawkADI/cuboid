package limiter

import (
	"context"
	"errors"
	"testing"
)

func testConfig() Config {
	c := Defaults
	c.HeavyPerTenant, c.LightPerTenant, c.GlobalConcurrency, c.WaitMS = 1, 2, 8, 5
	return c
}

func TestHeavyLimitIsPerStore(t *testing.T) {
	l := New(testConfig())
	ctx := context.Background()
	release, err := l.Acquire(ctx, "store:1", true)
	if err != nil {
		t.Fatal(err)
	}
	var lim *ErrLimited
	if _, err := l.Acquire(ctx, "store:1", true); !errors.As(err, &lim) || !lim.Heavy {
		t.Fatalf("second heavy query from store 1 should be limited, got %v", err)
	}
	r2, err := l.Acquire(ctx, "store:2", true)
	if err != nil {
		t.Fatalf("store 2 must not notice store 1: %v", err)
	}
	r2()
	r3, err := l.Acquire(ctx, "store:1", false)
	if err != nil {
		t.Fatalf("store 1's light queries are a separate pool: %v", err)
	}
	r3()
	release()
	r4, err := l.Acquire(ctx, "store:1", true)
	if err != nil {
		t.Fatalf("released slot was not returned: %v", err)
	}
	r4()
}

// Regression: a light pool above the global cap let one store hold every slot,
// and others' p99 reached 273 ms under a flood.
func TestShippedLimitsCannotLetOneStoreTakeEverySlot(t *testing.T) {
	cfg, err := LoadConfig("../../config/limits.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HeavyPerTenant+cfg.LightPerTenant >= cfg.GlobalConcurrency {
		t.Errorf("one store can hold %d of %d slots", cfg.HeavyPerTenant+cfg.LightPerTenant, cfg.GlobalConcurrency)
	}
}

func TestKeyRateLimits(t *testing.T) {
	c := testConfig()
	c.KeyRate, c.KeyBurst = 1, 2
	l := New(c)
	if !l.AllowKey("k") || !l.AllowKey("k") {
		t.Fatal("the burst should pass")
	}
	if l.AllowKey("k") {
		t.Error("beyond the burst should be limited")
	}
	if !l.AllowKey("other") {
		t.Error("another key must not share the bucket")
	}
}

func TestBusyAtHalfTheStoreSlots(t *testing.T) {
	c := testConfig()
	c.GlobalConcurrency = 2
	l := New(c)
	if l.Busy() {
		t.Fatal("busy while idle")
	}
	release, err := l.Acquire(context.Background(), "store:1", false)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Busy() {
		t.Error("one of two slots in use should count as busy")
	}
	release()
	if l.Busy() {
		t.Error("still busy after release")
	}
}
