// Package cli is axon's small verb registry and argument parser.
//
// Grammar:  axon <verb words…> [positional…] [--flag[=| ]value | --bool-flag]…
//
// Verbs are 1–3 words resolved by longest match. Flags are double-dash (a few
// global flags also accept a short form), may appear anywhere after the verb,
// and repeatable flags append.
package cli

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"axon/internal/ui"
)

// Exit codes.
const (
	ExitOK        = 0
	ExitFailure   = 1 // runtime failure, failing checks, pending --check changes
	ExitUsage     = 2 // unknown verb/flag, missing argument
	ExitNoContext = 3 // verb needs a descriptor that is not there
)

// ExitError carries a message and an exit code.
type ExitError struct {
	Code int
	Msg  string
	// Silent errors print nothing (the verb already reported).
	Silent bool
}

func (e *ExitError) Error() string { return e.Msg }

// Usage returns an exit-2 error.
func Usage(format string, a ...any) error {
	return &ExitError{Code: ExitUsage, Msg: fmt.Sprintf(format, a...)}
}

// NoContext returns an exit-3 error.
func NoContext(format string, a ...any) error {
	return &ExitError{Code: ExitNoContext, Msg: fmt.Sprintf(format, a...)}
}

// Failed returns an exit-1 error that prints nothing more (details already shown).
func Failed() error { return &ExitError{Code: ExitFailure, Silent: true} }

// Kind is a flag's value kind.
type Kind int

const (
	Bool Kind = iota
	String
	Int
	Multi // repeatable string
)

// Flag declares a verb flag.
type Flag struct {
	Name    string
	Kind    Kind
	Usage   string
	Default string
	Alias   []string // other long names accepted (e.g. a renamed flag)
	Short   string   // single-letter alias (global convenience flags only)
	Hidden  bool
	Value   string // placeholder in help, e.g. "<dir>"
}

// Arg declares a positional argument.
type Arg struct {
	Name     string
	Optional bool
	Variadic bool
}

// Verb is one command.
type Verb struct {
	Name    string // space-separated words
	Args    []Arg
	Flags   []Flag
	Summary string // one line
	Help    string // longer paragraph(s)
	Group   string
	Hidden  bool
	Run     func(*Ctx) error
}

func (v *Verb) words() []string { return strings.Fields(v.Name) }

// Synopsis renders "axon <verb> <args> [flags]".
func (v *Verb) Synopsis(prog string) string {
	var b strings.Builder
	b.WriteString(prog + " " + v.Name)
	for _, a := range v.Args {
		n := "<" + a.Name + ">"
		if a.Variadic {
			n += "…"
		}
		if a.Optional {
			n = "[" + n + "]"
		}
		b.WriteString(" " + n)
	}
	if len(v.visibleFlags()) > 0 {
		b.WriteString(" [flags]")
	}
	return b.String()
}

func (v *Verb) visibleFlags() []Flag {
	var out []Flag
	for _, f := range v.Flags {
		if !f.Hidden {
			out = append(out, f)
		}
	}
	return out
}

// Moved describes a renamed verb that still works after a notice.
type Moved struct {
	Old     string
	New     string // shown in the notice
	Resolve func(args []string) (string, []string)
	Silent  bool // forward without a notice (a first-class shortcut)
}

// Registry holds verbs and moved aliases.
type Registry struct {
	Prog    string
	Version string
	Tagline string
	verbs   map[string]*Verb
	moved   map[string]*Moved
	Globals []Flag
	Common  []string // verb names shown in "common commands"
}

// New creates a registry.
func New(prog string) *Registry {
	return &Registry{Prog: prog, verbs: map[string]*Verb{}, moved: map[string]*Moved{}}
}

// Add registers verbs.
func (r *Registry) Add(vs ...*Verb) {
	for _, v := range vs {
		if _, dup := r.verbs[v.Name]; dup {
			panic("duplicate verb " + v.Name)
		}
		r.verbs[v.Name] = v
	}
}

// AddMoved registers a moved/alias verb.
func (r *Registry) AddMoved(m *Moved) { r.moved[m.Old] = m }

// Lookup returns a verb by exact name.
func (r *Registry) Lookup(name string) *Verb { return r.verbs[name] }

