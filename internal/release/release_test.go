package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func doc() map[string]any {
	return map[string]any{
		"schema_version": int64(1),
		"version":        "0.1.0",
		"created":        "2026-01-01T00:00:00Z",
		"tool_version":   "axon/0.1.0",
		"platforms":      []any{"linux/arm64"},
		"images":         map[string]any{"demo_demoweb-core": "demo_demoweb-core:abc@sha256:1", "x.y": "x:1"},
		"sources":        map[string]any{"demo.web": "deadbeef"},
	}
}

// The canonical encoding is part of every recorded digest: changing it breaks
// `release check` for existing records. This golden value pins it.
func TestCanonicalGolden(t *testing.T) {
	want := `created = "2026-01-01T00:00:00Z"
platforms = ["linux/arm64"]
schema_version = 1
tool_version = "axon/0.1.0"
version = "0.1.0"

[images]
  demo_demoweb-core = "demo_demoweb-core:abc@sha256:1"
  "x.y" = "x:1"

[sources]
  "demo.web" = "deadbeef"
`
	if got := Canonical(doc()); got != want {
		t.Fatalf("canonical encoding changed:\n%s", got)
	}
	if d := Digest(doc()); d != "sha256:"+strings.Repeat("", 0)+d[7:] || len(d) != 71 {
		t.Fatalf("digest %q", d)
	}
}

func TestRenderRoundTripVerifies(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "0.1.0.toml")
	if err := os.WriteFile(p, []byte(Render(doc())), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	want, got, _ := Verify(r)
	if want != got || !MadeByAxon(r) {
		t.Fatalf("round trip: recorded %s computed %s", want, got)
	}
	// tamper
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "deadbeef", "cafebabe", 1)), 0o644)
	r, _ = Load(p)
	if want, got, _ := Verify(r); want == got {
		t.Fatal("tampering not detected")
	}
}

func TestNextAndLess(t *testing.T) {
	var rs []*Record
	for _, v := range []string{"0.1.2", "0.1.10", "0.2.0"} {
		rs = append(rs, &Record{Doc: map[string]any{"version": v}})
	}
	if n := Next("0.1", 2, rs); n != "0.1.11" {
		t.Fatalf("Next = %s", n)
	}
	if n := Next("0.3", 0, rs); n != "0.3.0" {
		t.Fatalf("Next = %s", n)
	}
	if !Less("0.1.2", "0.1.10") || Less("0.2.0", "0.1.10") {
		t.Fatal("Less is not numeric")
	}
}
