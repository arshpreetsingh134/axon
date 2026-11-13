// Package sh runs external tools (docker, kubectl, go, npm, git) with consistent
// error reporting.
package sh

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"axon/internal/ui"
)

// Cmd describes one invocation.
type Cmd struct {
	Name    string
	Args    []string
	Dir     string
	Env     []string // extra KEY=VALUE (added to os.Environ)
	Stdin   string
	Timeout time.Duration
	Stream  bool      // stream output to stderr (prefixed) instead of capturing
	Prefix  string    // prefix for streamed lines
	Out     io.Writer // when set, stdout goes here (e.g. ui.Stdout for logs)
}

// Result of a run.
type Result struct {
	Stdout string
	Stderr string
	Code   int
}

// Error wraps a failed command with its output tail.
type Error struct {
	Cmd  string
	Code int
	Tail string
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s failed (exit %d)", e.Cmd, e.Code)
	if e.Tail != "" {
		msg += ":\n" + indent(e.Tail)
	}
	return msg
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = "    " + lines[i]
	}
	return strings.Join(lines, "\n")
}

// Tail returns the last n lines of s.
func Tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// String renders the command line.
func (c Cmd) String() string { return strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " ")) }

type prefixWriter struct {
	w      io.Writer
	prefix string
	buf    bytes.Buffer
	keep   *bytes.Buffer
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.keep.Write(b)
	p.buf.Write(b)
	for {
		line, err := p.buf.ReadString('\n')
		if err != nil {
			p.buf.WriteString(line)
			break
		}
		fmt.Fprint(p.w, p.prefix+line)
	}
	return len(b), nil
}

// Run executes the command.
func Run(c Cmd) (*Result, error) {
	ctx := context.Background()
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	if c.Stdin != "" {
		cmd.Stdin = strings.NewReader(c.Stdin)
	}
	var so, se bytes.Buffer
	switch {
	case c.Out != nil:
		cmd.Stdout = c.Out
		cmd.Stderr = ui.Stderr
	case c.Stream:
		cmd.Stdout = &prefixWriter{w: ui.Stderr, prefix: c.Prefix, keep: &so}
		cmd.Stderr = &prefixWriter{w: ui.Stderr, prefix: c.Prefix, keep: &se}
	default:
		cmd.Stdout, cmd.Stderr = &so, &se
	}
	ui.Debug("$ %s", c.String())
	err := cmd.Run()
	r := &Result{Stdout: so.String(), Stderr: se.String()}
	if err != nil {
		code := -1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		r.Code = code
		if ctx.Err() == context.DeadlineExceeded {
			return r, &Error{Cmd: c.String(), Code: code, Tail: "timed out after " + c.Timeout.String()}
		}
		if _, nf := err.(*exec.Error); nf {
			return r, fmt.Errorf("%s: not found on PATH", c.Name)
		}
		tail := Tail(r.Stderr, 20)
		if strings.TrimSpace(r.Stderr) == "" {
			tail = Tail(r.Stdout, 20)
		}
		return r, &Error{Cmd: c.String(), Code: code, Tail: tail}
	}
	return r, nil
}

// Output runs and returns trimmed stdout.
func Output(name string, args ...string) (string, error) {
	r, err := Run(Cmd{Name: name, Args: args, Timeout: 60 * time.Second})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(r.Stdout), nil
}

// Interactive runs attached to the terminal (stdin/stdout/stderr).
func Interactive(dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	ui.Debug("$ %s %s", name, strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return &Error{Cmd: name + " " + strings.Join(args, " "), Code: ee.ExitCode()}
		}
		return err
	}
	return nil
}

// Has reports whether a tool is on PATH.
func Has(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
