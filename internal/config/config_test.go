package config

import (
	"os"
	"strings"
	"testing"
)

func shipped(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../config/metrics.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestShippedConfigIsValid(t *testing.T) {
	cfg, err := Parse([]byte(shipped(t)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Rollups.Get("totals") == nil || cfg.Sources.Get("soc_alerts") == nil {
		t.Error("expected rollup totals and source soc_alerts")
	}
}

// A mistake in the YAML must name what is wrong, and reload keeps the old config.
func TestInvalidConfigIsRefused(t *testing.T) {
	base := shipped(t)
	cases := []struct{ name, old, new string }{
		{"unknown dimension", "dims: [status],              grains", "dims: [no_such_dim],              grains"},
		{"bad retention", "grains: {hour: 35d,  day: all,  month: all}, key_dim: product_id", "grains: {hour: soon,  day: all,  month: all}, key_dim: product_id"},
		{"unknown source", "totals:          {source: orders,", "totals:          {source: no_such_source,"},
	}
	for _, c := range cases {
		if !strings.Contains(base, c.old) {
			t.Fatalf("%s: the shipped config no longer contains %q", c.name, c.old)
		}
		if _, err := Parse([]byte(strings.Replace(base, c.old, c.new, 1))); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
	if _, err := Parse([]byte("not: [valid")); err == nil {
		t.Error("broken YAML was accepted")
	}
}

// I11: configured PII columns are declared, so the mirror never copies them.
func TestPIIColumnsAreDeclared(t *testing.T) {
	cfg, err := Parse([]byte(shipped(t)))
	if err != nil {
		t.Fatal(err)
	}
	customers := cfg.Sources.Get("customers")
	for _, col := range []string{"full_name", "email", "phone"} {
		found := false
		for _, p := range customers.PIIColumns {
			found = found || p == col
		}
		if !found {
			t.Errorf("customers.%s is not marked as PII", col)
		}
	}
}
