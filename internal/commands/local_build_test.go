package commands

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"axon/internal/descriptor"
	"axon/internal/official"
)

// fakeGit answers from tables: heads by checkout, dirty paths ("dir|path"), and ancestry ("ancestor>commit").
type fakeGit struct {
	heads    map[string]string
	dirtyAt  map[string]bool
	ancestry map[string]bool
	unknown  map[string]bool
	sameAt   map[string]bool // "a=b|path": the same content
}

func (f fakeGit) head(dir string) (string, error) {
	if h, ok := f.heads[dir]; ok {
		return h, nil
	}
	return "", errors.New("not a git checkout")
}
func (f fakeGit) dirty(dir, path string) (bool, error) { return f.dirtyAt[dir+"|"+path], nil }
func (f fakeGit) same(dir, a, b, path string) (bool, bool) {
	if f.unknown[a] || f.unknown[b] {
		return false, false
	}
	return true, f.sameAt[a+"="+b+"|"+path]
}
func (f fakeGit) contains(dir, ancestor, commit, _ string) (bool, bool) {
	if f.unknown[ancestor] {
		return false, false
	}
	return true, f.ancestry[ancestor+">"+commit]
}

const (
	pinA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	headA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func fixture() ([]descriptor.LocalBuild, []official.Materialization, []official.LockMember, map[string][]string) {
	recipes := []descriptor.LocalBuild{
		{Targets: []string{"p.app.quote", "p.app.rating"}, Dockerfile: "github/business-build/Dockerfile", Context: "github/business-build",
			Contexts:   map[string]string{"source": "{checkout}", "descriptor": "."},
			Args:       map[string]string{"TOKEN": "{token}", "SOURCE_REVISION": "{commit}"},
			TargetArgs: map[string]map[string]string{"p.app.rating": {"BUILD_LDFLAGS": "-X main.x=1"}}},
		{Targets: []string{"p.wfc.worker"}, Dockerfile: "github/worker-build/Dockerfile", Context: "github/worker-build",
			Contexts: map[string]string{"policy": "{source}/workflows"}, Watch: []string{"{workspace}/other.wfc"}},
	}
	mats := []official.Materialization{
		{Target: "p.app.quote", SourceMember: "p.app", LocalCheckoutPath: "repos/p.app", SourcePath: "modules/quote"},
		{Target: "p.app.rating", SourceMember: "p.app", LocalCheckoutPath: "repos/p.app", SourcePath: "modules/rating"},
		{Target: "p.wfc.worker", SourceMember: "p.wfc", LocalCheckoutPath: "repos/p.wfc", SourcePath: "."},
	}
	lock := []official.LockMember{{AppID: "p.app", Commit: pinA}, {AppID: "p.wfc", Commit: pinA}}
	tokens := map[string][]string{"p.app.quote": {"quote"}, "p.app.rating": {"rating"}}
	return recipes, mats, lock, tokens
}

func targets(bs []localBuild) string {
	var out []string
	for _, b := range bs {
		out = append(out, b.Target)
	}
	return strings.Join(out, ",")
}

func TestLocalBuildsOnlyMembersThatMovedPastTheirPin(t *testing.T) {
	r, m, l, tk := fixture()
	g := fakeGit{heads: map[string]string{"/w/repos/p.app": headA, "/w/repos/p.wfc": pinA},
		ancestry: map[string]bool{pinA + ">" + headA: true}}
	bs, notes, err := planLocalBuilds(r, m, l, tk, "/w", "/w/cluster", nil, false, g)
	if err != nil {
		t.Fatal(err)
	}
	if targets(bs) != "p.app.quote,p.app.rating" || len(notes) != 0 {
		t.Fatalf("builds %s notes %v", targets(bs), notes)
	}
	q := bs[0]
	if q.Dockerfile != "/w/cluster/github/business-build/Dockerfile" || q.Context != "/w/cluster/github/business-build" ||
		q.Contexts["source"] != "/w/repos/p.app" || q.Contexts["descriptor"] != "/w/cluster" ||
		q.Args["TOKEN"] != "quote" || q.Args["SOURCE_REVISION"] != headA || !strings.Contains(q.Why, "ahead of the pin") {
		t.Fatalf("quote build %+v", q)
	}
	if bs[1].Args["BUILD_LDFLAGS"] != "-X main.x=1" || bs[0].Args["BUILD_LDFLAGS"] != "" {
		t.Fatalf("target args leaked: %+v / %+v", bs[0].Args, bs[1].Args)
	}
}

func TestLocalBuildsUncommittedChangesAndWatchedDirectories(t *testing.T) {
	r, m, l, tk := fixture()
	g := fakeGit{heads: map[string]string{"/w/repos/p.app": pinA, "/w/repos/p.wfc": pinA},
		dirtyAt: map[string]bool{"/w/repos/p.app|modules/rating": true, "/w/other.wfc|.": true}}
	bs, _, err := planLocalBuilds(r, m, l, tk, "/w", "/w/cluster", nil, false, g)
	if err != nil {
		t.Fatal(err)
	}
	if targets(bs) != "p.app.rating,p.wfc.worker" {
		t.Fatalf("builds %s", targets(bs))
	}
	if bs[1].Contexts["policy"] != "/w/repos/p.wfc/workflows" || !strings.Contains(bs[1].Why, "/w/other.wfc") {
		t.Fatalf("worker build %+v", bs[1])
	}
}

func TestLocalBuildsNeverDeploysACheckoutBehindItsPin(t *testing.T) {
	r, m, l, tk := fixture()
	g := fakeGit{heads: map[string]string{"/w/repos/p.app": headA, "/w/repos/p.wfc": headA}, unknown: map[string]bool{}}
	g.unknown = map[string]bool{}
	bs, notes, err := planLocalBuilds(r, m, l, tk, "/w", "/w/cluster", nil, false, g)
	if err != nil || len(bs) != 0 || len(notes) != 3 || !strings.Contains(notes[0], "behind or beside it") {
		t.Fatalf("builds %s notes %v err %v", targets(bs), notes, err)
	}
	// --app builds it anyway, and only it.
	bs, _, err = planLocalBuilds(r, m, l, tk, "/w", "/w/cluster", []string{"p.app.quote"}, false, g)
	if err != nil || targets(bs) != "p.app.quote" || bs[0].Why != "--app" {
		t.Fatalf("--app: %s %v", targets(bs), err)
	}
	// A pin missing from the checkout is reported, not guessed.
	g.unknown[pinA] = true
	_, notes, _ = planLocalBuilds(r, m, l, tk, "/w", "/w/cluster", nil, false, g)
	if len(notes) == 0 || !strings.Contains(notes[0], "not in the checkout") {
		t.Fatalf("notes %v", notes)
	}
}

func TestLocalBuildsRebuildAndErrors(t *testing.T) {
	r, m, l, tk := fixture()
	g := fakeGit{heads: map[string]string{"/w/repos/p.app": pinA}}
	bs, notes, err := planLocalBuilds(r, m, l, tk, "/w", "/w/cluster", nil, true, g)
	if err != nil || targets(bs) != "p.app.quote,p.app.rating" || len(notes) != 1 || !strings.Contains(notes[0], "no Git checkout") {
		t.Fatalf("--rebuild: %s %v %v", targets(bs), notes, err)
	}
	if _, _, err = planLocalBuilds(r, m, l, tk, "/w", "/w/cluster", []string{"nope"}, false, g); err == nil || !strings.Contains(err.Error(), "no [[local_build]] recipe") {
		t.Fatalf("unknown --app: %v", err)
	}
	bad := append(r, descriptor.LocalBuild{Targets: []string{"p.app.quote"}})
	if _, _, err = planLocalBuilds(bad, m, l, tk, "/w", "/w/cluster", nil, false, g); err == nil || !strings.Contains(err.Error(), "two recipes") {
		t.Fatalf("duplicate: %v", err)
	}
	orphan := []descriptor.LocalBuild{{Targets: []string{"x.not.materialized"}}}
	if _, _, err = planLocalBuilds(orphan, m, l, tk, "/w", "/w/cluster", nil, false, g); err == nil || !strings.Contains(err.Error(), "not a [[materialization]]") {
		t.Fatalf("orphan: %v", err)
	}
}

// A checkout on another branch whose source path holds the pin's content (develop's merge of the change main merged)
// is at its pin: nothing to build, nothing reported. Only a member whose own source path changed is built.
func TestLocalBuildsComparesContentNotHistory(t *testing.T) {
	r, m, l, tk := fixture()
	g := fakeGit{heads: map[string]string{"/w/repos/p.app": headA, "/w/repos/p.wfc": headA},
		sameAt:   map[string]bool{pinA + "=" + headA + "|modules/quote": true, pinA + "=" + headA + "|.": true},
		ancestry: map[string]bool{pinA + ">" + headA: true}}
	bs, notes, err := planLocalBuilds(r, m, l, tk, "/w", "/w/cluster", nil, false, g)
	if err != nil || targets(bs) != "p.app.rating" || len(notes) != 0 || !strings.Contains(bs[0].Why, "modules/rating changed") {
		t.Fatalf("builds %s notes %v err %v", targets(bs), notes, err)
	}
	// Same content but uncommitted changes still build.
	g.dirtyAt = map[string]bool{"/w/repos/p.app|modules/quote": true}
	bs, _, _ = planLocalBuilds(r, m, l, tk, "/w", "/w/cluster", nil, false, g)
	if targets(bs) != "p.app.quote,p.app.rating" {
		t.Fatalf("dirty: %s", targets(bs))
	}
}

func TestLocalImagesByMemberNamesMembersFromTheTag(t *testing.T) {
	got := localImagesByMember(map[string]string{
		"app-shop-wfc-worker": "local/app-shop-wfc-worker:abc123",
		"app-shop-billing":    "local/app-shop-billing:def456",
		"app-unknown":         "local/app-unknown:1",
	}, []string{"shop.wfc.worker", "shop.billing"})
	if len(got) != 2 || got["shop.wfc.worker"] != "local/app-shop-wfc-worker:abc123" || got["shop.billing"] != "local/app-shop-billing:def456" {
		t.Fatalf("got %v", got)
	}
}

func TestRealGitContainsMainsMergeOfDevelop(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	write("a.txt", "1")
	run("add", ".")
	run("commit", "-qm", "base")
	run("checkout", "-qb", "develop")
	write("a.txt", "2")
	run("commit", "-qam", "feature")
	run("checkout", "-q", "main")
	run("merge", "-q", "--no-ff", "-m", "merge develop", "develop")
	pin := run("rev-parse", "HEAD")
	run("checkout", "-q", "develop")
	write("a.txt", "3")
	run("commit", "-qam", "more work")
	head := run("rev-parse", "HEAD")
	if known, ok := (realGit{}).contains(dir, pin, head, "."); !known || !ok {
		t.Fatalf("develop beside main's merge of it: known %v ok %v", known, ok)
	}
	run("checkout", "-q", "main")
	write("b.txt", "hotfix")
	run("add", ".")
	run("commit", "-qm", "hotfix on main")
	hot := run("rev-parse", "HEAD")
	if _, ok := (realGit{}).contains(dir, hot, head, "."); ok {
		t.Fatal("a hotfix only on main must not count as contained")
	}
}
