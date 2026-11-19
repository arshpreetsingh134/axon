package deploy

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"axon/internal/build"
	"axon/internal/descriptor"
	"axon/internal/layout"
)

// fixture: shop.web composes web + api (source) and pay (prebuilt image);
// jobs is a worker built from its Dockerfile; feed runs a prebuilt image;
// other.web also composes a module "api", so a bare "api" is ambiguous.
func moduleFixture(t *testing.T) *descriptor.Model {
	t.Helper()
	root := t.TempDir()
	w := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w(filepath.Join(root, "shop.axon", ".axon.infra", "axonfile"), `build_type = "cluster"
name = "shop"
core_image = "ghcr.io/example/core:1"
[fabric.postgres]
port = 5545
[fabric.traefik]
http = 8090
[[app]]
name = "shop.web"
host = "web"
db = "web"
[[app]]
name = "other.web"
host = "other"
db = "other"
[[app]]
name = "jobs"
kind = "project"
[[app]]
name = "feed"
kind = "project"
image = "reg.example/feed:2"
`)
	app := func(dir, name string, mods ...string) {
		body := "build_type = \"app\"\n[app]\nname = \"" + name + "\"\n"
		for _, m := range mods {
			if strings.HasPrefix(m, "img:") {
				tok := strings.TrimPrefix(m, "img:")
				body += "[[compose]]\nsource = \"modules/" + tok + "/module\"\nimage = \"ghcr.io/example/" + tok + ":1\"\ntoken = \"" + tok + "\"\n"
				continue
			}
			body += "[[compose]]\nsource = \"modules/" + m + "/module\"\n"
			md := filepath.Join(root, dir, "modules", m, "module")
			w(filepath.Join(md, "axonfile"), "build_type = \"module\"\n[module]\ntoken = \""+m+"\"\nlanguage = \"go\"\n")
			w(filepath.Join(md, "service", "go.mod"), "module example.com/"+m+"\n\ngo 1.22\n")
			w(filepath.Join(md, "service", "main.go"), "package main\n\nfunc main() {}\n")
		}
		w(filepath.Join(root, dir, "axonfile"), body)
	}
	app("shop.web", "shop.web", "web", "api", "img:pay")
	app("other.web", "other.web", "api")
	w(filepath.Join(root, "jobs", "axonfile"), "build_type = \"project\"\n[app]\nname = \"jobs\"\n")
	w(filepath.Join(root, "jobs", "Dockerfile"), "FROM alpine:3\n")
	f, err := layout.FindCluster(filepath.Join(root, "shop.axon"), layout.ModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	m, err := descriptor.Load(f)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func targetNames(ts []Target) string {
	var ns []string
	for _, t := range ts {
		ns = append(ns, t.String())
	}
	return strings.Join(ns, ",")
}

func TestResolveModules(t *testing.T) {
	m := moduleFixture(t)
	ok := map[string]string{
		"web":                    "shop.web/web",
		"shop.web/api":           "shop.web/api",
		"jobs":                   "jobs",
		"web,shop.web/web":       "shop.web/web", // de-duplicated
		"other.web/api,web,jobs": "other.web/api,shop.web/web,jobs",
	}
	for in, want := range ok {
		ts, err := ResolveModules(m, strings.Split(in, ","))
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got := targetNames(ts); got != want {
			t.Errorf("%s → %s, want %s", in, got, want)
		}
	}
	bad := map[string]string{
		"api":           "several apps compose a module \"api\"",
		"shop.web":      "is an app with modules",
		"feed":          "runs the prebuilt image reg.example/feed:2",
		"shop.web/pay":  "runs the prebuilt image ghcr.io/example/pay:1",
		"shop.web/nope": "has no module \"nope\"",
		"nope/web":      "no app \"nope\"",
		"jobs/x":        "is a worker app",
		"nope":          "no module or worker app of that name",
		"":              "--module needs",
	}
	for in, want := range bad {
		_, err := ResolveModules(m, []string{in})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want error containing %q, got %v", in, want, err)
		}
	}
}

func TestWorkloadsTouchOnlyTargetsAndTheirCore(t *testing.T) {
	m := moduleFixture(t)
	ts, err := ResolveModules(m, []string{"web", "shop.web/api", "jobs"})
	if err != nil {
		t.Fatal(err)
	}
	deploys, services := workloads(ts)
	sort.Strings(deploys)
	want := []string{descriptor.CoreName("jobs"), "api", descriptor.CoreName("shop.web"), "web"}
	sort.Strings(want)
	if strings.Join(deploys, ",") != strings.Join(want, ",") {
		t.Fatalf("deployments %v, want %v", deploys, want)
	}
	if got := strings.Join(services["shop.web"], ","); got != "web,api,core" {
		t.Errorf("shop.web services = %s", got)
	}
	if got := strings.Join(services["jobs"], ","); got != descriptor.Slug("jobs") {
		t.Errorf("jobs services = %s", got)
	}
	if _, touched := services["other.web"]; touched {
		t.Error("other.web must not be touched")
	}
}

func TestBuildOnlyBuildsTheTargets(t *testing.T) {
	m := moduleFixture(t)
	repos := func(o build.Options) []string {
		t.Helper()
		o.DryRun = true
		res, err := build.Run(m, o)
		if err != nil {
			t.Fatal(err)
		}
		var rs []string
		for _, img := range res.Images {
			rs = append(rs, img.Repo)
		}
		sort.Strings(rs)
		return rs
	}
	web := descriptor.ServiceImage("shop", "shop.web", "web")
	core := descriptor.ServiceImage("shop", "shop.web", "core")
	got := repos(build.Options{Only: []string{"shop.web/web"}})
	if strings.Join(got, ",") != strings.Join([]string{core, web}, ",") {
		t.Fatalf("Only shop.web/web built %v", got)
	}
	got = repos(build.Options{Only: []string{"jobs"}})
	if len(got) != 1 || got[0] != descriptor.WorkerImage("shop", "jobs") {
		t.Fatalf("Only jobs built %v", got)
	}
	if all := repos(build.Options{}); len(all) < 6 {
		t.Fatalf("without Only every image is planned, got %v", all)
	}
}
