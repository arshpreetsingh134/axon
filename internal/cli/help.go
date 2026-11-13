package cli

import (
	"fmt"
	"sort"
	"strings"

	"axon/internal/ui"
)

const wrapWidth = 100

// Wrap word-wraps s to width with the given indent on continuation lines.
func Wrap(s string, width int, indent string) string {
	var out strings.Builder
	for pi, para := range strings.Split(s, "\n") {
		if pi > 0 {
			out.WriteString("\n")
		}
		words := strings.Fields(para)
		line := 0
		lead := ""
		if strings.HasPrefix(para, "  ") {
			lead = para[:len(para)-len(strings.TrimLeft(para, " "))]
			out.WriteString(lead)
			line = len(lead)
		}
		for i, w := range words {
			if i > 0 && line+1+len([]rune(w)) > width {
				out.WriteString("\n" + indent + lead)
				line = len(indent) + len(lead)
			} else if i > 0 {
				out.WriteString(" ")
				line++
			}
			out.WriteString(w)
			line += len([]rune(w))
		}
	}
	return out.String()
}

// PrintRootHelp prints the top-level help on stdout.
func (r *Registry) PrintRootHelp() {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s — %s\n\n", r.Prog, r.Version, r.Tagline)
	fmt.Fprintf(&b, "usage: %s <verb> [args] [--flags]\n\n", r.Prog)
	if len(r.Common) > 0 {
		b.WriteString("common commands:\n")
		var rows [][]string
		for _, n := range r.Common {
			if v := r.verbs[n]; v != nil {
				rows = append(rows, []string{"  " + r.Prog + " " + v.Name, v.Summary})
			}
		}
		b.WriteString(table(rows))
		b.WriteString("\n")
	}
	var moved []string
	for k, m := range r.moved {
		if !m.Silent {
			moved = append(moved, fmt.Sprintf("  %-22s → %s", k, m.New))
		}
	}
	if len(moved) > 0 {
		sort.Strings(moved)
		b.WriteString("moved (still work, with a notice):\n")
		b.WriteString(strings.Join(moved, "\n") + "\n\n")
	}
	groups := map[string][]*Verb{}
	var order []string
	for _, v := range r.Verbs(false) {
		g := v.Group
		if g == "" {
			g = strings.Fields(v.Name)[0]
		}
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], v)
	}
	sort.Strings(order)
	b.WriteString("all verbs:\n")
	for _, g := range order {
		fmt.Fprintf(&b, "\n  %s\n", ui.Bold(g))
		for _, v := range groups[g] {
			b.WriteString(column("    "+v.Synopsis(r.Prog), v.Summary, 54))
		}
	}
	b.WriteString("\nglobal flags:\n")
	var rows [][]string
	for _, f := range r.Globals {
		if f.Hidden {
			continue
		}
		n := "  --" + f.Name
		if f.Short != "" {
			n += ", -" + f.Short
		}
		if f.Kind != Bool {
			n += " " + placeholder(f)
		}
		rows = append(rows, []string{n, f.Usage})
	}
	b.WriteString(table(rows))
	fmt.Fprintf(&b, "\nRun `%s <verb> --help` for a verb's flags.\n", r.Prog)
	ui.Raw(b.String())
}

func placeholder(f Flag) string {
	if f.Value != "" {
		return f.Value
	}
	switch f.Kind {
	case Int:
		return "<n>"
	case Multi:
		return "<value> (repeatable)"
	}
	return "<value>"
}

func table(rows [][]string) string {
	w := 0
	for _, r := range rows {
		if l := len([]rune(r[0])); l > w && l <= 40 {
			w = l
		}
	}
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(column(r[0], r[1], w))
	}
	return b.String()
}

// column renders "head<pad>desc" with desc wrapped and aligned at width w+2.
func column(head, desc string, w int) string {
	var b strings.Builder
	indent := strings.Repeat(" ", w+2)
	lines := strings.Split(Wrap(desc, wrapWidth-w-2, ""), "\n")
	if len([]rune(head)) > w {
		b.WriteString(head + "\n" + indent)
	} else {
		b.WriteString(head + strings.Repeat(" ", w-len([]rune(head))+2))
	}
	for i, l := range lines {
		if i > 0 {
			b.WriteString("\n" + indent)
		}
		b.WriteString(l)
	}
	b.WriteString("\n")
	return b.String()
}

