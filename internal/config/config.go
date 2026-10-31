// Package config loads and validates config/metrics.yaml: the source tables,
// the tenant and company model, measures, dimensions and rollups. The engine
// is generic; "orders" is one configuration of it.
package config

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"cuboid/internal/timeutil"
)

// Config is a validated metrics configuration.
type Config struct {
	Path     string            `yaml:"-"`
	Timezone string            `yaml:"timezone"`
	Tenancy  Tenancy           `yaml:"tenancy"`
	Sources  Ordered[*Source]  `yaml:"sources"`
	Rollups  Ordered[*Rollup]  `yaml:"rollups"`
	Presets  []string          `yaml:"-"`
	byID     map[int16]*Rollup `yaml:"-"`
}

// Tenancy says how rows map to tenants (stores) and tenants to companies.
type Tenancy struct {
	TenantColumn string      `yaml:"tenant_column"`
	Company      *CompanyRef `yaml:"company"`
}

// CompanyRef names the table that maps each tenant to its company.
type CompanyRef struct {
	Table      string `yaml:"table"`
	Key        string `yaml:"key"`
	Column     string `yaml:"column"`
	NameColumn string `yaml:"name_column"`
}

// Source is a published table. A source with a time column is a fact source
// that rollups aggregate; the others are mirrored for joins and labels.
type Source struct {
	Name         string              `yaml:"-"`
	Line         int                 `yaml:"-"`
	Key          []string            `yaml:"key"`
	Time         string              `yaml:"time"`
	PIIColumns   []string            `yaml:"pii_columns"`
	CountMeasure string              `yaml:"count_measure"`
	Measures     Ordered[*Measure]   `yaml:"measures"`
	Derived      Ordered[*Derived]   `yaml:"derived"`
	Dimensions   Ordered[*Dimension] `yaml:"dimensions"`
	Top          Ordered[*TopList]   `yaml:"top"`
}

// Measure is an exact, retractable aggregate.
type Measure struct {
	Name       string `yaml:"-"`
	Line       int    `yaml:"-"`
	Type       string `yaml:"type"` // count | sum
	Column     string `yaml:"column"`
	Scale      int    `yaml:"scale"`       // decimal places when rendered
	MinorUnits bool   `yaml:"minor_units"` // the column already holds integer minor units
	Unit       string `yaml:"unit"`
}

// Derived is a ratio of two measures computed at read time.
type Derived struct {
	Name  string `yaml:"-"`
	Line  int    `yaml:"-"`
	Expr  string `yaml:"expr"`
	Round string `yaml:"round"`
	Num   string `yaml:"-"`
	Den   string `yaml:"-"`
}

// Dimension is something a query can group or filter by.
type Dimension struct {
	Name   string `yaml:"-"`
	Line   int    `yaml:"-"`
	Column string `yaml:"column"`
	Type   string `yaml:"type"` // "" (text, dictionary-encoded) | key (integer id used as-is)
	Join   *Join  `yaml:"join"`
	Label  *Label `yaml:"label"`
}

// IsKey reports whether the dimension's values are integer ids stored as-is.
func (d *Dimension) IsKey() bool { return d.Type == "key" }

// Join resolves a dimension through a mirrored dimension table.
type Join struct {
	Table  string `yaml:"table"`
	On     string `yaml:"on"`
	Column string `yaml:"column"`
}

// Label names the mirrored column that labels a key dimension.
type Label struct {
	Table  string `yaml:"table"`
	Column string `yaml:"column"`
}

// TopList maps an API entity (products, customers, cities) to a dimension.
type TopList struct {
	Name string `yaml:"-"`
	Line int    `yaml:"-"`
	Dim  string `yaml:"dim"`
}

// Rollup is one pre-computed summary: totals grouped by Dims per bucket.
type Rollup struct {
	Name      string            `yaml:"-"`
	Line      int               `yaml:"-"`
	ID        int16             `yaml:"-"`
	Source    string            `yaml:"source"`
	Dims      []string          `yaml:"dims"`
	GrainSpec map[string]string `yaml:"grains"`
	KeyDim    string            `yaml:"key_dim"`
	// Retention per grain; zero means the grain is kept for all time.
	Retention map[timeutil.Grain]time.Duration `yaml:"-"`
}

