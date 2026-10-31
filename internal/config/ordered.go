package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Ordered is a YAML mapping that keeps the author's order and records each
// entry's name and line, so validation errors can point at the right line.
type Ordered[T any] struct {
	keys []string
	vals map[string]T
}

type named interface{ setName(name string, line int) }

func (s *Source) setName(n string, l int)    { s.Name, s.Line = n, l }
func (m *Measure) setName(n string, l int)   { m.Name, m.Line = n, l }
func (d *Derived) setName(n string, l int)   { d.Name, d.Line = n, l }
func (d *Dimension) setName(n string, l int) { d.Name, d.Line = n, l }
func (t *TopList) setName(n string, l int)   { t.Name, t.Line = n, l }
func (r *Rollup) setName(n string, l int)    { r.Name, r.Line = n, l }

// UnmarshalYAML decodes a mapping node in order.
func (o *Ordered[T]) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: expected a mapping", node.Line)
	}
	o.vals = map[string]T{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, v := node.Content[i], node.Content[i+1]
		if _, dup := o.vals[k.Value]; dup {
			return fmt.Errorf("line %d: %q is defined twice", k.Line, k.Value)
		}
		var val T
		if err := v.Decode(&val); err != nil {
			return fmt.Errorf("line %d: %s: %w", k.Line, k.Value, err)
		}
		if n, ok := any(val).(named); ok {
			n.setName(k.Value, k.Line)
		}
		o.keys = append(o.keys, k.Value)
		o.vals[k.Value] = val
	}
	return nil
}

// Keys returns the names in declared order.
func (o *Ordered[T]) Keys() []string { return append([]string(nil), o.keys...) }

// Values returns the entries in declared order.
func (o *Ordered[T]) Values() []T {
	out := make([]T, 0, len(o.keys))
	for _, k := range o.keys {
		out = append(out, o.vals[k])
	}
	return out
}

// Get returns the entry with the given name, or the zero value.
func (o *Ordered[T]) Get(name string) T { return o.vals[name] }

// Len returns the number of entries.
func (o *Ordered[T]) Len() int { return len(o.keys) }

// Set adds or replaces an entry, keeping its position if it exists.
func (o *Ordered[T]) Set(name string, v T) {
	if o.vals == nil {
		o.vals = map[string]T{}
	}
	if _, ok := o.vals[name]; !ok {
		o.keys = append(o.keys, name)
	}
	o.vals[name] = v
}