// PrintVerbHelp prints a verb's help on stdout.
func (r *Registry) PrintVerbHelp(v *Verb) {
	var b strings.Builder
	fmt.Fprintf(&b, "usage: %s\n\n", v.Synopsis(r.Prog))
	text := v.Summary
	if v.Help != "" {
		text = v.Help
	}
	b.WriteString(Wrap(text, wrapWidth, "") + "\n")
	if fl := v.visibleFlags(); len(fl) > 0 {
		b.WriteString("\nflags:\n")
		var rows [][]string
		for _, f := range fl {
			n := "  --" + f.Name
			if f.Kind != Bool {
				n += " " + placeholder(f)
			}
			u := f.Usage
			if f.Default != "" && f.Kind != Bool {
				u += fmt.Sprintf(" (default %s)", f.Default)
			}
			if len(f.Alias) > 0 {
				u += " (also --" + strings.Join(f.Alias, ", --") + ")"
			}
			rows = append(rows, []string{n, u})
		}
		b.WriteString(table(rows))
	}
	ui.Raw(b.String())
}

// Completion renders a shell completion script from the live registry.
func (r *Registry) Completion(shell string) (string, error) {
	verbs := r.Verbs(false)
	tops := map[string]bool{}
	subs := map[string]map[string]bool{}
	flagSet := map[string]bool{}
	for _, v := range verbs {
		w := strings.Fields(v.Name)
		tops[w[0]] = true
		if len(w) > 1 {
			if subs[w[0]] == nil {
				subs[w[0]] = map[string]bool{}
			}
			subs[w[0]][w[1]] = true
		}
		for _, f := range v.Flags {
			if !f.Hidden {
				flagSet["--"+f.Name] = true
			}
		}
	}
	for _, f := range r.Globals {
		flagSet["--"+f.Name] = true
	}
	keys := func(m map[string]bool) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	top := strings.Join(keys(tops), " ")
	flags := strings.Join(keys(flagSet), " ")
	p := r.Prog
	switch shell {
	case "bash":
		var cases strings.Builder
		for _, t := range keys(tops) {
			if s := subs[t]; s != nil {
				fmt.Fprintf(&cases, "    %s) COMPREPLY=($(compgen -W \"%s\" -- \"$cur\")); return;;\n", t, strings.Join(keys(s), " "))
			}
		}
		return fmt.Sprintf(`# bash completion for %[1]s (generated from the verb registry)
_%[1]s() {
  local cur="${COMP_WORDS[COMP_CWORD]}"
  if [[ "$cur" == -* ]]; then COMPREPLY=($(compgen -W "%[3]s" -- "$cur")); return; fi
  if [[ $COMP_CWORD -eq 1 ]]; then COMPREPLY=($(compgen -W "%[2]s" -- "$cur")); return; fi
  if [[ $COMP_CWORD -eq 2 ]]; then
    case "${COMP_WORDS[1]}" in
%[4]s    esac
  fi
  COMPREPLY=($(compgen -f -- "$cur"))
}
complete -F _%[1]s %[1]s
`, p, top, flags, cases.String()), nil
	case "zsh":
		var cases strings.Builder
		for _, t := range keys(tops) {
			if s := subs[t]; s != nil {
				fmt.Fprintf(&cases, "      %s) compadd -- %s;;\n", t, strings.Join(keys(s), " "))
			}
		}
		return fmt.Sprintf(`#compdef %[1]s
# zsh completion for %[1]s (generated from the verb registry)
_%[1]s() {
  if [[ "$PREFIX" == -* ]]; then compadd -- %[3]s; return; fi
  if (( CURRENT == 2 )); then compadd -- %[2]s; return; fi
  if (( CURRENT == 3 )); then
    case "$words[2]" in
%[4]s    esac
    return
  fi
  _files
}
compdef _%[1]s %[1]s
`, p, top, flags, cases.String()), nil
	case "fish":
		var b strings.Builder
		fmt.Fprintf(&b, "# fish completion for %s (generated from the verb registry)\n", p)
		fmt.Fprintf(&b, "complete -c %s -f -n '__fish_use_subcommand' -a '%s'\n", p, top)
		for _, t := range keys(tops) {
			if s := subs[t]; s != nil {
				fmt.Fprintf(&b, "complete -c %s -f -n '__fish_seen_subcommand_from %s' -a '%s'\n", p, t, strings.Join(keys(s), " "))
			}
		}
		for _, f := range keys(flagSet) {
			fmt.Fprintf(&b, "complete -c %s -l %s\n", p, strings.TrimPrefix(f, "--"))
		}
		return b.String(), nil
	}
	return "", Usage("completion: unsupported shell %q (bash, zsh or fish)", shell)
}
