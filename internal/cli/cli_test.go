package cli

import (
	"bytes"
	"strings"
	"testing"

	"axon/internal/ui"
)

func testRegistry(got *Ctx) *Registry {
	r := New("axon")
	r.Globals = []Flag{{Name: "help", Short: "h", Kind: Bool}, {Name: "yes", Short: "y", Kind: Bool}, {Name: "version", Kind: Bool}}
	run := func(c *Ctx) error { *got = *c; return nil }
	r.Add(
		&Verb{Name: "version", Run: run},
		&Verb{Name: "cluster new", Args: []Arg{{Name: "dir"}}, Flags: []Flag{{Name: "name", Kind: String}}, Run: run},
		&Verb{Name: "cluster set", Args: []Arg{{Name: "kv", Variadic: true}}, Run: run},
		&Verb{Name: "release list", Run: run},
		&Verb{Name: "env deploy", Args: []Arg{{Name: "env"}, {Name: "release", Optional: true}},
			Flags: []Flag{{Name: "build", Kind: Bool}, {Name: "jobs", Kind: Int}, {Name: "app", Kind: Multi}}, Run: run},
	)
	r.AddMoved(&Moved{Old: "platform status", New: "release list"})
	return r
}

func capture(t *testing.T) (*bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errb bytes.Buffer
	oldOut, oldErr := ui.Stdout, ui.Stderr
	ui.Stdout, ui.Stderr = &out, &errb
	t.Cleanup(func() { ui.Stdout, ui.Stderr = oldOut, oldErr })
	return &out, &errb
}

func TestSuggest(t *testing.T) {
	var c Ctx
	r := testRegistry(&c)
	cases := map[string]string{"cluster": "cluster new", "release": "release list", "bogus": "", "platform": "platform status"}
	for in, want := range cases {
		if got := r.Suggest(in); got != want {
			t.Errorf("Suggest(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnknownVerbEchoesAllArgs(t *testing.T) {
	var c Ctx
	r := testRegistry(&c)
	_, errb := capture(t)
	code := r.Run([]string{"release", "create", "--help"})
	if code != ExitUsage {
		t.Fatalf("exit %d, want 2", code)
	}
	want := `axon: unknown verb "release create --help" (did you mean "release list"?)`
	if strings.TrimSpace(errb.String()) != want {
		t.Fatalf("got %q", errb.String())
	}
}

func TestFlagsAnywhere(t *testing.T) {
	var c Ctx
	r := testRegistry(&c)
	capture(t)
	if code := r.Run([]string{"env", "deploy", "--build", "dev", "--jobs=4", "--app", "a", "0.1.2", "--app=b", "-y"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if c.Arg(0) != "dev" || c.Arg(1) != "0.1.2" || !c.Bool("build") || c.Int("jobs") != 4 || !c.G.Yes {
		t.Fatalf("parsed %+v / %+v", c.Args, c.values)
	}
	if got := strings.Join(c.Strings("app"), ","); got != "a,b" {
		t.Fatalf("repeatable flag = %q", got)
	}
}

func TestUsageErrors(t *testing.T) {
	var c Ctx
	r := testRegistry(&c)
	for _, tc := range []struct {
		args []string
		msg  string
	}{
		{[]string{"cluster", "new"}, "cluster new: missing required argument(s): dir"},
		{[]string{"cluster", "new", "x", "--bogus"}, "cluster new: unrecognized flag --bogus"},
		{[]string{"env", "deploy", "dev", "--jobs", "many"}, `flag --jobs wants a number`},
		{[]string{"cluster", "new", "a", "b"}, `unexpected argument "b"`},
	} {
		_, errb := capture(t)
		if code := r.Run(tc.args); code != ExitUsage {
			t.Errorf("%v: exit %d, want 2", tc.args, code)
		}
		if !strings.Contains(errb.String(), tc.msg) {
			t.Errorf("%v: stderr %q lacks %q", tc.args, errb.String(), tc.msg)
		}
	}
}

func TestMovedVerbForwardsWithNotice(t *testing.T) {
	var c Ctx
	r := testRegistry(&c)
	_, errb := capture(t)
	if code := r.Run([]string{"platform", "status"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if c.Verb.Name != "release list" || !strings.Contains(errb.String(), `"platform status" is now "release list"`) {
		t.Fatalf("verb %q, stderr %q", c.Verb.Name, errb.String())
	}
}

func TestVariadicAndHelp(t *testing.T) {
	var c Ctx
	r := testRegistry(&c)
	out, _ := capture(t)
	if code := r.Run([]string{"cluster", "set", "a=1", "b=2", "c"}); code != 0 || len(c.Args) != 3 {
		t.Fatalf("variadic: exit %d args %v", code, c.Args)
	}
	if code := r.Run([]string{"cluster", "new", "--help"}); code != 0 || !strings.Contains(out.String(), "usage: axon cluster new <dir> [flags]") {
		t.Fatalf("help: %q", out.String())
	}
}

func TestCompletionScripts(t *testing.T) {
	var c Ctx
	r := testRegistry(&c)
	for _, sh := range []string{"bash", "zsh", "fish"} {
		s, err := r.Completion(sh)
		if err != nil || !strings.Contains(s, "cluster") || !strings.Contains(s, "build") {
			t.Errorf("%s: err %v", sh, err)
		}
	}
	if _, err := r.Completion("tcsh"); err == nil {
		t.Error("tcsh should be rejected")
	}
}

func TestWrapKeepsWords(t *testing.T) {
	s := Wrap(strings.Repeat("word ", 40), 30, "  ")
	for _, l := range strings.Split(s, "\n") {
		if len(l) > 32 {
			t.Fatalf("line too long: %q", l)
		}
	}
}
