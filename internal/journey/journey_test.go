package journey

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

func j(s string) any {
	var v any
	json.Unmarshal([]byte(s), &v)
	return v
}

func TestMatchSubset(t *testing.T) {
	got := j(`{"modules":[{"token":"a","health":"NOT_SERVING"},{"token":"b","health":"SERVING"}],"n":3,"ok":true}`)
	cases := []struct {
		want string
		ok   bool
	}{
		{`{"modules":[{"health":"SERVING"}]}`, true}, // some element matches
		{`{"modules":[{"health":"GONE"}]}`, false},
		{`{"n":3}`, true},
		{`{"n":4}`, false},
		{`{"ok":true}`, true},
		{`{"missing":1}`, false},
		{`{"modules":[{"token":"a"},{"token":"b"}]}`, true},
		{`{"modules":{"token":"a"}}`, false},
	}
	for _, c := range cases {
		if _, ok := Match(j(c.want), got, "$"); ok != c.ok {
			t.Errorf("Match(%s) = %v, want %v", c.want, ok, c.ok)
		}
	}
	// TOML integers arrive as int64
	if _, ok := Match(map[string]any{"n": int64(3)}, got, "$"); !ok {
		t.Error("int64 pattern should match a JSON number")
	}
}

func TestRunnerAgainstServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "web.lvh.me" {
			http.Error(w, "wrong host", 421)
			return
		}
		switch r.URL.Path {
		case "/healthz":
			w.Write([]byte("ok"))
		case "/api/system":
			w.Write([]byte(`{"modules":[{"health":"SERVING"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	r := &Runner{Port: port, Domain: "lvh.me"}
	jr := &Journey{Steps: []Step{
		{Name: "alive", Path: "/healthz", Expect: &Expect{Contains: "ok"}},
		{Name: "serving", Path: "/api/system", Expect: &Expect{JSON: map[string]any{"modules": []any{map[string]any{"health": "SERVING"}}}}},
		{Name: "404", Path: "/nope", Expect: &Expect{Status: 404}},
	}}
	jr.Meta.App = "web"
	res := r.Run(jr)
	if !res.Pass {
		t.Fatalf("journey failed: %+v", res.Steps)
	}
	jr.Steps = []Step{{Name: "default 2xx", Path: "/nope"}}
	if r.Run(jr).Pass {
		t.Fatal("a 404 must fail the default 2xx expectation")
	}
	if p := r.ProbeSystem("web", "web"); !p.Pass || p.Status != 200 {
		t.Fatalf("probe %+v", p)
	}
}

func TestLint(t *testing.T) {
	js := []*Journey{{Path: "a.toml"}, {Path: "b.toml", Steps: []Step{{Path: "nope", Method: "FETCH"}}}}
	js[1].Meta.App = "ghost"
	probs := Lint(js, map[string]bool{"web": true})
	if len(probs) != 5 {
		t.Fatalf("got %d problems: %v", len(probs), probs)
	}
}
