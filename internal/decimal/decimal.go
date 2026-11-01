// Package decimal renders exact scaled integers. Money is stored as integer
// minor units (paise) and never passes through a float.
package decimal

import (
	"fmt"
	"strconv"
	"strings"
)

// Format renders v / 10^scale with exactly scale decimal places, matching
// Postgres numeric output for a value of that scale ("18432.00", "-0.05").
func Format(v int64, scale int) string {
	if scale <= 0 {
		return strconv.FormatInt(v, 10)
	}
	neg := v < 0
	u := uint64(v)
	if neg {
		u = uint64(-v)
	}
	s := strconv.FormatUint(u, 10)
	if len(s) <= scale {
		s = strings.Repeat("0", scale-len(s)+1) + s
	}
	out := s[:len(s)-scale] + "." + s[len(s)-scale:]
	if neg {
		out = "-" + out
	}
	return out
}

// DivHalfUp returns num/den rounded half away from zero, as an integer.
// Used for derived ratios such as average order value in paise.
func DivHalfUp(num, den int64) int64 {
	if den == 0 {
		return 0
	}
	q, r := num/den, num%den
	if r < 0 {
		r = -r
	}
	d := den
	if d < 0 {
		d = -d
	}
	if 2*r >= d {
		if (num < 0) != (den < 0) {
			q--
		} else {
			q++
		}
	}
	return q
}

// Parse reads a decimal string ("1234", "1234.5", "-0.25") into an integer
// scaled by 10^scale. It rejects values with more fractional digits than the
// scale rather than rounding, because a silent rounding breaks exactness.
func Parse(s string, scale int) (int64, error) {
	s = strings.TrimSpace(s)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
	intPart, frac, _ := strings.Cut(s, ".")
	frac = strings.TrimRight(frac, "0")
	if len(frac) > scale {
		return 0, fmt.Errorf("value %q has more than %d decimal places", s, scale)
	}
	frac += strings.Repeat("0", scale-len(frac))
	if intPart == "" {
		intPart = "0"
	}
	v, err := strconv.ParseInt(intPart+frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("value %q is not a decimal: %w", s, err)
	}
	if neg {
		v = -v
	}
	return v, nil
}
