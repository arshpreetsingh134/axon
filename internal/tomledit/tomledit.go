// Package tomledit edits TOML descriptors as text blocks so comments, ordering and
// hand formatting survive every mutation (no full re-serialization).
package tomledit

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Doc is an editable TOML document.
type Doc struct{ lines []string }

// Parse splits text into lines.
func Parse(text string) *Doc {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return &Doc{}
	}
	return &Doc{lines: strings.Split(text, "\n")}
}

// String renders the document with a trailing newline.
func (d *Doc) String() string {
	if len(d.lines) == 0 {
		return ""
	}
	return strings.Join(d.lines, "\n") + "\n"
}

var headerRE = regexp.MustCompile(`^\s*(\[\[?)\s*([^\]]+?)\s*(\]\]?)\s*(#.*)?$`)

type header struct {
	line  int
	name  string
	array bool
}

func (d *Doc) headers() []header {
	var hs []header
	inMulti := false
	for i, l := range d.lines {
		if strings.Count(l, `"""`)%2 == 1 || strings.Count(l, `'''`)%2 == 1 {
			inMulti = !inMulti
		}
		if inMulti {
			continue
		}
		if m := headerRE.FindStringSubmatch(l); m != nil {
			hs = append(hs, header{line: i, name: unquoteKey(m[2]), array: m[1] == "[["})
		}
	}
	return hs
}

