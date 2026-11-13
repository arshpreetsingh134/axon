// Package dotenv reads and updates .env files and resolves ${VAR} references.
package dotenv

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Read parses a dotenv file. A missing file yields an empty map.
// Supports KEY=value, export KEY=value, quotes, and multi-line double-quoted values.
func Read(path string) (map[string]string, error) {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(string(b))
}

// Parse parses dotenv content.
func Parse(s string) (map[string]string, error) {
	out := map[string]string{}
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("line %d: expected KEY=value", i+1)
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		switch {
		case strings.HasPrefix(val, `"`):
			body := val[1:]
			for !closesDouble(body) && i+1 < len(lines) {
				i++
				body += "\n" + lines[i]
			}
			end := strings.LastIndex(body, `"`)
			if end < 0 {
				return nil, fmt.Errorf("key %s: unterminated quote", key)
			}
			val = unescape(body[:end])
		case strings.HasPrefix(val, `'`):
			end := strings.LastIndex(val[1:], `'`)
			if end < 0 {
				return nil, fmt.Errorf("key %s: unterminated quote", key)
			}
			val = val[1 : end+1]
		default:
			if j := strings.Index(val, " #"); j >= 0 {
				val = strings.TrimSpace(val[:j])
			}
		}
		out[key] = val
	}
	return out, nil
}

func closesDouble(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == '"' {
			return true
		}
	}
	return false
}

func unescape(s string) string {
	return strings.NewReplacer(`\n`, "\n", `\"`, `"`, `\\`, `\`).Replace(s)
}

var refRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Refs returns the ${VAR} names referenced in s.
func Refs(s string) []string {
	var out []string
	for _, m := range refRE.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

// IsPureRef reports whether s is exactly "${VAR}" and returns VAR.
func IsPureRef(s string) (string, bool) {
	m := regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`).FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Expand resolves ${VAR} in s from vals; unknown vars are reported.
func Expand(s string, vals map[string]string) (string, []string) {
	var missing []string
	out := refRE.ReplaceAllStringFunc(s, func(ref string) string {
		k := ref[2 : len(ref)-1]
		if v, ok := vals[k]; ok {
			return v
		}
		missing = append(missing, k)
		return ref
	})
	return out, missing
}

// Set adds or replaces KEY in a dotenv file, preserving every other line.
func Set(path, key, value string) error {
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(b) == 0 {
		lines = nil
	}
	enc := key + "=" + quote(value)
	done := false
	for i, l := range lines {
		t := strings.TrimPrefix(strings.TrimSpace(l), "export ")
		if strings.HasPrefix(t, key+"=") {
			lines[i] = enc
			done = true
		}
	}
	if !done {
		lines = append(lines, enc)
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

func quote(v string) string {
	if v == "" || strings.ContainsAny(v, " #\"'\n$`\\") {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v) + `"`
	}
	return v
}

// Keys returns sorted keys of m.
func Keys(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Lines renders m as KEY=value lines (sorted), quoting where needed.
func Lines(m map[string]string) string {
	var b strings.Builder
	w := bufio.NewWriter(&b)
	for _, k := range Keys(m) {
		fmt.Fprintf(w, "%s=%s\n", k, quote(m[k]))
	}
	w.Flush()
	return b.String()
}

// Remove deletes keys from a dotenv file (including multi-line quoted values),
// preserving every other line and the file mode.
func Remove(path string, keys ...string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
	}
	lines := strings.Split(string(b), "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		t := strings.TrimPrefix(strings.TrimSpace(lines[i]), "export ")
		eq := strings.IndexByte(t, '=')
		if eq <= 0 || strings.HasPrefix(t, "#") || !drop[strings.TrimSpace(t[:eq])] {
			out = append(out, lines[i])
			continue
		}
		// skip continuation lines of a multi-line double-quoted value
		val := strings.TrimSpace(t[eq+1:])
		if strings.HasPrefix(val, `"`) && !closesDouble(val[1:]) {
			for i+1 < len(lines) {
				i++
				if closesDouble(lines[i]) {
					break
				}
			}
		}
	}
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	return os.WriteFile(path, []byte(strings.Join(out, "\n")), mode)
}