// Verbs returns all verbs sorted by name.
func (r *Registry) Verbs(includeHidden bool) []*Verb {
	var out []*Verb
	for _, v := range r.verbs {
		if v.Hidden && !includeHidden {
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Globals parsed from the command line.
type Globals struct {
	Help    bool
	Version bool
	Yes     bool
	Quiet   bool
	Verbose bool
	NoColor bool
	Compat  string
}

// Ctx is passed to a verb's Run.
type Ctx struct {
	Verb   *Verb
	Args   []string
	G      *Globals
	R      *Registry
	values map[string][]string
	set    map[string]bool
}

// Arg returns positional i or "".
func (c *Ctx) Arg(i int) string {
	if i < len(c.Args) {
		return c.Args[i]
	}
	return ""
}

// Has reports whether the flag was given.
func (c *Ctx) Has(name string) bool { return c.set[name] }

// String returns the last value of a flag (or its default).
func (c *Ctx) String(name string) string {
	if vs := c.values[name]; len(vs) > 0 {
		return vs[len(vs)-1]
	}
	for _, f := range c.Verb.Flags {
		if f.Name == name {
			return f.Default
		}
	}
	return ""
}

// Strings returns every value of a repeatable flag; comma lists are kept whole.
func (c *Ctx) Strings(name string) []string { return c.values[name] }

// Bool returns a boolean flag.
func (c *Ctx) Bool(name string) bool {
	v := c.String(name)
	b, _ := strconv.ParseBool(v)
	return b
}

// Int returns an int flag (0 if unset/invalid; validated at parse time).
func (c *Ctx) Int(name string) int {
	n, _ := strconv.Atoi(c.String(name))
	return n
}

// Run parses argv (without the program name) and executes. It returns the exit code.
func (r *Registry) Run(argv []string) int {
	err := r.dispatch(argv)
	if err == nil {
		return ExitOK
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		if !ee.Silent && ee.Msg != "" {
			fmt.Fprintf(ui.Stderr, "%s: %s\n", r.Prog, ee.Msg)
		}
		return ee.Code
	}
	fmt.Fprintf(ui.Stderr, "%s: %s\n", r.Prog, err.Error())
	return ExitFailure
}

func (r *Registry) globalFlag(tok string) (*Flag, string, bool) {
	name, val, hasVal := splitFlag(tok)
	for i := range r.Globals {
		f := &r.Globals[i]
		if name == "--"+f.Name || (f.Short != "" && name == "-"+f.Short) {
			return f, val, hasVal
		}
	}
	return nil, "", false
}

func splitFlag(tok string) (name, val string, hasVal bool) {
	if i := strings.IndexByte(tok, '='); i > 0 {
		return tok[:i], tok[i+1:], true
	}
	return tok, "", false
}

func leadingWords(rest []string) []string {
	var words []string
	for _, t := range rest {
		if strings.HasPrefix(t, "-") {
			break
		}
		words = append(words, t)
	}
	return words
}

func (r *Registry) extractGlobals(argv []string, skip map[string]bool) (*Globals, []string, error) {
	g := &Globals{Compat: os.Getenv("AXON_COMPAT")}
	var rest []string
	for i := 0; i < len(argv); i++ {
		tok := argv[i]
		if tok == "--" {
			rest = append(rest, argv[i:]...)
			break
		}
		if strings.HasPrefix(tok, "-") {
			if f, val, hasVal := r.globalFlag(tok); f != nil && !skip[f.Name] {
				if f.Kind != Bool && !hasVal {
					if i+1 >= len(argv) {
						return nil, nil, Usage("flag --%s needs a value", f.Name)
					}
					i++
					val = argv[i]
				}
				applyGlobal(g, f.Name, val, hasVal)
				continue
			}
		}
		rest = append(rest, tok)
	}
	return g, rest, nil
}

func (r *Registry) dispatch(argv []string) error {
	// Pass 1: pull global flags out from anywhere (before a bare "--"). A verb's
	// own flag wins over a global of the same name (release create --version).
	g, rest, err := r.extractGlobals(argv, nil)
	if err != nil {
		return err
	}
	if v, _ := r.resolve(leadingWords(rest)); v != nil {
		own := map[string]bool{}
		for _, f := range v.Flags {
			for _, gf := range r.Globals {
				// only a visible verb flag shadows a global (hidden aliases of a global, like a
				// deploy verb's --verbose, mean the same thing and must not swallow -v)
				if f.Name == gf.Name && !f.Hidden {
					own[f.Name] = true
				}
			}
		}
		if len(own) > 0 {
			if g, rest, err = r.extractGlobals(argv, own); err != nil {
				return err
			}
		}
	}
	if g.NoColor {
		ui.DisableColor()
	}
	ui.Quiet, ui.Verbose = g.Quiet, g.Verbose

	if g.Version {
		return r.runVerb("version", nil, g)
	}
	if len(rest) == 0 {
		r.PrintRootHelp()
		return nil
	}
	// leading words (non-flags) for verb resolution
	var words []string
	for _, t := range rest {
		if strings.HasPrefix(t, "-") {
			break
		}
		words = append(words, t)
	}
	if len(words) == 0 {
		if g.Help {
			r.PrintRootHelp()
			return nil
		}
		return Usage("unrecognized flag %s (run `%s help`)", rest[0], r.Prog)
	}
	if words[0] == "help" && len(words) == 1 {
		r.PrintRootHelp()
		return nil
	}
	if words[0] == "help" && len(words) > 1 {
		if v, n := r.resolve(words[1:]); v != nil && n == len(words)-1 {
			r.PrintVerbHelp(v)
			return nil
		}
		g.Help = true
		rest = rest[1:]
		words = words[1:]
	}
	v, n := r.resolve(words)
	if v == nil {
		// moved / alias verbs
		if m, n2 := r.resolveMoved(words); m != nil {
			newName, newArgs := m.New, append([]string{}, rest[n2:]...)
			if m.Resolve != nil {
				newName, newArgs = m.Resolve(newArgs)
			}
			if !m.Silent {
				fmt.Fprintf(ui.Stderr, "%s: %q is now %q; the old name will be removed in a later release\n", r.Prog, m.Old, newName)
			}
			target := r.verbs[newName]
			if target == nil {
				return Usage("unknown verb %q", newName)
			}
			return r.exec(target, newArgs, g)
		}
		msg := fmt.Sprintf("unknown verb %q", strings.Join(argv, " "))
		if s := r.Suggest(words[0]); s != "" {
			msg += fmt.Sprintf(" (did you mean %q?)", s)
		}
		return Usage("%s", msg)
	}
	return r.exec(v, rest[n:], g)
}

func applyGlobal(g *Globals, name, val string, hasVal bool) {
	b := true
	if hasVal {
		b, _ = strconv.ParseBool(val)
	}
	switch name {
	case "help":
		g.Help = b
	case "version":
		g.Version = b
	case "yes":
		g.Yes = b
	case "quiet":
		g.Quiet = b
	case "verbose":
		g.Verbose = b
	case "no-color":
		g.NoColor = b
	case "compat":
		g.Compat = val
	}
}

func (r *Registry) resolve(words []string) (*Verb, int) {
	for n := min(3, len(words)); n >= 1; n-- {
		if v := r.verbs[strings.Join(words[:n], " ")]; v != nil {
			return v, n
		}
	}
	return nil, 0
}

func (r *Registry) resolveMoved(words []string) (*Moved, int) {
	for n := min(3, len(words)); n >= 1; n-- {
		if m := r.moved[strings.Join(words[:n], " ")]; m != nil {
			return m, n
		}
	}
	return nil, 0
}

// Suggest implements the did-you-mean rule: among registered verbs, then moved
// names, keep those starting with the typed first word; pick the shortest, ties
// broken by sort order.
func (r *Registry) Suggest(first string) string {
	pick := func(names []string) string {
		sort.Strings(names)
		best := ""
		for _, n := range names {
			if strings.HasPrefix(n, first) && (best == "" || len(n) < len(best)) {
				best = n
			}
		}
		return best
	}
	var vs []string
	for _, v := range r.verbs {
		if !v.Hidden {
			vs = append(vs, v.Name)
		}
	}
	if s := pick(vs); s != "" {
		return s
	}
	var ms []string
	for k := range r.moved {
		ms = append(ms, k)
	}
	return pick(ms)
}

func (r *Registry) runVerb(name string, args []string, g *Globals) error {
	v := r.verbs[name]
	if v == nil {
		return Usage("unknown verb %q", name)
	}
	return r.exec(v, args, g)
}

func (r *Registry) exec(v *Verb, args []string, g *Globals) error {
	if g.Help {
		r.PrintVerbHelp(v)
		return nil
	}
	c := &Ctx{Verb: v, G: g, R: r, values: map[string][]string{}, set: map[string]bool{}}
	byName := map[string]*Flag{}
	for i := range v.Flags {
		f := &v.Flags[i]
		byName["--"+f.Name] = f
		for _, a := range f.Alias {
			byName["--"+a] = f
		}
		if f.Short != "" {
			byName["-"+f.Short] = f
		}
	}
	var pos []string
	flagsDone := false
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if flagsDone || !strings.HasPrefix(tok, "-") || tok == "-" {
			pos = append(pos, tok)
			continue
		}
		if tok == "--" {
			flagsDone = true
			continue
		}
		name, val, hasVal := splitFlag(tok)
		f := byName[name]
		if f == nil {
			return Usage("%s: unrecognized flag %s", v.Name, name)
		}
		if f.Kind == Bool {
			if !hasVal {
				val = "true"
			} else if _, err := strconv.ParseBool(val); err != nil {
				return Usage("%s: flag --%s takes no value (or true/false), got %q", v.Name, f.Name, val)
			}
		} else if !hasVal {
			if i+1 >= len(args) {
				return Usage("%s: flag --%s needs a value", v.Name, f.Name)
			}
			i++
			val = args[i]
		}
		if f.Kind == Int {
			if _, err := strconv.Atoi(val); err != nil {
				return Usage("%s: flag --%s wants a number, got %q", v.Name, f.Name, val)
			}
		}
		c.set[f.Name] = true
		if f.Kind == Multi {
			c.values[f.Name] = append(c.values[f.Name], val)
		} else {
			c.values[f.Name] = []string{val}
		}
	}
	// positional validation
	var missing []string
	maxPos, variadic := 0, false
	for i, a := range v.Args {
		if a.Variadic {
			variadic = true
		}
		maxPos++
		if !a.Optional && i >= len(pos) {
			missing = append(missing, a.Name)
		}
	}
	if len(missing) > 0 {
		return Usage("%s: missing required argument(s): %s", v.Name, strings.Join(missing, ", "))
	}
	if !variadic && len(pos) > maxPos {
		return Usage("%s: unexpected argument %q", v.Name, pos[maxPos])
	}
	c.Args = pos
	if v.Run == nil {
		return fmt.Errorf("%s: not implemented", v.Name)
	}
	return v.Run(c)
}
