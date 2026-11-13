// Package journey runs declarative HTTP journeys (journeys/*.toml) against a
// deployed cluster through its front door.
package journey

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"axon/internal/dotenv"
)

// Expect is a step expectation.
type Expect struct {
	Status   int               `toml:"status" json:"status,omitempty"`
	JSON     any               `toml:"json" json:"json,omitempty"`
	Contains string            `toml:"contains" json:"contains,omitempty"`
	Headers  map[string]string `toml:"headers" json:"headers,omitempty"`
}

// Step is one request.
type Step struct {
	Name    string            `toml:"name"`
	Method  string            `toml:"method"`
	Path    string            `toml:"path"`
	Body    any               `toml:"body"`
	Headers map[string]string `toml:"headers"`
	Expect  *Expect           `toml:"expect"`
}

// Journey is one file.
type Journey struct {
	Meta struct {
		Name     string `toml:"name"`
		App      string `toml:"app"`
		Actor    string `toml:"actor"`
		Password string `toml:"password"`
		Level    string `toml:"level"`
	} `toml:"journey"`
	Steps []Step `toml:"step"`
	Path  string `toml:"-"`
}

// Load reads every journey under dir.
func Load(dir string) ([]*Journey, error) {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.toml"))
	sort.Strings(paths)
	var out []*Journey
	for _, p := range paths {
		var j Journey
		if _, err := toml.DecodeFile(p, &j); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		j.Path = p
		if j.Meta.Name == "" {
			j.Meta.Name = strings.TrimSuffix(filepath.Base(p), ".toml")
		}
		out = append(out, &j)
	}
	return out, nil
}

// Lint validates journeys offline.
func Lint(js []*Journey, hosts map[string]bool) []string {
	var problems []string
	for _, j := range js {
		f := filepath.Base(j.Path)
		if j.Meta.App == "" {
			problems = append(problems, f+": [journey].app is required")
		} else if hosts != nil && !hosts[j.Meta.App] {
			problems = append(problems, fmt.Sprintf("%s: app %q is not a routed host of this cluster", f, j.Meta.App))
		}
		if len(j.Steps) == 0 {
			problems = append(problems, f+": no [[step]]")
		}
		for i, s := range j.Steps {
			if s.Path == "" || !strings.HasPrefix(s.Path, "/") {
				problems = append(problems, fmt.Sprintf("%s: step %d (%s): path must start with /", f, i+1, s.Name))
			}
			switch strings.ToUpper(s.Method) {
			case "", "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD":
			default:
				problems = append(problems, fmt.Sprintf("%s: step %d: unknown method %q", f, i+1, s.Method))
			}
		}
	}
	return problems
}

// StepResult is the outcome of one step.
type StepResult struct {
	Name   string `json:"name"`
	Status int    `json:"status"`
	Pass   bool   `json:"pass"`
	Reason string `json:"reason,omitempty"`
	Millis int64  `json:"ms"`
}

// Result is the outcome of one journey.
type Result struct {
	Name    string       `json:"name"`
	App     string       `json:"app"`
	Pass    bool         `json:"pass"`
	Skipped string       `json:"skipped,omitempty"`
	Steps   []StepResult `json:"steps"`
}

// Runner executes journeys against a front door on 127.0.0.1:<port>.
type Runner struct {
	Port   int
	Domain string
	Vars   map[string]string
	Client *http.Client
}

