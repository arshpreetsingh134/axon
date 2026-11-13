package ci

import (
	"regexp"
	"strings"
)

var tsRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z ?`)

// stripLine removes the timestamp prefix of a GitHub Actions log line.
func stripLine(l string) string { return tsRE.ReplaceAllString(strings.TrimRight(l, "\r"), "") }

// FailureExcerpt returns the part of a job log that explains a failure: the
// failing step's output up to its first error marker, without timestamps and
// runner noise, capped at max lines (the most recent ones are kept).
func FailureExcerpt(log string, max int) string {
	if max <= 0 {
		max = 60
	}
	raw := strings.Split(log, "\n")
	lines := make([]string, len(raw))
	for i, l := range raw {
		lines[i] = stripLine(l)
	}
	errAt := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "##[error]") {
			errAt = i
			break
		}
	}
	if errAt < 0 {
		// no marker: the tail is the best evidence
		start := len(lines) - max
		if start < 0 {
			start = 0
		}
		return clean(lines[start:])
	}
	// start at the step that failed (the last "##[group]Run" before the error)
	start := 0
	for i := errAt; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "##[group]Run ") {
			start = i
			break
		}
	}
	section := lines[start : errAt+1]
	if len(section) > max {
		section = append([]string{section[0], "…"}, section[len(section)-max+2:]...)
	}
	return clean(section)
}

func clean(ls []string) string {
	var out []string
	for _, l := range ls {
		switch {
		case strings.HasPrefix(l, "##[group]Run "):
			out = append(out, "$ "+strings.TrimPrefix(l, "##[group]Run "))
		case strings.HasPrefix(l, "##[group]"), strings.HasPrefix(l, "##[endgroup]"):
			continue
		case strings.HasPrefix(l, "##[error]"):
			out = append(out, "ERROR: "+strings.TrimPrefix(l, "##[error]"))
		default:
			out = append(out, l)
		}
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}