// MaxDims is the number of key columns in a rollup cell.
const MaxDims = 3

// HasGrain reports whether the rollup keeps cells at grain g.
func (r *Rollup) HasGrain(g timeutil.Grain) bool {
	_, ok := r.Retention[g]
	return ok
}

// Since returns the earliest bucket start the rollup holds at grain g, given
// now. Buckets at or after it are complete; the planner adds its own margin.
func (r *Rollup) Since(g timeutil.Grain, now time.Time, loc *time.Location) time.Time {
	d := r.Retention[g]
	if d == 0 {
		return time.Time{}
	}
	return timeutil.Trunc(now.Add(-d), g, loc)
}

// DimIndex returns the key position of dim in the rollup, or -1.
func (r *Rollup) DimIndex(dim string) int {
	for i, d := range r.Dims {
		if d == dim {
			return i
		}
	}
	return -1
}

// Load reads and validates a metrics file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	cfg, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.Path = path
	return cfg, nil
}

// Parse decodes and validates a metrics document.
func Parse(b []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg.AssignIDs(nil)
	return &cfg, nil
}

// AssignIDs gives every rollup a stable numeric id. Ids already recorded in
// the aggregate store are kept; new rollups get the next free id.
func (c *Config) AssignIDs(existing map[string]int16) {
	used := map[int16]bool{}
	var next int16 = 1
	for _, id := range existing {
		used[id] = true
		if id >= next {
			next = id + 1
		}
	}
	c.byID = map[int16]*Rollup{}
	names := c.Rollups.Keys()
	sort.Strings(names)
	for _, name := range names {
		r := c.Rollups.Get(name)
		if id, ok := existing[name]; ok {
			r.ID = id
		} else {
			for used[next] {
				next++
			}
			r.ID = next
			used[next] = true
			next++
		}
		c.byID[r.ID] = r
	}
}

// RollupByID returns the rollup with the given id.
func (c *Config) RollupByID(id int16) *Rollup { return c.byID[id] }

// FactSources returns the sources that have a time column.
func (c *Config) FactSources() []*Source {
	var out []*Source
	for _, s := range c.Sources.Values() {
		if s.Time != "" {
			out = append(out, s)
		}
	}
	return out
}

// RollupsFor returns the rollups over a source, in declared order.
func (c *Config) RollupsFor(source string) []*Rollup {
	var out []*Rollup
	for _, r := range c.Rollups.Values() {
		if r.Source == source {
			out = append(out, r)
		}
	}
	return out
}

// CellTable is the aggregate-store table holding a fact source's rollup cells.
func CellTable(source string) string { return "rollup_cells_" + source }

// MirrorTable is the aggregate-store table mirroring a source table.
func MirrorTable(source string) string { return "m_" + source }

// IsPII reports whether a column of the source is excluded from the mirror.
func (s *Source) IsPII(col string) bool {
	for _, p := range s.PIIColumns {
		if p == col {
			return true
		}
	}
	return false
}

// MeasureIndex returns the position of a measure in the cell vector, or -1.
func (s *Source) MeasureIndex(name string) int {
	for i, k := range s.Measures.Keys() {
		if k == name {
			return i
		}
	}
	return -1
}

// JoinTables returns the dimension tables this source's join dimensions use.
func (s *Source) JoinTables() []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range s.Dimensions.Values() {
		if d.Join != nil && !seen[d.Join.Table] {
			seen[d.Join.Table] = true
			out = append(out, d.Join.Table)
		}
	}
	return out
}

