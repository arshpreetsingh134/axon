package build

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// skipDirs are never part of a content hash or copied into a build context.
var skipDirs = map[string]bool{".git": true, "node_modules": true, ".DS_Store": true, "target": true, "__pycache__": true, ".svelte-kit": true}

// HashDir hashes every file under root (relative path + mode + bytes), following
// symlinks, skipping skipDirs and any extra relative paths in skip.
func HashDir(root string, skip ...string) (string, error) {
	h := sha256.New()
	if err := hashInto(h, root, "", skip, 0); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashInto(h io.Writer, root, rel string, skip []string, depth int) error {
	if depth > 24 {
		return fmt.Errorf("%s: too deep (symlink loop?)", root)
	}
	dir := filepath.Join(root, rel)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	for _, e := range ents {
		name := e.Name()
		r := filepath.ToSlash(filepath.Join(rel, name))
		if skipDirs[name] || contains(skip, r) {
			continue
		}
		full := filepath.Join(dir, name)
		st, err := os.Stat(full) // follows symlinks
		if err != nil {
			continue // dangling link
		}
		if st.IsDir() {
			if err := hashInto(h, root, filepath.Join(rel, name), skip, depth+1); err != nil {
				return err
			}
			continue
		}
		fmt.Fprintf(h, "%s\x00%o\x00", r, st.Mode().Perm()&0o111)
		f, err := os.Open(full)
		if err != nil {
			return err
		}
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		h.Write([]byte{0})
	}
	return nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// HashStrings hashes strings.
func HashStrings(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CopyDir copies src into dst (following symlinks, skipping skipDirs).
func CopyDir(src, dst string) error {
	return copyInto(src, dst, 0)
}

func copyInto(src, dst string, depth int) error {
	if depth > 24 {
		return fmt.Errorf("%s: too deep", src)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if skipDirs[e.Name()] {
			continue
		}
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		st, err := os.Stat(s)
		if err != nil {
			continue
		}
		if st.IsDir() {
			if err := copyInto(s, d, depth+1); err != nil {
				return err
			}
			continue
		}
		if err := CopyFile(s, d, st.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// CopyFile copies one file.
func CopyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode|0o200)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// WriteIfChanged writes content only when it differs; reports whether it wrote.
func WriteIfChanged(path, content string, mode os.FileMode) (bool, error) {
	if b, err := os.ReadFile(path); err == nil && string(b) == content {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, []byte(content), mode)
}

// Cache persists input hashes of build steps between runs.
type Cache struct {
	path string
	mu   sync.Mutex
	m    map[string]string
}

// OpenCache loads <state>/cache/build.json.
func OpenCache(stateDir string) *Cache {
	c := &Cache{path: filepath.Join(stateDir, "cache", "build.json"), m: map[string]string{}}
	if b, err := os.ReadFile(c.path); err == nil {
		_ = json.Unmarshal(b, &c.m)
	}
	return c
}

// Get returns a stored hash.
func (c *Cache) Get(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[key]
}

// Put stores a hash and saves.
func (c *Cache) Put(key, hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = hash
	os.MkdirAll(filepath.Dir(c.path), 0o755)
	b, _ := json.MarshalIndent(c.m, "", "  ")
	_ = os.WriteFile(c.path, b, 0o644)
}

func shortHash(h string) string {
	h = strings.TrimPrefix(h, "sha256:")
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
