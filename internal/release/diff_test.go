package release

import (
	"os"
	"path/filepath"
	"testing"
)

func load(t *testing.T, body string) *Record {
	t.Helper()
	p := filepath.Join(t.TempDir(), "r.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDiff(t *testing.T) {
	a := load(t, `schema_version = 3
version = "0.1.1"
commit = "aaaa"
digest = "sha256:1"
[modules]
  "x.app" = "reg/x@sha256:11"
  "y.app" = "reg/y@sha256:22"
[sources]
  "x.app" = "c1"
`)
	b := load(t, `schema_version = 3
version = "0.1.2"
commit = "bbbb"
digest = "sha256:2"
[modules]
  "x.app" = "reg/x@sha256:33"
  "z.app" = "reg/z@sha256:44"
[sources]
  "x.app" = "c1"
[products]
  clc = "reg/clc@sha256:55"
`)
	got := Diff(a, b)
	want := []Change{
		{Table: "record", Key: "commit", Old: "aaaa", New: "bbbb"},
		{Table: "modules", Key: "x.app", Old: "reg/x@sha256:11", New: "reg/x@sha256:33"},
		{Table: "modules", Key: "y.app", Old: "reg/y@sha256:22"},
		{Table: "modules", Key: "z.app", New: "reg/z@sha256:44"},
		{Table: "products", Key: "clc", New: "reg/clc@sha256:55"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: got %+v want %+v", i, got[i], want[i])
		}
	}
	if len(Diff(a, a)) != 0 {
		t.Error("a record differs from itself")
	}
}
