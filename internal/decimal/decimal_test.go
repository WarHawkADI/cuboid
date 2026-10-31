package decimal

import "testing"

func TestFormat(t *testing.T) {
	cases := []struct {
		v     int64
		scale int
		want  string
	}{{1843200, 2, "18432.00"}, {-5, 2, "-0.05"}, {0, 2, "0.00"}, {7, 0, "7"}, {100, 2, "1.00"}}
	for _, c := range cases {
		if got := Format(c.v, c.scale); got != c.want {
			t.Errorf("Format(%d, %d) = %s, want %s", c.v, c.scale, got, c.want)
		}
	}
}

func TestDivHalfUpRoundsAwayFromZero(t *testing.T) {
	cases := []struct{ num, den, want int64 }{{5, 2, 3}, {-5, 2, -3}, {4, 3, 1}, {5, 3, 2}, {1, 2, 1}, {-1, 2, -1}, {10, 5, 2}}
	for _, c := range cases {
		if got := DivHalfUp(c.num, c.den); got != c.want {
			t.Errorf("DivHalfUp(%d, %d) = %d, want %d", c.num, c.den, got, c.want)
		}
	}
}

func TestParseRefusesRatherThanRounds(t *testing.T) {
	if v, err := Parse("12.34", 2); err != nil || v != 1234 {
		t.Errorf("12.34: %d, %v", v, err)
	}
	if _, err := Parse("12.345", 2); err == nil {
		t.Error("an extra fractional digit must be refused: rounding would break exactness")
	}
	if _, err := Parse("twelve", 2); err == nil {
		t.Error("text must be refused")
	}
}

func FuzzFormatParseRoundTrip(f *testing.F) {
	f.Add(int64(1843200))
	f.Add(int64(-5))
	f.Fuzz(func(t *testing.T, v int64) {
		if v == -1<<63 {
			return
		}
		got, err := Parse(Format(v, 2), 2)
		if err != nil || got != v {
			t.Fatalf("%d came back as %d, %v", v, got, err)
		}
	})
}
