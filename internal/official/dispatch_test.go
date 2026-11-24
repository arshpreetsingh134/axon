package official

import "testing"

func TestFindDispatch(t *testing.T) {
	root := fixture(t)
	write(t, root, ".github/workflows/deploy.yml", `name: Deploy
on:
  workflow_dispatch:
    inputs:
      version:
        required: true
        type: string
      environment:
        required: true
        type: choice
        options: ["qa", "uat"]
      check:
        type: boolean
`)
	write(t, root, ".github/workflows/release.yml", `name: Release
on:
  workflow_dispatch:
    inputs:
      version: {required: true}
      series: {required: true}
`)
	l, _, _ := Detect(root)
	w, ok, err := l.FindDispatch("environment", "version")
	if err != nil || !ok || w.File != "deploy.yml" || len(w.EnvOptions) != 2 || !w.Has("check") {
		t.Fatalf("deploy: %+v ok=%v err=%v", w, ok, err)
	}
	if w, ok, _ := l.FindDispatch("version", "series"); !ok || w.File != "release.yml" {
		t.Fatalf("release: %+v ok=%v", w, ok)
	}
	if _, ok, _ := l.FindDispatch("version"); ok {
		t.Fatal("an ambiguous match (two workflows take version) was accepted")
	}
	if o, n, ok := l.RepoSlug(); !ok || o != "example" || n != "cluster" {
		t.Fatalf("slug %s/%s %v", o, n, ok)
	}
}
