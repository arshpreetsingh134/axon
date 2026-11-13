package layout

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, p, s string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindClusterPrefersAxonThenMycel(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, ".mycel.infra", "mycelfile"), "build_type = \"cluster\"\nname = \"x\"\n")
	f, err := FindCluster(d, ModeAuto)
	if err != nil || !f.Layout.Compat() {
		t.Fatalf("mycel repo not found in auto mode: %v", err)
	}
	if _, err := FindCluster(d, ModeOff); err == nil {
		t.Fatal("compat off must ignore mycel files")
	}
	write(t, filepath.Join(d, ".axon.infra", "axonfile"), "build_type = \"cluster\"\nname = \"x\"\n")
	if f, _ := FindCluster(d, ModeAuto); f.Layout.Brand != "axon" {
		t.Fatal("axon files win in auto mode")
	}
	if f, _ := FindCluster(d, ModeMycel); f.Layout.Brand != "mycel" {
		t.Fatal("mycel files win in mycel mode")
	}
	sub := filepath.Join(d, "a", "b")
	os.MkdirAll(sub, 0o755)
	if f, err := FindClusterUpward(sub, ModeAuto); err != nil || f.Dir != d {
		t.Fatalf("upward search: %v", err)
	}
}

func TestFindAppLocations(t *testing.T) {
	for _, rel := range []string{"axonfile", ".axon.infra/axonfile", "module/axonfile", "mycelfile", ".mycel.infra/mycelfile"} {
		d := t.TempDir()
		write(t, filepath.Join(d, rel), "build_type = \"project\"\n")
		if _, err := FindApp(d, ModeAuto); err != nil {
			t.Errorf("%s not found", rel)
		}
	}
	d := t.TempDir()
	write(t, filepath.Join(d, "axonfile"), "build_type = \"cluster\"\n")
	if _, err := FindApp(d, ModeAuto); err == nil {
		t.Error("a cluster descriptor is not an app")
	}
}

func TestNormalizeModeAndConfigDir(t *testing.T) {
	for in, want := range map[string]string{"": "auto", "MYCEL": "mycel", "off": "off", "on": "mycel"} {
		if got, err := NormalizeMode(in); err != nil || got != want {
			t.Errorf("NormalizeMode(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := NormalizeMode("sometimes"); err == nil {
		t.Error("bad mode accepted")
	}
	t.Setenv("XDG_CONFIG_HOME", "/x")
	if ConfigDir("axon") != "/x/axon" {
		t.Error("XDG_CONFIG_HOME ignored")
	}
	if Mycel.K8s("app-env") != "mycel-app-env" || Axon.K8s("app-env") != "axon-app-env" {
		t.Error("k8s names")
	}
}