func (c *Config) validate() error {
	if c.Tenancy.TenantColumn == "" {
		c.Tenancy.TenantColumn = "tenant_id"
	}
	if c.Timezone == "" {
		c.Timezone = "from_main_db"
	}
	if c.Timezone != "from_main_db" {
		if _, err := time.LoadLocation(c.Timezone); err != nil {
			return fmt.Errorf("timezone: %w", err)
		}
	}
	if c.Sources.Len() == 0 {
		return fmt.Errorf("sources: at least one source is required")
	}
	if tc := c.Tenancy.Company; tc != nil {
		if c.Sources.Get(tc.Table) == nil {
			return fmt.Errorf("tenancy.company.table %q is not a source; add it under sources so it is mirrored", tc.Table)
		}
	}
	for _, s := range c.Sources.Values() {
		if err := c.validateSource(s); err != nil {
			return err
		}
	}
	if len(c.FactSources()) == 0 {
		return fmt.Errorf("sources: no source has a time column, so there is nothing to aggregate")
	}
	for _, r := range c.Rollups.Values() {
		if err := c.validateRollup(r); err != nil {
			return err
		}
	}
	for _, s := range c.FactSources() {
		if err := c.validateCoverage(s); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) validateSource(s *Source) error {
	at := func(line int, format string, args ...any) error {
		return fmt.Errorf("line %d: source %s: %s", line, s.Name, fmt.Sprintf(format, args...))
	}
	if len(s.Key) == 0 {
		return at(s.Line, "key is required (for example [tenant_id, order_id])")
	}
	if s.Time == "" {
		if s.Measures.Len() > 0 || s.Dimensions.Len() > 0 {
			return at(s.Line, "measures and dimensions need a time column; add time:")
		}
		return nil
	}
	if s.IsPII(s.Time) {
		return at(s.Line, "the time column %q is listed as PII", s.Time)
	}
	if s.Measures.Len() == 0 {
		return at(s.Line, "a fact source needs at least one measure")
	}
	for _, m := range s.Measures.Values() {
		switch m.Type {
		case "count":
			if m.Column != "" {
				return at(m.Line, "measure %s: count takes no column", m.Name)
			}
		case "sum":
			if m.Column == "" {
				return at(m.Line, "measure %s: sum needs a column", m.Name)
			}
			if s.IsPII(m.Column) {
				return at(m.Line, "measure %s: column %q is PII and is not mirrored, so it cannot be aggregated", m.Name, m.Column)
			}
		case "min", "max", "median":
			return at(m.Line, "measure %s: %s is not retractable under updates and deletes, so it cannot be kept exact incrementally", m.Name, m.Type)
		default:
			return at(m.Line, "measure %s: unknown type %q (use count or sum)", m.Name, m.Type)
		}
	}
	if s.CountMeasure == "" {
		for _, m := range s.Measures.Values() {
			if m.Type == "count" {
				s.CountMeasure = m.Name
				break
			}
		}
	}
	if m := s.Measures.Get(s.CountMeasure); m == nil || m.Type != "count" {
		return at(s.Line, "count_measure %q must name a count measure", s.CountMeasure)
	}
	for _, d := range s.Derived.Values() {
		num, den, ok := strings.Cut(d.Expr, "/")
		d.Num, d.Den = strings.TrimSpace(num), strings.TrimSpace(den)
		if !ok || s.Measures.Get(d.Num) == nil || s.Measures.Get(d.Den) == nil {
			return at(d.Line, "derived %s: expr must be \"<measure> / <measure>\", got %q", d.Name, d.Expr)
		}
	}
	for _, d := range s.Dimensions.Values() {
		switch {
		case d.Join != nil:
			jt := c.Sources.Get(d.Join.Table)
			if jt == nil {
				return at(d.Line, "dimension %s: join table %q is not a source, so it is neither published nor mirrored", d.Name, d.Join.Table)
			}
			if jt.IsPII(d.Join.Column) {
				return at(d.Line, "dimension %s: %s.%s is PII", d.Name, d.Join.Table, d.Join.Column)
			}
			if d.Join.On == "" || d.Join.Column == "" {
				return at(d.Line, "dimension %s: join needs on and column", d.Name)
			}
		case d.Column != "":
			if s.IsPII(d.Column) {
				return at(d.Line, "dimension %s: column %q is PII", d.Name, d.Column)
			}
		default:
			return at(d.Line, "dimension %s: needs column or join", d.Name)
		}
		if d.Type != "" && d.Type != "key" {
			return at(d.Line, "dimension %s: unknown type %q (omit it, or use key)", d.Name, d.Type)
		}
		if d.Label != nil && c.Sources.Get(d.Label.Table) == nil {
			return at(d.Line, "dimension %s: label table %q is not a source", d.Name, d.Label.Table)
		}
	}
	for _, t := range s.Top.Values() {
		if s.Dimensions.Get(t.Dim) == nil {
			return at(t.Line, "top %s: unknown dimension %q", t.Name, t.Dim)
		}
	}
	return nil
}

func (c *Config) validateRollup(r *Rollup) error {
	at := func(format string, args ...any) error {
		return fmt.Errorf("line %d: rollup %s: %s", r.Line, r.Name, fmt.Sprintf(format, args...))
	}
	s := c.Sources.Get(r.Source)
	if s == nil || s.Time == "" {
		return at("source %q is not a fact source", r.Source)
	}
	if len(r.Dims) > MaxDims {
		return at("at most %d dims per rollup", MaxDims)
	}
	seen := map[string]bool{}
	for _, d := range r.Dims {
		if s.Dimensions.Get(d) == nil {
			return at("unknown dimension %q (source %s has %s)", d, s.Name, strings.Join(s.Dimensions.Keys(), ", "))
		}
		if seen[d] {
			return at("dimension %q listed twice", d)
		}
		seen[d] = true
	}
	if r.KeyDim != "" && !seen[r.KeyDim] {
		return at("key_dim %q is not one of its dims", r.KeyDim)
	}
	if len(r.GrainSpec) == 0 {
		return at("grains is required (for example {hour: 35d, day: all, month: all})")
	}
	r.Retention = map[timeutil.Grain]time.Duration{}
	for name, spec := range r.GrainSpec {
		g, err := timeutil.ParseGrain(name)
		if err != nil {
			return at("%v", err)
		}
		d, err := parseRetention(spec)
		if err != nil {
			return at("grain %s: %v", name, err)
		}
		r.Retention[g] = d
	}
	return nil
}

// validateCoverage checks that a source has a dimensionless rollup holding
// day and month cells for all time, which every preset needs.
func (c *Config) validateCoverage(s *Source) error {
	for _, r := range c.RollupsFor(s.Name) {
		if len(r.Dims) == 0 && r.HasGrain(timeutil.GrainDay) && r.Retention[timeutil.GrainDay] == 0 {
			return nil
		}
	}
	return fmt.Errorf("source %s: add a rollup with dims: [] and grains {day: all}, so totals for every range are pre-aggregated", s.Name)
}

func parseRetention(s string) (time.Duration, error) {
	if s == "all" {
		return 0, nil
	}
	if len(s) < 2 {
		return 0, fmt.Errorf("retention %q: use all, or a number of days like 35d", s)
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("retention %q: use all, or a number of days like 35d", s)
	}
	switch s[len(s)-1] {
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	case 'y':
		return time.Duration(n) * 366 * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("retention %q: use all, or a number of days like 35d", s)
}

// ValidateColumns checks every referenced column against the columns that
// actually exist in the main database (table -> columns).
func (c *Config) ValidateColumns(tables map[string][]string) error {
	has := func(table, col string) bool {
		for _, x := range tables[table] {
			if x == col {
				return true
			}
		}
		return false
	}
	for _, s := range c.Sources.Values() {
		if _, ok := tables[s.Name]; !ok {
			return fmt.Errorf("line %d: source %s: table %q does not exist in the main database", s.Line, s.Name, s.Name)
		}
		cols := append([]string{}, s.Key...)
		if s.Time != "" {
			cols = append(cols, s.Time)
		}
		for _, m := range s.Measures.Values() {
			if m.Column != "" {
				cols = append(cols, m.Column)
			}
		}
		for _, d := range s.Dimensions.Values() {
			if d.Join != nil {
				cols = append(cols, d.Join.On)
				if !has(d.Join.Table, d.Join.Column) {
					return fmt.Errorf("line %d: dimension %s: column %s.%s does not exist", d.Line, d.Name, d.Join.Table, d.Join.Column)
				}
			} else {
				cols = append(cols, d.Column)
			}
		}
		for _, col := range cols {
			if !has(s.Name, col) {
				return fmt.Errorf("line %d: source %s: column %q does not exist in table %s", s.Line, s.Name, col, s.Name)
			}
		}
	}
	return nil
}