// Run executes one journey.
func (r *Runner) Run(j *Journey) Result {
	res := Result{Name: j.Meta.Name, App: j.Meta.App, Pass: true}
	if r.Client == nil {
		r.Client = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	host := j.Meta.App + "." + r.Domain
	for _, s := range j.Steps {
		sr := r.step(host, s)
		res.Steps = append(res.Steps, sr)
		if !sr.Pass {
			res.Pass = false
			break
		}
	}
	return res
}

func (r *Runner) expand(s string) string {
	out, _ := dotenv.Expand(s, r.Vars)
	return out
}

func (r *Runner) step(host string, s Step) StepResult {
	sr := StepResult{Name: s.Name}
	method := strings.ToUpper(s.Method)
	if method == "" {
		method = "GET"
	}
	var body io.Reader
	if s.Body != nil {
		b, _ := json.Marshal(s.Body)
		body = bytes.NewReader([]byte(r.expand(string(b))))
	}
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", r.Port, r.expand(s.Path)), body)
	if err != nil {
		sr.Reason = err.Error()
		return sr
	}
	req.Host = host
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range s.Headers {
		req.Header.Set(k, r.expand(v))
	}
	start := time.Now()
	resp, err := r.Client.Do(req)
	sr.Millis = time.Since(start).Milliseconds()
	if err != nil {
		sr.Reason = err.Error()
		return sr
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	sr.Status = resp.StatusCode
	exp := s.Expect
	if exp == nil {
		exp = &Expect{}
	}
	if exp.Status != 0 {
		if resp.StatusCode != exp.Status {
			sr.Reason = fmt.Sprintf("status %d, want %d", resp.StatusCode, exp.Status)
			return sr
		}
	} else if resp.StatusCode < 200 || resp.StatusCode > 299 {
		sr.Reason = fmt.Sprintf("status %d, want 2xx", resp.StatusCode)
		return sr
	}
	if exp.Contains != "" && !strings.Contains(string(raw), exp.Contains) {
		sr.Reason = fmt.Sprintf("body does not contain %q", exp.Contains)
		return sr
	}
	for k, v := range exp.Headers {
		if got := resp.Header.Get(k); got != v {
			sr.Reason = fmt.Sprintf("header %s = %q, want %q", k, got, v)
			return sr
		}
	}
	if exp.JSON != nil {
		var got any
		if err := json.Unmarshal(raw, &got); err != nil {
			sr.Reason = "response is not JSON"
			return sr
		}
		if path, ok := Match(exp.JSON, got, "$"); !ok {
			sr.Reason = "json mismatch at " + path
			return sr
		}
	}
	sr.Pass = true
	return sr
}

// Match reports whether got satisfies the subset pattern want. Objects match
// key by key (extra keys allowed); an array pattern matches when every pattern
// element is satisfied by some element of the actual array; scalars compare by
// value (numbers numerically).
func Match(want, got any, path string) (string, bool) {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return path, false
		}
		keys := make([]string, 0, len(w))
		for k := range w {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			gv, ok := g[k]
			if !ok {
				return path + "." + k, false
			}
			if p, ok := Match(w[k], gv, path+"."+k); !ok {
				return p, false
			}
		}
		return "", true
	case []any:
		g, ok := got.([]any)
		if !ok {
			return path, false
		}
		for i, wp := range w {
			found := false
			for _, gv := range g {
				if _, ok := Match(wp, gv, path); ok {
					found = true
					break
				}
			}
			if !found {
				return fmt.Sprintf("%s[%d]", path, i), false
			}
		}
		return "", true
	case []map[string]any:
		l := make([]any, len(w))
		for i := range w {
			l[i] = w[i]
		}
		return Match(l, got, path)
	case int64:
		return path, num(got) == float64(w) && isNum(got)
	case int:
		return path, num(got) == float64(w) && isNum(got)
	case float64:
		return path, num(got) == w && isNum(got)
	case string:
		g, ok := got.(string)
		return path, ok && g == w
	case bool:
		g, ok := got.(bool)
		return path, ok && g == w
	case nil:
		return path, got == nil
	default:
		if fmt.Sprint(want) == fmt.Sprint(got) {
			return "", true
		}
		return path, false
	}
}

func isNum(v any) bool {
	switch v.(type) {
	case float64, int64, int:
		return true
	}
	return false
}

func num(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int64:
		return float64(t)
	case int:
		return float64(t)
	}
	return 0
}

// LastRun is verify/last-run.json.
type LastRun struct {
	At       string   `json:"at"`
	Cluster  string   `json:"cluster"`
	Env      string   `json:"env"`
	Pass     bool     `json:"pass"`
	Probes   []Probe  `json:"probes"`
	Journeys []Result `json:"journeys"`
}

// Probe is one /api/system probe.
type Probe struct {
	App    string `json:"app"`
	Kind   string `json:"kind"`
	URL    string `json:"url"`
	Status int    `json:"status"`
	Pass   bool   `json:"pass"`
	Report any    `json:"report,omitempty"`
}

// ProbeSystem GETs /api/system on a cored app and checks its modules are SERVING.
func (r *Runner) ProbeSystem(app, host string) Probe {
	p := Probe{App: app, Kind: "core", URL: fmt.Sprintf("http://%s.%s:%d/api/system", host, r.Domain, r.Port)}
	if r.Client == nil {
		r.Client = &http.Client{Timeout: 10 * time.Second}
	}
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/api/system", r.Port), nil)
	req.Host = host + "." + r.Domain
	resp, err := r.Client.Do(req)
	if err != nil {
		return p
	}
	defer resp.Body.Close()
	p.Status = resp.StatusCode
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var rep any
	if json.Unmarshal(raw, &rep) == nil {
		p.Report = rep
	}
	p.Pass = resp.StatusCode == 200
	if m, ok := rep.(map[string]any); ok {
		if mods, ok := m["modules"].([]any); ok {
			for _, mod := range mods {
				if mm, ok := mod.(map[string]any); ok {
					if h, ok := mm["health"].(string); ok && h != "SERVING" {
						p.Pass = false
					}
				}
			}
		}
	}
	return p
}

// Save writes verify/last-run.json.
func Save(dir string, lr *LastRun) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(lr, "", "  ")
	return os.WriteFile(filepath.Join(dir, "last-run.json"), append(b, '\n'), 0o644)
}
