package ports

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAllocateReservesAndReuses(t *testing.T) {
	dir := t.TempDir()
	busy := map[int]bool{5545: true, 8090: true}
	FreeFunc = func(p int) bool { return !busy[p] }
	defer func() { FreeFunc = Free }()
	mycel := filepath.Join(dir, "mycel.toml")
	os.WriteFile(mycel, []byte("old = [5546, 8091, 7233, 8233, 3000, 8123]\n"), 0o600)
	o := Options{Path: filepath.Join(dir, "ports.toml"), ExtraRead: []string{mycel}, Tool: "axon"}
	a, err := Allocate("a", o)
	if err != nil {
		t.Fatal(err)
	}
	// 5545 busy, 5546 reserved by mycel's registry → 5547; 8090 busy, 8091 reserved → 8092
	if a.Postgres() != 5547 || a.Traefik() != 8092 || a.Temporal() != 7234 {
		t.Fatalf("a = %v", a)
	}
	b, _ := Allocate("b", o)
	if b.Postgres() == a.Postgres() || b.Traefik() == a.Traefik() {
		t.Fatalf("b collides with a: %v %v", a, b)
	}
	again, _ := Allocate("a", o)
	if again != a {
		t.Fatalf("re-allocation reshuffled: %v → %v", a, again)
	}
	// the mycel registry's own entry for a cluster is adopted
	old, _ := Allocate("old", o)
	if old.Postgres() != 5546 {
		t.Fatalf("old = %v", old)
	}
	b2, _ := os.ReadFile(o.Path)
	if !strings.Contains(string(b2), "a = [") || !strings.HasPrefix(string(b2), "#") {
		t.Fatalf("registry:\n%s", b2)
	}
}

func TestAllocateExhausted(t *testing.T) {
	FreeFunc = func(p int) bool { return p < 5545 || p > 6000 }
	defer func() { FreeFunc = Free }()
	_, err := Allocate("x", Options{Path: filepath.Join(t.TempDir(), "p.toml")})
	if err == nil || !strings.Contains(err.Error(), "postgres (searched 5545–5945)") {
		t.Fatalf("err = %v", err)
	}
}
