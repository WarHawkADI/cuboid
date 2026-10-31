// Command contrast computes WCAG contrast ratios for the UI's colour tokens
// and fails if any text or mark pair is below its minimum.
package main

import (
	"fmt"
	"math"
	"os"
	"strconv"
)

var tokens = map[string]string{
	"canvas": "#EEF1F4", "paper": "#FFFFFF", "ink": "#16202A", "slate": "#56616E", "rule": "#D6DCE3", "grid": "#BAC3CD",
	"pen": "#2846C4", "umber": "#8A5A2B", "limit": "#C8222B", "amber": "#A86400", "verified": "#1D7A47",
}

type pair struct {
	fg, bg string
	min    float64
	use    string
}

var pairs = []pair{
	{"ink", "paper", 4.5, "primary text"}, {"ink", "canvas", 4.5, "primary text on canvas"},
	{"slate", "paper", 4.5, "secondary text"}, {"slate", "canvas", 4.5, "secondary text on canvas"},
	{"pen", "paper", 4.5, "Cuboid traces and links"}, {"pen", "canvas", 4.5, "links on canvas"},
	{"paper", "pen", 4.5, "text on primary buttons"},
	{"umber", "paper", 4.5, "Direct path label"}, {"limit", "paper", 4.5, "breach and blocked text"},
	{"amber", "paper", 4.5, "stale and limited text"}, {"verified", "paper", 4.5, "exact and Great text"},
}

func lum(hex string) float64 {
	ch := func(s string) float64 {
		v, _ := strconv.ParseInt(s, 16, 32)
		c := float64(v) / 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(hex[1:3]) + 0.7152*ch(hex[3:5]) + 0.0722*ch(hex[5:7])
}

func ratio(a, b string) float64 {
	x, y := lum(a), lum(b)
	if x < y {
		x, y = y, x
	}
	return (x + 0.05) / (y + 0.05)
}

func main() {
	failed := false
	fmt.Println("| Foreground | Background | Ratio | Minimum | Use |\n|---|---|---|---|---|")
	for _, p := range pairs {
		r := ratio(tokens[p.fg], tokens[p.bg])
		mark := ""
		if r < p.min {
			mark, failed = " FAIL", true
		}
		fmt.Printf("| %s | %s | %.2f%s | %.1f | %s |\n", p.fg, p.bg, r, mark, p.min, p.use)
	}
	if failed {
		os.Exit(1)
	}
}
