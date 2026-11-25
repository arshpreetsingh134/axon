package commands

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"axon/internal/descriptor"
	"axon/internal/official"
	"axon/internal/sh"
	"axon/internal/ui"
)

// deriveLocalBuilds reads the cluster's GitHub build workflows and returns, for every image-backed member a workflow
// builds, the recipe to build it from the developer's checkouts the same way (SPEC.md Appendix C.12): the workflow's
// Dockerfile, context, build stage, build args and named contexts, with each checked-out repository replaced by the
// local checkout whose origin is that repository. Recipes of the overlay win, member by member; notes explain members
// or workflows that could not be derived.
func deriveLocalBuilds(clusterDir, workspace string, l *official.Layout) ([]descriptor.LocalBuild, []string) {
	members, _ := l.Materializations()
	byRepo := map[string]string{} // image repository → member
	for _, m := range members {
		for _, img := range m.Images {
			byRepo[imageRepo(img)] = m.Name
		}
	}
	files, _ := filepath.Glob(filepath.Join(clusterDir, ".github", "workflows", "*.yml"))
	sort.Strings(files)
	checkouts := &checkoutIndex{root: workspace}
	var out []descriptor.LocalBuild
	var notes []string
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasPrefix(filepath.Base(f), "mycel-") {
			continue // the generated release and deploy workflows build nothing a member runs
		}
		recipes, n := deriveFromWorkflow(f, clusterDir, byRepo, checkouts)
		notes = append(notes, n...)
		for _, r := range recipes {
			if seen[r.Targets[0]] {
				continue
			}
			seen[r.Targets[0]] = true
			out = append(out, r)
		}
	}
	return out, notes
}

