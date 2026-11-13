package dotenv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParse(t *testing.T) {
	m, err := Parse("# c\nA=1\nexport B = two # note\nC=\"multi\nline\"\nD='x y'\nE=\"q\\\"uote\"\n")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "1", "B": "two", "C": "multi\nline", "D": "x y", "E": `q"uote`}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	if _, err := Parse("NOEQUALS\n"); err == nil {
		t.Error("expected a parse error")
	}
}

func TestSetPreservesAndRoundTrips(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(p, []byte("# keep me\nA=1\n"), 0o600)
	pem := "-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----\n"
	if err := Set(p, "A", "2"); err != nil {
		t.Fatal(err)
	}
	Set(p, "KEY", pem)
	m, err := Read(p)
	if err != nil || m["A"] != "2" || m["KEY"] != pem {
		t.Fatalf("read back %v (%v)", m, err)
	}
	b, _ := os.ReadFile(p)
	if string(b[:9]) != "# keep me" {
		t.Fatalf("comment lost: %q", b)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
}

func TestExpandAndRefs(t *testing.T) {
	out, missing := Expand("http://${HOST}:${PORT}/x", map[string]string{"HOST": "h"})
	if out != "http://h:${PORT}/x" || len(missing) != 1 || missing[0] != "PORT" {
		t.Fatalf("%q %v", out, missing)
	}
	if v, ok := IsPureRef("${A_1}"); !ok || v != "A_1" {
		t.Fatal("pure ref")
	}
	if _, ok := IsPureRef("x${A}"); ok {
		t.Fatal("not a pure ref")
	}
}

func TestRemoveKeepsOtherLinesAndMultiline(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(p, []byte("# head\nA=1\nKEY=\"-----BEGIN\nabc\n-----END\"\nB=2\nexport C=3\n"), 0o600)
	if err := Remove(p, "KEY", "C"); err != nil {
		t.Fatal(err)
	}
	m, _ := Read(p)
	if _, ok := m["KEY"]; ok || m["A"] != "1" || m["B"] != "2" || len(m) != 2 {
		t.Fatalf("after remove: %v", m)
	}
	b, _ := os.ReadFile(p)
	if string(b[:6]) != "# head" {
		t.Fatalf("comment lost: %q", b)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode changed: %o", st.Mode().Perm())
	}
}
