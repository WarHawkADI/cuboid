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
		// A rank rollup bounds one key's month totals; anything else is no bound.
		{"rank with two dims", "dims: [customer_id],         grains: {month: all}, rank: true", "dims: [status, customer_id], grains: {month: all}, rank: true"},
		{"rank on a join", "dims: [customer_id],         grains: {month: all}, rank: true", "dims: [city],         grains: {month: all}, rank: true"},
		{"rank on text", "dims: [customer_id],         grains: {month: all}, rank: true", "dims: [status],         grains: {month: all}, rank: true"},
		{"rank with days", "dims: [customer_id],         grains: {month: all}, rank: true", "dims: [customer_id],         grains: {day: all, month: all}, rank: true"},
		{"rank with short months", "dims: [customer_id],         grains: {month: all}, rank: true", "dims: [customer_id],         grains: {month: 90d}, rank: true"},
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

// The main schema's own CHECK definitions, as pg_get_constraintdef prints them.
func TestNonnegativeNeedsACheck(t *testing.T) {
	cases := []struct {
		def, col string
		want     bool
	}{
		{"CHECK ((amount_paise >= 0))", "amount_paise", true},
		{"CHECK ((quantity > 0))", "quantity", true},
		{"CHECK (((risk_score >= 0) AND (risk_score <= 100)))", "risk_score", true},
		{`CHECK (("Amount" >= (0)::numeric))`, "Amount", true},
		{"CHECK ((amount_paise >= 0))", "quantity", false},
		{"CHECK ((amount_paise >= '-5'::integer))", "amount_paise", false},
		{"CHECK (((amount_paise >= 0) OR (refund IS TRUE)))", "amount_paise", false},
		{"CHECK ((amount_paise >= 0)) NOT VALID", "amount_paise", false},
		{"CHECK ((amount_paise <= 100))", "amount_paise", false},
	}
	for _, c := range cases {
		if got := GuaranteesNonnegative(c.def, c.col); got != c.want {
			t.Errorf("%s on %s: %v, want %v", c.def, c.col, got, c.want)
		}
	}
	cfg, err := Parse([]byte(shipped(t)))
	if err != nil {
		t.Fatal(err)
	}
	main := map[string][]string{"orders": {"CHECK ((quantity > 0))", "CHECK ((amount_paise >= 0))"}}
	if err := cfg.ValidateNonnegative(main); err != nil {
		t.Errorf("the main schema's checks were refused: %v", err)
	}
	if err := cfg.ValidateNonnegative(map[string][]string{"orders": {"CHECK ((quantity > 0))"}}); err == nil {
		t.Error("sales was declared nonnegative without a CHECK, and was accepted")
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
