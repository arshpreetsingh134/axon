// Package yamlx provides an insertion-ordered map for deterministic, readable YAML.
package yamlx

import (
	"bytes"
	"strings"

	"gopkg.in/yaml.v3"
)

// M is an ordered map. Build with New("k", v, "k2", v2…).
type M struct {
	keys []string
	vals map[string]any
}

// New creates an ordered map from alternating key/value pairs.
func New(kv ...any) *M {
	m := &M{vals: map[string]any{}}
	for i := 0; i+1 < len(kv); i += 2 {
		m.Set(kv[i].(string), kv[i+1])
	}
	return m
}

// Set adds or replaces a key (insertion order kept). Nil values are skipped.
func (m *M) Set(k string, v any) *M {
	if v == nil {
		return m
	}
	switch t := v.(type) {
	case *M:
		if t == nil {
			return m
		}
	case []any:
		if t == nil {
			return m
		}
	}
	if _, ok := m.vals[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.vals[k] = v
	return m
}

// Get returns a value.
func (m *M) Get(k string) any { return m.vals[k] }

// Len returns the number of keys.
func (m *M) Len() int { return len(m.keys) }

// MarshalYAML implements yaml.Marshaler.
func (m *M) MarshalYAML() (any, error) {
	n := &yaml.Node{Kind: yaml.MappingNode}
	for _, k := range m.keys {
		var kn, vn yaml.Node
		kn.SetString(k)
		if err := vn.Encode(m.vals[k]); err != nil {
			return nil, err
		}
		n.Content = append(n.Content, &kn, &vn)
	}
	return n, nil
}

// StrMap converts a string map to an ordered map with sorted keys.
func StrMap(in map[string]string) *M {
	m := New()
	for _, k := range sortedKeys(in) {
		m.Set(k, in[k])
	}
	return m
}

func sortedKeys(in map[string]string) []string {
	ks := make([]string, 0, len(in))
	for k := range in {
		ks = append(ks, k)
	}
	// simple insertion sort keeps this package dependency-free
	for i := 1; i < len(ks); i++ {
		for j := i; j > 0 && ks[j] < ks[j-1]; j-- {
			ks[j], ks[j-1] = ks[j-1], ks[j]
		}
	}
	return ks
}

// Marshal renders one document with 2-space indentation.
func Marshal(v any) (string, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	enc.Close()
	return b.String(), nil
}

// MarshalAll renders documents separated by "---".
func MarshalAll(docs []any) (string, error) {
	var parts []string
	for _, d := range docs {
		s, err := Marshal(d)
		if err != nil {
			return "", err
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "---\n"), nil
}

// L is a convenience list constructor.
func L(items ...any) []any { return items }
