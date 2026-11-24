package gitmirror

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, msg string) string {
	run(t, dir, "commit", "--allow-empty", "-q", "-m", msg)
	return run(t, dir, "rev-parse", "HEAD")
}

// upstream: main has m1 ← m2 ← m3; develop branches from m1 with d1; the
// default branch (HEAD) is develop, as in repositories whose gate branch is not
// their default.
func upstream(t *testing.T) (url string, m1, m3, d1 string, dir string) {
	t.Helper()
	dir = t.TempDir()
	run(t, dir, "init", "-q")
	m1 = commit(t, dir, "m1")
	run(t, dir, "checkout", "-q", "-b", "develop")
	d1 = commit(t, dir, "d1")
	run(t, dir, "checkout", "-q", "main")
	commit(t, dir, "m2")
	m3 = commit(t, dir, "m3")
	run(t, dir, "symbolic-ref", "HEAD", "refs/heads/develop")
	return "file://" + dir, m1, m3, d1, dir
}

func TestCheck(t *testing.T) {
	t.Setenv("AXON_CACHE_DIR", t.TempDir())
	url, m1, m3, d1, dir := upstream(t)
	m, err := Sync(url)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := m.DefaultBranch(); err != nil || b != "develop" {
		t.Fatalf("default branch %q %v", b, err)
	}

	a, err := m.Check(m3, "main")
	if err != nil || a.Status != OnMain || a.Behind != 0 || a.Head != m3 {
		t.Fatalf("head: %+v %v", a, err)
	}
	a, err = m.Check(m1, "main")
	if err != nil || a.Status != OnMain || a.Behind != 2 {
		t.Fatalf("behind: %+v %v", a, err)
	}
	a, err = m.Check(d1, "main")
	if err != nil || a.Status != NotOnMain || len(a.Branches) != 1 || a.Branches[0] != "develop" {
		t.Fatalf("develop-only: %+v %v", a, err)
	}
	a, err = m.Check(strings.Repeat("9", 40), "main")
	if err != nil || a.Status != Missing {
		t.Fatalf("missing: %+v %v", a, err)
	}
	if _, err := m.Check(m1, "nope"); err == nil {
		t.Fatal("unknown branch accepted")
	}

	// a new upstream commit is seen after the next sync
	run(t, dir, "checkout", "-q", "main")
	m4 := commit(t, dir, "m4")
	if _, err := Sync(url); err != nil {
		t.Fatal(err)
	}
	a, err = m.Check(m3, "main")
	if err != nil || a.Behind != 1 || a.Head != m4 {
		t.Fatalf("after fetch: %+v %v", a, err)
	}
}

func TestSyncAllReportsErrorsPerURL(t *testing.T) {
	t.Setenv("AXON_CACHE_DIR", t.TempDir())
	url, _, _, _, _ := upstream(t)
	bad := "file://" + filepath.Join(t.TempDir(), "absent")
	ms, errs := SyncAll([]string{url, bad}, 2)
	if ms[url] == nil || errs[url] != nil {
		t.Fatalf("good url: %v", errs[url])
	}
	if errs[bad] == nil || ms[bad] != nil {
		t.Fatal("missing repository did not fail")
	}
}

func TestPathForStaysInCache(t *testing.T) {
	t.Setenv("AXON_CACHE_DIR", "/cache")
	for in, want := range map[string]string{
		"https://github.com/example/a.git": "/cache/mirrors/github.com/example/a.git",
		"git@github.com:example/a.git":     "/cache/mirrors/github.com/example/a.git",
		"https://github.com/../../etc/x":   "/cache/mirrors/github.com/etc/x.git",
	} {
		if got := pathFor(in); got != want {
			t.Errorf("%s → %s, want %s", in, got, want)
		}
	}
}