func unquoteKey(k string) string {
	var parts []string
	for _, p := range splitDotted(k) {
		p = strings.TrimSpace(p)
		if uq, err := strconv.Unquote(p); err == nil {
			p = uq
		} else if strings.HasPrefix(p, "'") && strings.HasSuffix(p, "'") && len(p) >= 2 {
			p = p[1 : len(p)-1]
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ".")
}

func splitDotted(k string) []string {
	var out []string
	var cur strings.Builder
	q := byte(0)
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case q != 0:
			cur.WriteByte(c)
			if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
			cur.WriteByte(c)
		case c == '.':
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(out, cur.String())
}

// span is [start, end) of lines.
type span struct{ start, end int }

// blockEnd returns the line index where the table starting at header h ends
// (the next header that is not a sub-table of it), excluding trailing blank and
// comment lines that lead into the next block.
func (d *Doc) blockEnd(hs []header, idx int, includeSubtables bool) int {
	h := hs[idx]
	end := len(d.lines)
	for j := idx + 1; j < len(hs); j++ {
		n := hs[j]
		if includeSubtables && !n.array && strings.HasPrefix(n.name, h.name+".") {
			continue
		}
		end = n.line
		break
	}
	// leave the next block's leading comments/blanks to it
	for end > h.line+1 {
		t := strings.TrimSpace(d.lines[end-1])
		if t == "" || strings.HasPrefix(t, "#") {
			end--
			continue
		}
		break
	}
	return end
}

// leadStart extends a block upward over attached comment lines (no blank gap).
func (d *Doc) leadStart(line int) int {
	s := line
	for s > 0 && strings.HasPrefix(strings.TrimSpace(d.lines[s-1]), "#") {
		s--
	}
	return s
}

// HasTable reports whether [name] exists.
func (d *Doc) HasTable(name string) bool {
	name = unquoteKey(name)
	for _, h := range d.headers() {
		if !h.array && h.name == name {
			return true
		}
	}
	return false
}

// RemoveTable deletes [name] and its sub-tables (with attached comments).
func (d *Doc) RemoveTable(name string) bool {
	name = unquoteKey(name)
	hs := d.headers()
	for i, h := range hs {
		if !h.array && h.name == name {
			d.cut(span{d.leadStart(h.line), d.blockEnd(hs, i, true)})
			return true
		}
	}
	return false
}

// ArrayBlockIndex finds the [[array]] element whose key equals value.
func (d *Doc) arrayBlock(array, key, value string) (span, bool) {
	hs := d.headers()
	for i, h := range hs {
		if !h.array || h.name != array {
			continue
		}
		end := d.blockEnd(hs, i, true)
		for l := h.line + 1; l < end; l++ {
			if k, v, ok := keyValue(d.lines[l]); ok && k == key && v == value {
				return span{d.leadStart(h.line), end}, true
			}
		}
	}
	return span{}, false
}

// HasArrayElem reports whether an [[array]] element with key=value exists.
func (d *Doc) HasArrayElem(array, key, value string) bool {
	_, ok := d.arrayBlock(array, key, value)
	return ok
}

// RemoveArrayElem deletes the [[array]] element with key=value (and its sub-tables).
func (d *Doc) RemoveArrayElem(array, key, value string) bool {
	sp, ok := d.arrayBlock(array, key, value)
	if ok {
		d.cut(sp)
	}
	return ok
}

func (d *Doc) cut(sp span) {
	lines := append([]string{}, d.lines[:sp.start]...)
	lines = append(lines, d.lines[sp.end:]...)
	// collapse >2 consecutive blank lines left behind
	var out []string
	blank := 0
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	d.lines = out
}

// AppendBlock appends a raw block (separated by one blank line).
func (d *Doc) AppendBlock(block string) {
	block = strings.TrimRight(block, "\n")
	if len(d.lines) > 0 && strings.TrimSpace(d.lines[len(d.lines)-1]) != "" {
		d.lines = append(d.lines, "")
	}
	d.lines = append(d.lines, strings.Split(block, "\n")...)
}

// InsertBlockBefore inserts a block before the first [[array]] header (or appends).
func (d *Doc) InsertBlockBeforeArray(array, block string) {
	for _, h := range d.headers() {
		if h.array && h.name == array {
			at := d.leadStart(h.line)
			ins := append(strings.Split(strings.TrimRight(block, "\n"), "\n"), "")
			lines := append([]string{}, d.lines[:at]...)
			lines = append(lines, ins...)
			d.lines = append(lines, d.lines[at:]...)
			return
		}
	}
	d.AppendBlock(block)
}

var kvRE = regexp.MustCompile(`^\s*("[^"]+"|'[^']+'|[A-Za-z0-9_.-]+)\s*=\s*(.*)$`)

// keyValue parses `key = "value"  # c` → key, value (unquoted string or raw).
func keyValue(line string) (string, string, bool) {
	m := kvRE.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	k := unquoteKey(m[1])
	raw := stripComment(m[2])
	if uq, err := strconv.Unquote(raw); err == nil {
		return k, uq, true
	}
	return k, raw, true
}

func stripComment(v string) string {
	q := byte(0)
	for i := 0; i < len(v); i++ {
		c := v[i]
		if q != 0 {
			if c == '\\' && q == '"' {
				i++
				continue
			}
			if c == q {
				q = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			q = c
			continue
		}
		if c == '#' {
			return strings.TrimSpace(v[:i])
		}
	}
	return strings.TrimSpace(v)
}

// tableRange returns the line span of keys belonging to a table ("" = root).
func (d *Doc) tableRange(table string) (span, bool) {
	hs := d.headers()
	if table == "" {
		end := len(d.lines)
		if len(hs) > 0 {
			end = hs[0].line
		}
		return span{0, end}, true
	}
	want := unquoteKey(table)
	for i, h := range hs {
		if !h.array && h.name == want {
			end := len(d.lines)
			if i+1 < len(hs) {
				end = hs[i+1].line
			}
			return span{h.line + 1, end}, true
		}
	}
	return span{}, false
}

// Get returns the raw (unquoted if string) value of key in table.
func (d *Doc) Get(table, key string) (string, bool) {
	sp, ok := d.tableRange(table)
	if !ok {
		return "", false
	}
	for l := sp.start; l < sp.end; l++ {
		if k, v, ok := keyValue(d.lines[l]); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// Set sets key = value (value already TOML-encoded) in table, creating the table
// at the end if needed. An existing line keeps its trailing comment.
func (d *Doc) Set(table, key, encoded string) {
	sp, ok := d.tableRange(table)
	if !ok {
		d.AppendBlock(fmt.Sprintf("[%s]\n%s = %s", table, keyName(key), encoded))
		return
	}
	last := -1
	for l := sp.start; l < sp.end; l++ {
		line := d.lines[l]
		if k, _, ok := keyValue(line); ok {
			last = l
			if k == key {
				m := kvRE.FindStringSubmatch(line)
				comment := ""
				if c := strings.TrimPrefix(m[2], stripComment(m[2])); strings.Contains(c, "#") {
					comment = "  " + strings.TrimSpace(c)
				}
				prefix := line[:strings.Index(line, "=")+1]
				d.lines[l] = strings.TrimRight(prefix, " ") + " " + encoded + comment
				return
			}
		}
	}
	at := sp.start
	if last >= 0 {
		at = last + 1
	}
	newLine := keyName(key) + " = " + encoded
	lines := append([]string{}, d.lines[:at]...)
	lines = append(lines, newLine)
	d.lines = append(lines, d.lines[at:]...)
}

// Unset removes key from table.
func (d *Doc) Unset(table, key string) bool {
	sp, ok := d.tableRange(table)
	if !ok {
		return false
	}
	for l := sp.start; l < sp.end; l++ {
		if k, _, ok := keyValue(d.lines[l]); ok && k == key {
			d.lines = append(d.lines[:l], d.lines[l+1:]...)
			return true
		}
	}
	return false
}

// Tables returns all [table] names (not arrays) in order.
func (d *Doc) Tables() []string {
	var out []string
	for _, h := range d.headers() {
		if !h.array {
			out = append(out, h.name)
		}
	}
	return out
}

func keyName(k string) string {
	if regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(k) {
		return k
	}
	return strconv.Quote(k)
}

// Quote encodes a TOML basic string.
func Quote(s string) string { return strconv.Quote(s) }

// Encode encodes a Go scalar or slice as a TOML value.
func Encode(v any) string {
	switch t := v.(type) {
	case string:
		return Quote(t)
	case int:
		return strconv.Itoa(t)
	case bool:
		return strconv.FormatBool(t)
	case []string:
		var qs []string
		for _, s := range t {
			qs = append(qs, Quote(s))
		}
		return "[" + strings.Join(qs, ", ") + "]"
	case map[string]string:
		var ks []string
		for k := range t {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		var parts []string
		for _, k := range ks {
			parts = append(parts, keyName(k)+" = "+Quote(t[k]))
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	}
	return fmt.Sprint(v)
}

// EncodeGuess encodes a CLI-provided value: ints and bools bare, lists from
// "[a,b]", everything else as a string.
func EncodeGuess(raw string) string {
	if _, err := strconv.Atoi(raw); err == nil {
		return raw
	}
	if raw == "true" || raw == "false" {
		return raw
	}
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		inner := strings.TrimSpace(raw[1 : len(raw)-1])
		if inner == "" {
			return "[]"
		}
		var items []string
		for _, p := range strings.Split(inner, ",") {
			items = append(items, strings.Trim(strings.TrimSpace(p), `"'`))
		}
		return Encode(items)
	}
	return Quote(raw)
}
