package official

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// KeyRef is one place that needs a key in the environment's app env secret.
type KeyRef struct {
	Name string `json:"name"`
	App  string `json:"app,omitempty"`
	Line int    `json:"line,omitempty"` // 0 when only the TOML decoder saw it (an inline table)
}

var (
	exactRefRE = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)
	anyRefRE   = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	headerRE   = regexp.MustCompile(`^\s*(\[\[?)\s*([^\]]+?)\s*\]\]?\s*(#.*)?$`)
	kvRE       = regexp.MustCompile(`^\s*("?[A-Za-z0-9_.-]+"?)\s*=\s*"((?:[^"\\]|\\.)*)"\s*(#.*)?$`)
)

// RequiredKeys returns the keys an official deploy requires its pre-provisioned
// app env secret to hold: every [app.env] value of the cluster descriptor that
// is exactly "${NAME}" (SPEC.md Appendix C.5). It also returns ${NAME}
// patterns found in comments, which the gate does not count (a real deploy
// passed with one present); callers may show them for information.
func RequiredKeys(descriptor string) (refs, inComments []KeyRef, err error) {
	b, err := os.ReadFile(descriptor)
	if err != nil {
		return nil, nil, err
	}
	// The decoder is the authority on which values are exact references.
	var doc struct {
		App []struct {
			Name string         `toml:"name"`
			Env  map[string]any `toml:"env"`
		} `toml:"app"`
	}
	if _, err := toml.Decode(string(b), &doc); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", descriptor, err)
	}
	decoded := map[string]map[string]bool{} // key → apps
	for _, a := range doc.App {
		for _, v := range a.Env {
			s, ok := v.(string)
			if !ok {
				continue
			}
			if m := exactRefRE.FindStringSubmatch(s); m != nil {
				if decoded[m[1]] == nil {
					decoded[m[1]] = map[string]bool{}
				}
				decoded[m[1]][a.Name] = true
			}
		}
	}

	// A line scan finds where each one is, for messages. Table states: outside
	// any app, at the root of an [[app]] table, in its [app.env], or in another
	// [app.*] sub-table.
	const (
		outside = iota
		appRoot
		appEnv
		appOther
	)
	state, app := outside, ""
	located := map[string]map[string]bool{}
	for i, line := range strings.Split(string(b), "\n") {
		n := i + 1
		if c := commentOf(line); c != "" {
			for _, m := range anyRefRE.FindAllStringSubmatch(c, -1) {
				inComments = append(inComments, KeyRef{Name: m[1], App: app, Line: n})
			}
		}
		if h := headerRE.FindStringSubmatch(line); h != nil {
			name := strings.TrimSpace(h[2])
			switch {
			case h[1] == "[[" && name == "app":
				state, app = appRoot, ""
			case h[1] == "[" && name == "app.env" && state != outside:
				state = appEnv
			case strings.HasPrefix(name, "app.") && state != outside:
				state = appOther
			default:
				state, app = outside, ""
			}
			continue
		}
		kv := kvRE.FindStringSubmatch(line)
		if kv == nil {
			continue
		}
		if state == appRoot && strings.Trim(kv[1], `"`) == "name" {
			app = kv[2]
			continue
		}
		if state != appEnv {
			continue
		}
		if m := exactRefRE.FindStringSubmatch(kv[2]); m != nil && decoded[m[1]] != nil {
			refs = append(refs, KeyRef{Name: m[1], App: app, Line: n})
			if located[m[1]] == nil {
				located[m[1]] = map[string]bool{}
			}
			located[m[1]][app] = true
		}
	}
	for k, apps := range decoded {
		for a := range apps {
			if !located[k][a] {
				refs = append(refs, KeyRef{Name: k, App: a})
			}
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Name != refs[j].Name {
			return refs[i].Name < refs[j].Name
		}
		return refs[i].Line < refs[j].Line
	})
	return refs, inComments, nil
}

// KeyNames returns the distinct, sorted names of refs.
func KeyNames(refs []KeyRef) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range refs {
		if !seen[r.Name] {
			seen[r.Name] = true
			out = append(out, r.Name)
		}
	}
	sort.Strings(out)
	return out
}

// commentOf returns the comment part of a TOML line (after an unquoted #).
func commentOf(line string) string {
	inStr, quote := false, byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inStr && c == '\\' && quote == '"':
			i++
		case inStr && c == quote:
			inStr = false
		case !inStr && (c == '"' || c == '\''):
			inStr, quote = true, c
		case !inStr && c == '#':
			return line[i:]
		}
	}
	return ""
}
