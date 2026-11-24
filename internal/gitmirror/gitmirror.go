// Package gitmirror keeps treeless bare mirrors of source repositories in the
// user cache and answers "is this commit on the default branch?" with Git
// ancestry. It never reads a compare API, so the answer does not depend on how
// large the difference between two commits is (SPEC.md Appendix C.3).
package gitmirror

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"axon/internal/sh"
)

// Dir is the mirror cache: $AXON_CACHE_DIR, else <user cache>/axon/mirrors.
func Dir() string {
	if d := os.Getenv("AXON_CACHE_DIR"); d != "" {
		return filepath.Join(d, "mirrors")
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "axon", "mirrors")
	}
	return filepath.Join(os.TempDir(), "axon-mirrors")
}

// Mirror is one cached bare repository.
type Mirror struct {
	URL  string
	Path string
}

// pathFor maps a URL to a cache path (host/owner/name.git; file URLs by base name).
func pathFor(url string) string {
	u := strings.TrimSuffix(url, "/")
	for _, p := range []string{"https://", "http://", "ssh://", "file://"} {
		u = strings.TrimPrefix(u, p)
	}
	u = strings.TrimPrefix(u, "git@")
	u = strings.Replace(u, ":", "/", 1)
	u = strings.TrimSuffix(u, ".git")
	clean := []string{}
	for _, part := range strings.Split(u, "/") {
		if part != "" && part != "." && part != ".." {
			clean = append(clean, part)
		}
	}
	return filepath.Join(Dir(), filepath.Join(clean...)+".git")
}

func git(dir string, args ...string) (*sh.Result, error) {
	full := append([]string{"-C", dir}, args...)
	return sh.Run(sh.Cmd{Name: "git", Args: full, Timeout: 10 * time.Minute,
		Env: []string{"GIT_TERMINAL_PROMPT=0"}})
}

// brief turns a failed git command into its last stderr line.
func brief(r *sh.Result, err error) error {
	if r != nil {
		lines := strings.Split(strings.TrimSpace(r.Stderr), "\n")
		if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
			last = strings.TrimPrefix(last, "fatal: ")
			if strings.Contains(last, "not found") || strings.Contains(last, "could not read Username") || strings.Contains(last, "Authentication failed") {
				last += " (no access with your Git credentials?)"
			}
			return fmt.Errorf("%s", last)
		}
	}
	return err
}

// Sync clones (treeless, bare) or fetches the mirror for url.
func Sync(url string) (*Mirror, error) {
	m := &Mirror{URL: url, Path: pathFor(url)}
	if _, err := os.Stat(filepath.Join(m.Path, "HEAD")); err == nil {
		if r, err := git(m.Path, "fetch", "--quiet", "--prune", "--filter=tree:0", "origin", "+refs/heads/*:refs/heads/*"); err != nil {
			return nil, fmt.Errorf("fetch %s: %w", url, brief(r, err))
		}
		return m, nil
	}
	if err := os.MkdirAll(filepath.Dir(m.Path), 0o755); err != nil {
		return nil, err
	}
	tmp := m.Path + ".partial"
	_ = os.RemoveAll(tmp)
	if r, err := sh.Run(sh.Cmd{Name: "git", Args: []string{"clone", "--quiet", "--bare", "--filter=tree:0", url, tmp},
		Timeout: 20 * time.Minute, Env: []string{"GIT_TERMINAL_PROMPT=0"}}); err != nil {
		_ = os.RemoveAll(tmp)
		return nil, fmt.Errorf("clone %s: %w", url, brief(r, err))
	}
	if err := os.Rename(tmp, m.Path); err != nil {
		return nil, err
	}
	return m, nil
}

// DefaultBranch asks the remote which branch HEAD names (e.g. "main"). The
// release gate does not use it: it checks the branch named main (Appendix C.3).
func (m *Mirror) DefaultBranch() (string, error) {
	r, err := git(m.Path, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read default branch of %s: %w", m.URL, err)
	}
	for _, line := range strings.Split(r.Stdout, "\n") {
		if strings.HasPrefix(line, "ref: refs/heads/") {
			f := strings.Fields(strings.TrimPrefix(line, "ref: refs/heads/"))
			if len(f) > 0 {
				return f[0], nil
			}
		}
	}
	return "", fmt.Errorf("%s does not advertise a default branch", m.URL)
}

// Status of a commit relative to the default branch.
type Status string

const (
	OnMain    Status = "on-main"     // reachable from the default branch
	NotOnMain Status = "not-on-main" // exists, but the default branch does not contain it
	Missing   Status = "missing"     // the repository has no such commit on any branch
)

// Ancestry is the answer for one commit.
type Ancestry struct {
	Status   Status   `json:"status"`
	Branch   string   `json:"branch"`             // the default branch
	Behind   int      `json:"behind"`             // commits on the branch after this one (OnMain)
	Head     string   `json:"head"`               // the branch head
	Branches []string `json:"branches,omitempty"` // branches that contain it (NotOnMain)
}

// Check places commit relative to branch.
func (m *Mirror) Check(commit, branch string) (*Ancestry, error) {
	a := &Ancestry{Branch: branch}
	head, err := git(m.Path, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return nil, fmt.Errorf("%s has no branch %s", m.URL, branch)
	}
	a.Head = strings.TrimSpace(head.Stdout)
	if _, err := git(m.Path, "cat-file", "-e", commit+"^{commit}"); err != nil {
		a.Status = Missing
		return a, nil
	}
	if _, err := git(m.Path, "merge-base", "--is-ancestor", commit, "refs/heads/"+branch); err != nil {
		if e, ok := err.(*sh.Error); ok && e.Code == 1 {
			a.Status = NotOnMain
			r, _ := git(m.Path, "for-each-ref", "--contains", commit, "--format=%(refname:short)", "refs/heads/")
			if r != nil {
				for _, b := range strings.Fields(r.Stdout) {
					a.Branches = append(a.Branches, b)
				}
			}
			return a, nil
		}
		return nil, fmt.Errorf("compare %s with %s in %s: %w", commit, branch, m.URL, err)
	}
	a.Status = OnMain
	if r, err := git(m.Path, "rev-list", "--count", commit+"..refs/heads/"+branch); err == nil {
		a.Behind, _ = strconv.Atoi(strings.TrimSpace(r.Stdout))
	}
	return a, nil
}

// SyncAll syncs every URL with at most n at a time. Errors are returned per URL.
func SyncAll(urls []string, n int) (map[string]*Mirror, map[string]error) {
	if n < 1 {
		n = 1
	}
	ms, errs := map[string]*Mirror{}, map[string]error{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, n)
	for _, u := range urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			m, err := Sync(u)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[u] = err
			} else {
				ms[u] = m
			}
		}(u)
	}
	wg.Wait()
	return ms, errs
}
