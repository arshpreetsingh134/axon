package official

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"axon/internal/sh"
)

// LastApplied is the annotation kubectl apply records on every object.
const LastApplied = "kubectl.kubernetes.io/last-applied-configuration"

// Key identifies an object: "Kind/name".
func Key(o Object) string {
	m, _ := o["metadata"].(map[string]any)
	return fmt.Sprintf("%v/%v", o["kind"], m["name"])
}

// Normalize round-trips an object through JSON so numbers and nesting compare
// like decoded live objects.
func Normalize(o Object) Object {
	b, _ := json.Marshal(o)
	var out Object
	_ = json.Unmarshal(b, &out)
	return out
}

// FetchLive reads the last-applied configuration of every object of kinds in
// namespace, with the developer's own kube context (plan D1). Secrets are never
// read. Objects without the annotation are returned in unmanaged.
func FetchLive(context, namespace string, kinds []string) (live map[string]Object, unmanaged []string, err error) {
	live = map[string]Object{}
	for _, k := range kinds {
		if strings.EqualFold(k, "secrets") || strings.EqualFold(k, "secret") {
			continue
		}
		r, err := sh.Run(sh.Cmd{Name: "kubectl", Args: []string{"--context", context, "-n", namespace, "get", k, "-o", "json"}, Timeout: 3 * time.Minute})
		if err != nil {
			return nil, nil, fmt.Errorf("read %s in %s (context %s): %w", k, namespace, context, err)
		}
		var list struct {
			Items []struct {
				Kind     string `json:"kind"`
				Metadata struct {
					Name        string            `json:"name"`
					Annotations map[string]string `json:"annotations"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(r.Stdout), &list); err != nil {
			return nil, nil, err
		}
		for _, it := range list.Items {
			la := it.Metadata.Annotations[LastApplied]
			if la == "" {
				unmanaged = append(unmanaged, it.Kind+"/"+it.Metadata.Name)
				continue
			}
			var o Object
			if err := json.Unmarshal([]byte(la), &o); err != nil {
				continue
			}
			live[Key(o)] = o
		}
	}
	return live, unmanaged, nil
}

// PlanEntry is the planned action for one object.
type PlanEntry struct {
	Key    string   `json:"key"`
	Action string   `json:"action"` // unchanged | change | create
	Diffs  []string `json:"diffs,omitempty"`
}

// Plan compares rendered objects with live ones.
func Plan(rendered []Object, live map[string]Object) []PlanEntry {
	var out []PlanEntry
	for _, r := range rendered {
		n := Normalize(r)
		k := Key(n)
		l, ok := live[k]
		switch {
		case !ok:
			out = append(out, PlanEntry{Key: k, Action: "create"})
		default:
			var d []string
			diff(l, n, "", &d)
			if len(d) == 0 {
				out = append(out, PlanEntry{Key: k, Action: "unchanged"})
			} else {
				out = append(out, PlanEntry{Key: k, Action: "change", Diffs: d})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// diff lists the paths where live (a) and rendered (b) differ. Lists of
// objects with a "name" are compared by name, then for order.
func diff(a, b any, path string, out *[]string) {
	if reflect.DeepEqual(a, b) {
		return
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: %s → %s", or(path, "/"), brief(a), brief(b)))
			return
		}
		keys := map[string]bool{}
		for k := range av {
			keys[k] = true
		}
		for k := range bv {
			keys[k] = true
		}
		var ks []string
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			x, xok := av[k]
			y, yok := bv[k]
			switch {
			case !xok:
				*out = append(*out, fmt.Sprintf("%s/%s: added %s", path, k, brief(y)))
			case !yok:
				*out = append(*out, fmt.Sprintf("%s/%s: removed (was %s)", path, k, brief(x)))
			default:
				diff(x, y, path+"/"+k, out)
			}
		}
	case []any:
		bv, ok := b.([]any)
		if !ok {
			*out = append(*out, fmt.Sprintf("%s: %s → %s", path, brief(a), brief(b)))
			return
		}
		if an, bn := names(av), names(bv); an != nil && bn != nil {
			for n, x := range an {
				if y, ok := bn[n]; ok {
					diff(x, y, fmt.Sprintf("%s[%s]", path, n), out)
				} else {
					*out = append(*out, fmt.Sprintf("%s[%s]: removed", path, n))
				}
			}
			for n, y := range bn {
				if _, ok := an[n]; !ok {
					*out = append(*out, fmt.Sprintf("%s[%s]: added %s", path, n, brief(y)))
				}
			}
			if order(av) != order(bv) && len(an) == len(bn) {
				*out = append(*out, path+": entries in a different order")
			}
			return
		}
		if len(av) != len(bv) {
			*out = append(*out, fmt.Sprintf("%s: %d → %d items", path, len(av), len(bv)))
			return
		}
		for i := range av {
			diff(av[i], bv[i], fmt.Sprintf("%s[%d]", path, i), out)
		}
	default:
		*out = append(*out, fmt.Sprintf("%s: %s → %s", path, brief(a), brief(b)))
	}
}

// order is the sequence of names in a list of named objects.
func order(l []any) string {
	var s []string
	for _, x := range l {
		if m, ok := x.(map[string]any); ok {
			s = append(s, fmt.Sprint(m["name"]))
		}
	}
	return strings.Join(s, ",")
}

// names maps a list of named objects by name, or returns nil.
func names(l []any) map[string]any {
	out := map[string]any{}
	for _, x := range l {
		m, ok := x.(map[string]any)
		if !ok {
			return nil
		}
		n, ok := m["name"].(string)
		if !ok {
			return nil
		}
		out[n] = m
	}
	return out
}

func brief(v any) string {
	b, _ := json.Marshal(v)
	s := string(b)
	if len(s) > 90 {
		s = s[:87] + "..."
	}
	return s
}
