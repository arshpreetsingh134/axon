package build

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHashDirIsContentAddressed(t *testing.T) {
	d := t.TempDir()
	os.MkdirAll(filepath.Join(d, "node_modules", "x"), 0o755)
	os.WriteFile(filepath.Join(d, "a.go"), []byte("package a"), 0o644)
	os.WriteFile(filepath.Join(d, "node_modules", "x", "big.js"), []byte("ignored"), 0o644)
	h1, _ := HashDir(d)
	os.WriteFile(filepath.Join(d, "node_modules", "x", "big.js"), []byte("changed"), 0o644)
	h2, _ := HashDir(d)
	if h1 != h2 {
		t.Fatal("node_modules must not affect the hash")
	}
	os.WriteFile(filepath.Join(d, "a.go"), []byte("package b"), 0o644)
	if h3, _ := HashDir(d); h3 == h1 {
		t.Fatal("content change must change the hash")
	}
	os.Mkdir(filepath.Join(d, "dist"), 0o755)
	os.WriteFile(filepath.Join(d, "dist", "entry.js"), []byte("x"), 0o644)
	h4, _ := HashDir(d, "dist")
	h5, _ := HashDir(d)
	if h4 == h5 {
		t.Fatal("skip list ignored")
	}
}

func TestDistFresh(t *testing.T) {
	d := t.TempDir()
	os.MkdirAll(filepath.Join(d, "src"), 0o755)
	os.WriteFile(filepath.Join(d, "src", "App.svelte"), []byte("<p/>"), 0o644)
	if distFresh(d) {
		t.Fatal("no dist yet")
	}
	os.MkdirAll(filepath.Join(d, "dist"), 0o755)
	os.WriteFile(filepath.Join(d, "dist", "entry.js"), []byte("x"), 0o644)
	future := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(d, "dist", "entry.js"), future, future)
	if !distFresh(d) {
		t.Fatal("dist newer than sources should be adopted")
	}
	later := future.Add(time.Hour)
	os.Chtimes(filepath.Join(d, "src", "App.svelte"), later, later)
	if distFresh(d) {
		t.Fatal("a newer source must invalidate dist")
	}
}

func TestCoreDockerfile(t *testing.T) {
	df := coreDockerfile("core:1", []string{"COPY --from=iam:1 /mycelfile /app/modules/mycel_iam/module/mycelfile"})
	for _, want := range []string{"COPY --from=core:1 /app_core", "COPY --from=core:1 /shell", "COPY --from=iam:1", "ENV MYCEL_RUNTIME=container", `CMD ["/app/app_core"]`} {
		if !strings.Contains(df, want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
}

func TestCache(t *testing.T) {
	d := t.TempDir()
	c := OpenCache(d)
	c.Put("k", "v")
	if OpenCache(d).Get("k") != "v" {
		t.Fatal("cache not persisted")
	}
}
