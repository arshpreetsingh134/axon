// Package ui owns axon's terminal output conventions.
//
// Results go to stdout; progress and diagnostics go to stderr. Status glyphs:
// ✓ success, ✗ blocking problem, ! warning, · note. Color is used only when the
// stream is a terminal and NO_COLOR is unset.
package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"golang.org/x/term"
)

var (
	mu      sync.Mutex
	Stdout  io.Writer = os.Stdout
	Stderr  io.Writer = os.Stderr
	Quiet   bool
	Verbose bool
	colorOn = detectColor()
)

func detectColor() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("AXON_NO_COLOR") != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return IsTerminal(os.Stderr)
}

// DisableColor turns color off (the --no-color flag).
func DisableColor() { colorOn = false }

// IsTerminal reports whether f is an interactive terminal (not /dev/null or a pipe).
func IsTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// StdinIsTerminal reports whether interactive confirmation is possible.
func StdinIsTerminal() bool { return IsTerminal(os.Stdin) }

func paint(code, s string) string {
	if !colorOn {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func Green(s string) string  { return paint("32", s) }
func Red(s string) string    { return paint("31", s) }
func Yellow(s string) string { return paint("33", s) }
func Dim(s string) string    { return paint("2", s) }
func Bold(s string) string   { return paint("1", s) }
func Cyan(s string) string   { return paint("36", s) }

func errf(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	fmt.Fprintf(Stderr, format, a...)
}

// OK prints "✓ msg" to stderr.
func OK(format string, a ...any) {
	if Quiet {
		return
	}
	errf("%s %s\n", Green("✓"), fmt.Sprintf(format, a...))
}

// Fail prints "✗ msg" to stderr (never suppressed).
func Fail(format string, a ...any) { errf("%s %s\n", Red("✗"), fmt.Sprintf(format, a...)) }

// Warn prints "! warn: msg" to stderr (never suppressed).
func Warn(format string, a ...any) {
	errf("%s %s\n", Yellow("! warn:"), fmt.Sprintf(format, a...))
}

// Note prints "· msg" to stderr.
func Note(format string, a ...any) {
	if Quiet {
		return
	}
	errf("%s %s\n", Dim("·"), fmt.Sprintf(format, a...))
}

// Step prints a top-level progress line "▶ msg" to stderr.
func Step(format string, a ...any) {
	if Quiet {
		return
	}
	errf("%s %s\n", Cyan("▶"), fmt.Sprintf(format, a...))
}

// Sub prints an indented progress line to stderr.
func Sub(format string, a ...any) {
	if Quiet {
		return
	}
	errf("  %s\n", fmt.Sprintf(format, a...))
}

// Debug prints only with --verbose.
func Debug(format string, a ...any) {
	if !Verbose {
		return
	}
	errf("%s\n", Dim("  "+fmt.Sprintf(format, a...)))
}

// Out prints a result line to stdout.
func Out(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	fmt.Fprintf(Stdout, format+"\n", a...)
}

// Raw writes s to stdout unchanged.
func Raw(s string) {
	mu.Lock()
	defer mu.Unlock()
	fmt.Fprint(Stdout, s)
}

// JSON prints v as one pretty (2-space) JSON document on stdout.
func JSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	Raw(string(b) + "\n")
	return nil
}

// Table renders rows with aligned columns (first row is the header when header=true).
func Table(w io.Writer, rows [][]string, header bool) {
	if len(rows) == 0 {
		return
	}
	widths := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, c := range r {
			if i < len(widths) && runeLen(c) > widths[i] {
				widths[i] = runeLen(c)
			}
		}
	}
	for ri, r := range rows {
		var b strings.Builder
		for i, c := range r {
			if i == len(r)-1 {
				b.WriteString(c)
				break
			}
			b.WriteString(c)
			b.WriteString(strings.Repeat(" ", widths[i]-runeLen(c)+2))
		}
		line := strings.TrimRight(b.String(), " ")
		if header && ri == 0 {
			line = Bold(line)
		}
		fmt.Fprintln(w, line)
	}
}

func runeLen(s string) int { return len([]rune(s)) }

// Confirm asks a y/N question on stderr. Without a TTY it returns false unless
// assumeYes is set.
func Confirm(question string, assumeYes bool) bool {
	if assumeYes {
		return true
	}
	if !StdinIsTerminal() {
		return false
	}
	errf("%s [y/N] ", question)
	var ans string
	fmt.Fscanln(os.Stdin, &ans)
	ans = strings.ToLower(strings.TrimSpace(ans))
	return ans == "y" || ans == "yes"
}