type ghWorkflow struct {
	Jobs map[string]struct {
		Strategy struct {
			Matrix map[string]any `yaml:"matrix"`
		} `yaml:"strategy"`
		Steps []struct {
			Uses string         `yaml:"uses"`
			With map[string]any `yaml:"with"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func deriveFromWorkflow(file, clusterDir string, byRepo map[string]string, idx *checkoutIndex) ([]descriptor.LocalBuild, []string) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, nil
	}
	var wf ghWorkflow
	if err := yaml.Unmarshal(b, &wf); err != nil {
		return nil, []string{fmt.Sprintf("%s: %v", filepath.Base(file), err)}
	}
	var out []descriptor.LocalBuild
	var notes []string
	for _, job := range wf.Jobs {
		for _, mx := range matrixRows(job.Strategy.Matrix) {
			ev := func(s string) string { return evalGH(s, mx) }
			paths := map[string]string{} // runner path → local directory
			for _, st := range job.Steps {
				if !strings.HasPrefix(st.Uses, "actions/checkout@") {
					continue
				}
				p := strings.Trim(ev(str(st.With["path"])), "./")
				repo := ev(str(st.With["repository"]))
				if repo == "" {
					paths[p] = clusterDir
					continue
				}
				if dir := idx.find(repo); dir != "" {
					paths[p] = dir
				} else {
					notes = append(notes, fmt.Sprintf("%s: no local checkout of %s", filepath.Base(file), repo))
				}
			}
			local := func(runnerPath string) string {
				rp := strings.TrimPrefix(strings.TrimSpace(runnerPath), "./")
				best := ""
				for p := range paths {
					if (rp == p || strings.HasPrefix(rp, p+"/") || p == "") && len(p) >= len(best) {
						best = p
					}
				}
				dir, ok := paths[best]
				if !ok {
					return ""
				}
				return filepath.Join(dir, strings.TrimPrefix(strings.TrimPrefix(rp, best), "/"))
			}
			for _, st := range job.Steps {
				if !strings.HasPrefix(st.Uses, "docker/build-push-action@") {
					continue
				}
				member := ""
				for _, tag := range strings.FieldsFunc(ev(str(st.With["tags"])), func(r rune) bool { return r == ',' || r == '\n' }) {
					if m := byRepo[imageRepo(strings.TrimSpace(tag))]; m != "" {
						member = m
					}
				}
				if member == "" {
					continue
				}
				r := descriptor.LocalBuild{Targets: []string{member}, Dockerfile: local(ev(str(st.With["file"]))),
					Context: local(ev(str(st.With["context"]))), Target: ev(str(st.With["target"])),
					Contexts: map[string]string{}, Args: map[string]string{}}
				for _, line := range lines(ev(str(st.With["build-contexts"]))) {
					if k, v, ok := strings.Cut(line, "="); ok {
						r.Contexts[strings.TrimSpace(k)] = local(v)
					}
				}
				for _, line := range lines(ev(str(st.With["build-args"]))) {
					if k, v, ok := strings.Cut(line, "="); ok {
						r.Args[strings.TrimSpace(k)] = strings.TrimSpace(v)
					}
				}
				if r.Dockerfile == "" || r.Context == "" {
					notes = append(notes, fmt.Sprintf("%s: %s: Dockerfile or context not found locally", filepath.Base(file), member))
					continue
				}
				r.Watch = dockerfileWatch(r.Dockerfile, r.Contexts, r.Args)
				out = append(out, r)
			}
		}
	}
	return out, notes
}

// matrixRows expands a job matrix: its include rows, or the cross product of its list-valued keys.
func matrixRows(m map[string]any) []map[string]string {
	if len(m) == 0 {
		return []map[string]string{{}}
	}
	if inc, ok := m["include"].([]any); ok {
		var rows []map[string]string
		for _, it := range inc {
			if r, ok := it.(map[string]any); ok {
				row := map[string]string{}
				for k, v := range r {
					row[k] = fmt.Sprint(v)
				}
				rows = append(rows, row)
			}
		}
		return rows
	}
	rows := []map[string]string{{}}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		vals, ok := m[k].([]any)
		if !ok {
			continue
		}
		var next []map[string]string
		for _, r := range rows {
			for _, v := range vals {
				n := map[string]string{k: fmt.Sprint(v)}
				for a, b := range r {
					n[a] = b
				}
				next = append(next, n)
			}
		}
		rows = next
	}
	return rows
}

var ghExpr = regexp.MustCompile(`\$\{\{\s*(.*?)\s*\}\}`)

// evalGH evaluates the expressions build workflows use: matrix.<key>, a 'literal', a == b, and the
// "cond && 'x' || 'y'" choice. Anything else (github.*, secrets.*) becomes "".
func evalGH(s string, matrix map[string]string) string {
	return ghExpr.ReplaceAllStringFunc(s, func(m string) string {
		return evalGHExpr(ghExpr.FindStringSubmatch(m)[1], matrix)
	})
}

func evalGHExpr(e string, matrix map[string]string) string {
	e = strings.TrimSpace(e)
	if cond, rest, ok := strings.Cut(e, "&&"); ok {
		yes, no, _ := strings.Cut(rest, "||")
		if evalGHExpr(cond, matrix) == "true" {
			return evalGHExpr(yes, matrix)
		}
		return evalGHExpr(no, matrix)
	}
	if a, b, ok := strings.Cut(e, "=="); ok {
		if evalGHExpr(a, matrix) == evalGHExpr(b, matrix) {
			return "true"
		}
		return "false"
	}
	if len(e) >= 2 && e[0] == '\'' && e[len(e)-1] == '\'' {
		return e[1 : len(e)-1]
	}
	if strings.HasPrefix(e, "matrix.") {
		return matrix[strings.TrimPrefix(e, "matrix.")]
	}
	return ""
}

// dockerfileWatch lists what the Dockerfile copies from named contexts (COPY --from=<context> <src>): uncommitted
// changes there call for a rebuild, beside the member's own source path.
func dockerfileWatch(dockerfile string, contexts, args map[string]string) []string {
	f, err := os.Open(dockerfile)
	if err != nil {
		return nil
	}
	defer f.Close()
	seen := map[string]bool{filepath.Dir(dockerfile): true}
	out := []string{filepath.Dir(dockerfile)}
	s := bufio.NewScanner(f)
	for s.Scan() {
		fs := strings.Fields(s.Text())
		if len(fs) < 3 || !strings.EqualFold(fs[0], "COPY") || !strings.HasPrefix(fs[1], "--from=") {
			continue
		}
		dir, ok := contexts[strings.TrimPrefix(fs[1], "--from=")]
		if !ok {
			continue
		}
		src := fs[2]
		for k, v := range args { // a per-build path names its build arg (${TOKEN})
			src = strings.ReplaceAll(src, "${"+k+"}", v)
		}
		if strings.Contains(src, "${") { // still unknown: watch its folder
			src = src[:strings.LastIndex(src[:strings.Index(src, "${")], "/")+1]
		}
		p := filepath.Join(dir, src)
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// checkoutIndex finds a local Git checkout by its origin repository (owner/name), searching the workspace.
type checkoutIndex struct {
	root  string
	repos map[string]string
}

func (c *checkoutIndex) find(repo string) string {
	if c.repos == nil {
		c.repos = map[string]string{}
		_ = filepath.WalkDir(c.root, func(p string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			if strings.Count(strings.TrimPrefix(p, c.root), string(filepath.Separator)) > 4 || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
				if u, err := sh.Output("git", "-C", p, "remote", "get-url", "origin"); err == nil {
					c.repos[repoSlug(u)] = p
				}
			}
			return nil
		})
	}
	return c.repos[repoSlug(repo)]
}

// repoSlug is owner/name in lower case, from a URL or a bare owner/name.
func repoSlug(u string) string {
	u = strings.TrimSuffix(strings.TrimSpace(u), ".git")
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "git@")
	u = strings.Replace(u, ":", "/", 1)
	parts := strings.Split(u, "/")
	if len(parts) >= 2 {
		return strings.ToLower(parts[len(parts)-2] + "/" + parts[len(parts)-1])
	}
	return strings.ToLower(u)
}

// imageRepo is an image reference without tag or digest.
func imageRepo(img string) string {
	img = strings.TrimSpace(img)
	if i := strings.Index(img, "@"); i >= 0 {
		img = img[:i]
	}
	if i := strings.LastIndex(img, ":"); i > strings.LastIndex(img, "/") {
		img = img[:i]
	}
	return img
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// localRecipes are the overlay's [[local_build]] recipes, then the recipes derived from the build workflows for every
// member no overlay recipe covers. Derivation notes are reported once each.
func localRecipes(overlay []descriptor.LocalBuild, clusterDir, workspace string, l *official.Layout) []descriptor.LocalBuild {
	covered := map[string]bool{}
	for _, r := range overlay {
		for _, t := range r.Targets {
			covered[t] = true
		}
	}
	derived, notes := deriveLocalBuilds(clusterDir, workspace, l)
	out := append([]descriptor.LocalBuild{}, overlay...)
	for _, r := range derived {
		if !covered[r.Targets[0]] {
			out = append(out, r)
		}
	}
	seen := map[string]bool{}
	for _, n := range notes {
		if !seen[n] {
			seen[n] = true
			ui.Debug("--build: %s", n)
		}
	}
	return out
}
