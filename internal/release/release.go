// Package release manages immutable release records (releases/<version>.toml):
// image references and source commits, sealed by a SHA-256 digest.
package release

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// SchemaVersion is the axon record schema.
const SchemaVersion = 1

// Record is a release record kept as a generic document so unknown keys survive.
type Record struct {
	Doc  map[string]any
	Path string
}

// Version returns the version string.
func (r *Record) Version() string { s, _ := r.Doc["version"].(string); return s }

// Str returns a top-level string field.
func (r *Record) Str(k string) string {
	switch v := r.Doc[k].(type) {
	case string:
		return v
	case time.Time:
		return v.UTC().Format(time.RFC3339)
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// Table returns a string-valued table.
func (r *Record) Table(k string) map[string]string {
	out := map[string]string{}
	if m, ok := r.Doc[k].(map[string]any); ok {
		for kk, v := range m {
			out[kk] = fmt.Sprint(v)
		}
	}
	return out
}

// Dir is <controller>/releases.
func Dir(controller string) string { return filepath.Join(controller, "releases") }

// Load reads one record.
func Load(path string) (*Record, error) {
	var doc map[string]any
	if _, err := toml.DecodeFile(path, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &Record{Doc: doc, Path: path}, nil
}

// List returns records sorted by semantic version.
func List(controller string) ([]*Record, error) {
	paths, _ := filepath.Glob(filepath.Join(Dir(controller), "*.toml"))
	var out []*Record
	for _, p := range paths {
		r, err := Load(p)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return Less(out[i].Version(), out[j].Version()) })
	return out, nil
}

// Less compares dotted numeric versions.
func Less(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		if ea == nil && eb == nil {
			if na != nb {
				return na < nb
			}
			continue
		}
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return len(pa) < len(pb)
}

// Canonical encodes the record without `digest`: top-level scalars sorted by key,
// then tables sorted by name with their keys sorted and indented two spaces.
// Any change to this encoding changes every digest — it is pinned by golden tests.
func Canonical(doc map[string]any) string {
	var scal, tabs []string
	for k, v := range doc {
		if k == "digest" {
			continue
		}
		if _, ok := v.(map[string]any); ok {
			tabs = append(tabs, k)
		} else {
			scal = append(scal, k)
		}
	}
	sort.Strings(scal)
	sort.Strings(tabs)
	var b strings.Builder
	for _, k := range scal {
		fmt.Fprintf(&b, "%s = %s\n", key(k), value(doc[k]))
	}
	for _, t := range tabs {
		fmt.Fprintf(&b, "\n[%s]\n", key(t))
		m := doc[t].(map[string]any)
		var ks []string
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			fmt.Fprintf(&b, "  %s = %s\n", key(k), value(m[k]))
		}
	}
	return b.String()
}

var bare = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func key(k string) string {
	if bare.MatchString(k) {
		return k
	}
	return strconv.Quote(k)
}

func value(v any) string {
	switch t := v.(type) {
	case string:
		return strconv.Quote(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	case []any:
		var parts []string
		for _, e := range t {
			parts = append(parts, value(e))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []string:
		var parts []string
		for _, e := range t {
			parts = append(parts, strconv.Quote(e))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		var ks []string
		for k := range t {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		var parts []string
		for _, k := range ks {
			parts = append(parts, key(k)+" = "+value(t[k]))
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	}
	return strconv.Quote(fmt.Sprint(v))
}

// Digest returns "sha256:<hex>" of the canonical body.
func Digest(doc map[string]any) string {
	sum := sha256.Sum256([]byte(Canonical(doc)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Render writes a record file: a comment header, then the canonical body with the
// digest inserted after `version`.
func Render(doc map[string]any) string {
	d := Digest(doc)
	body := Canonical(doc)
	var b strings.Builder
	b.WriteString("# Release record — written by `axon release create`. IMMUTABLE: never edit;\n")
	b.WriteString("# `axon release check` recomputes the digest over everything but `digest`.\n")
	lines := strings.Split(body, "\n")
	inserted := false
	for _, l := range lines {
		if !inserted && (strings.HasPrefix(l, "\n") || l == "" || strings.HasPrefix(l, "[")) {
			fmt.Fprintf(&b, "digest = %q\n", d)
			inserted = true
		}
		b.WriteString(l + "\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// Verify recomputes a record's digest.
func Verify(r *Record) (want, got string, producer string) {
	want = r.Str("digest")
	got = Digest(r.Doc)
	producer = r.Str("tool_version")
	return
}

// MadeByAxon reports whether the record was produced by axon (so its digest is
// verifiable with axon's canonical encoding).
func MadeByAxon(r *Record) bool { return strings.HasPrefix(r.Str("tool_version"), "axon/") }

// Policy is release/policy.toml.
type Policy struct {
	Series     string `toml:"series"`
	FirstPatch int    `toml:"first_patch"`
}

// LoadPolicy reads the policy (missing → series "0.1").
func LoadPolicy(controller string) Policy {
	p := Policy{Series: "0.1"}
	_, _ = toml.DecodeFile(filepath.Join(controller, "release", "policy.toml"), &p)
	if p.Series == "" {
		p.Series = "0.1"
	}
	return p
}

// Next returns the next version in a series given existing records.
func Next(series string, firstPatch int, existing []*Record) string {
	next := firstPatch
	for _, r := range existing {
		v := r.Version()
		if strings.HasPrefix(v, series+".") {
			if n, err := strconv.Atoi(strings.TrimPrefix(v, series+".")); err == nil && n >= next {
				next = n + 1
			}
		}
	}
	return fmt.Sprintf("%s.%d", series, next)
}
