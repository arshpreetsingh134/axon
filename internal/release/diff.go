package release

import (
	"fmt"
	"sort"
)

// Change is one difference between two release records.
type Change struct {
	Table string `json:"table"` // "record" for top-level fields
	Key   string `json:"key"`
	Old   string `json:"old,omitempty"` // empty: added
	New   string `json:"new,omitempty"` // empty: removed
}

// recordFields are the top-level fields worth comparing (the rest identify the
// record itself: version, digest, build id, …).
var recordFields = []string{"schema_version", "commit", "tool_version", "platforms", "source_lock_digest"}

// Diff lists every difference between a and b in their string tables
// (modules, sources, fabric, products, …) and in recordFields, sorted by table
// then key.
func Diff(a, b *Record) []Change {
	var out []Change
	for _, f := range recordFields {
		av, bv := scalar(a.Doc[f]), scalar(b.Doc[f])
		if av != bv {
			out = append(out, Change{Table: "record", Key: f, Old: av, New: bv})
		}
	}
	tables := map[string]bool{}
	for _, r := range []*Record{a, b} {
		for k, v := range r.Doc {
			if _, ok := v.(map[string]any); ok {
				tables[k] = true
			}
		}
	}
	for t := range tables {
		at, bt := a.Table(t), b.Table(t)
		keys := map[string]bool{}
		for k := range at {
			keys[k] = true
		}
		for k := range bt {
			keys[k] = true
		}
		for k := range keys {
			if at[k] != bt[k] {
				out = append(out, Change{Table: t, Key: k, Old: at[k], New: bt[k]})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Table != out[j].Table {
			return tableRank(out[i].Table) < tableRank(out[j].Table) ||
				(tableRank(out[i].Table) == tableRank(out[j].Table) && out[i].Table < out[j].Table)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func tableRank(t string) int {
	switch t {
	case "record":
		return 0
	case "modules":
		return 1
	case "sources":
		return 2
	}
	return 3
}

func scalar(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []any:
		s := ""
		for i, e := range x {
			if i > 0 {
				s += ","
			}
			s += fmt.Sprint(e)
		}
		return s
	}
	return fmt.Sprint(v)
}
