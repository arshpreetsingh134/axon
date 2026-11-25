// Package official reads the layout of a cluster repository that is released
// and deployed by a pipeline to official environments (SPEC.md Appendix C): the
// pipeline workflow file, the source lock, image-backed member files, and the
// image pins and source revisions spread across the generated workflows.
//
// Everything here is read-only. axon never writes this layout's branding; it
// only reads the files so it can check them before a pipeline run does.
package official

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

// WorkflowFile is the pipeline workflow file whose presence marks the layout.
const WorkflowFile = "github/workflow.toml"

// DefaultLock is the source lock path when the workflow file names none.
const DefaultLock = "github/sources.lock.toml"

// MaterializationsDir holds one directory per image-backed member.
const MaterializationsDir = "github/materializations"

// Workflow is github/workflow.toml (the keys axon uses).
type Workflow struct {
	SchemaVersion     int      `toml:"schema_version"`
	ClusterRepository string   `toml:"cluster_repository"`
	SourceLock        string   `toml:"source_lock"`
	RunnerVersion     string   `toml:"runner_version"`
	OfficialRegistry  string   `toml:"official_registry"`
	Environments      []string `toml:"environments"`
	ReleasePlatforms  []string `toml:"release_platforms"`
	// Materializations name, for each image-backed member, the source its image is built from.
	Materializations []Materialization `toml:"materialization"`
}

// Materialization is one [[materialization]] of the workflow file: an image-backed member, the source-lock member its
// image is built from, and where a developer's checkout of that source sits (relative to the workspace root).
type Materialization struct {
	Target            string `toml:"target"`
	SourceMember      string `toml:"source_member"`
	LocalCheckoutPath string `toml:"local_checkout_path"`
	SourcePath        string `toml:"source_path"`
	MetadataPath      string `toml:"metadata_path"`
}

// Layout is a detected official cluster repository.
type Layout struct {
	Root     string
	Workflow Workflow
}

// Detect reports whether root holds the official layout. A missing workflow
// file is not an error; a malformed one is.
func Detect(root string) (*Layout, bool, error) {
	p := filepath.Join(root, WorkflowFile)
	if _, err := os.Stat(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	l := &Layout{Root: root}
	if _, err := toml.DecodeFile(p, &l.Workflow); err != nil {
		return nil, false, fmt.Errorf("%s: %w", WorkflowFile, err)
	}
	return l, true, nil
}

// Rel returns p relative to the repository root, for messages.
func (l *Layout) Rel(p string) string {
	if r, err := filepath.Rel(l.Root, p); err == nil {
		return r
	}
	return p
}

// Owner is the owner (organization) of the cluster repository, e.g. "acme" for
// "github.com/acme/cluster". It resolves bare repository names in workflows.
func (l *Layout) Owner() string {
	parts := strings.Split(strings.Trim(l.Workflow.ClusterRepository, "/"), "/")
	if len(parts) >= 3 {
		return parts[1]
	}
	if len(parts) == 2 {
		return parts[0]
	}
	return ""
}

// Host is the Git host of the cluster repository (default github.com).
func (l *Layout) Host() string {
	parts := strings.Split(strings.Trim(l.Workflow.ClusterRepository, "/"), "/")
	if len(parts) >= 3 && strings.Contains(parts[0], ".") {
		return parts[0]
	}
	return "github.com"
}

// ---- source lock ----

// LockMember is one [[member]] of the source lock.
type LockMember struct {
	AppID      string `toml:"app_id"`
	Repository string `toml:"repository"`
	Commit     string `toml:"commit"`
}

// LockPath is the source lock's path.
func (l *Layout) LockPath() string {
	return filepath.Join(l.Root, or(l.Workflow.SourceLock, DefaultLock))
}

// Lock reads the source lock.
func (l *Layout) Lock() ([]LockMember, error) {
	var doc struct {
		Version int          `toml:"version"`
		Member  []LockMember `toml:"member"`
	}
	if _, err := toml.DecodeFile(l.LockPath(), &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", l.Rel(l.LockPath()), err)
	}
	for _, m := range doc.Member {
		if m.AppID == "" || m.Repository == "" || !IsCommit(m.Commit) {
			return nil, fmt.Errorf("%s: member %q needs app_id, repository and a 40-hex commit", l.Rel(l.LockPath()), m.AppID)
		}
	}
	return doc.Member, nil
}

// ---- repositories ----

// Repo identifies a Git repository on a host.
type Repo struct {
	Host string // github.com
	Slug string // owner/name
}

func (r Repo) String() string { return r.Host + "/" + r.Slug }

// URL is the HTTPS clone URL.
func (r Repo) URL() string { return "https://" + r.Host + "/" + r.Slug + ".git" }

// ParseRepo accepts "https://host/owner/name(.git)", "host/owner/name",
// "owner/name", or a bare "name" (resolved with defaultOwner on defaultHost).
func ParseRepo(s, defaultHost, defaultOwner string) (Repo, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	parts := strings.Split(s, "/")
	switch {
	case len(parts) == 3 && strings.Contains(parts[0], "."):
		return Repo{Host: parts[0], Slug: parts[1] + "/" + parts[2]}, nil
	case len(parts) == 2:
		return Repo{Host: defaultHost, Slug: s}, nil
	case len(parts) == 1 && parts[0] != "" && defaultOwner != "":
		return Repo{Host: defaultHost, Slug: defaultOwner + "/" + parts[0]}, nil
	}
	return Repo{}, fmt.Errorf("cannot read repository %q", s)
}

var commitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// IsCommit reports whether s is a full 40-hex commit id.
func IsCommit(s string) bool { return commitRE.MatchString(s) }

// ---- image-backed members ----

// Member is one image-backed member under github/materializations/.
type Member struct {
	Name    string
	Dir     string
	Images  []string // every image reference the member file or its Dockerfile names
	Sources int      // [[compose]] entries built from a source path instead of an image
}

// Materializations lists the image-backed members.
func (l *Layout) Materializations() ([]Member, error) {
	dir := filepath.Join(l.Root, MaterializationsDir)
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Member
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		m := Member{Name: e.Name(), Dir: filepath.Join(dir, e.Name())}
		for _, f := range []string{"mycelfile", "axonfile"} {
			var doc struct {
				App struct {
					Name string `toml:"name"`
				} `toml:"app"`
				Compose []struct {
					Image  string `toml:"image"`
					Source string `toml:"source"`
				} `toml:"compose"`
			}
			p := filepath.Join(m.Dir, f)
			if _, err := os.Stat(p); err != nil {
				continue
			}
			if _, err := toml.DecodeFile(p, &doc); err != nil {
				return nil, fmt.Errorf("%s: %w", l.Rel(p), err)
			}
			if doc.App.Name != "" {
				m.Name = doc.App.Name
			}
			for _, c := range doc.Compose {
				switch {
				case c.Image != "":
					m.Images = append(m.Images, c.Image)
				case c.Source != "" && c.Source != "module":
					m.Sources++
				}
			}
		}
		for _, df := range []string{".mycel.infra/Dockerfile", ".axon.infra/Dockerfile", "Dockerfile"} {
			b, err := os.ReadFile(filepath.Join(m.Dir, df))
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(b), "\n") {
				f := strings.Fields(line)
				if len(f) >= 2 && strings.EqualFold(f[0], "FROM") {
					img := f[1]
					if strings.HasPrefix(img, "--") && len(f) >= 3 {
						img = f[2]
					}
					m.Images = append(m.Images, img)
				}
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// ---- image pins ----

// Pin is one digest-pinned image reference found in a file.
type Pin struct {
	File   string // relative to the repository root
	Line   int
	Image  string // repository without tag, e.g. ghcr.io/acme/app
	Digest string // sha256:<64-hex>
}

var pinRE = regexp.MustCompile(`([a-z0-9][a-z0-9._-]*(?:\.[a-z0-9._-]+)*(?::[0-9]+)?(?:/[a-z0-9._-]+)+)(?::[A-Za-z0-9._-]+)?@(sha256:[0-9a-f]{64})`)

// ParsePin splits "repo[:tag]@sha256:…" into the repository and digest.
func ParsePin(ref string) (image, digest string, ok bool) {
	m := pinRE.FindStringSubmatch(ref)
	if m == nil || m[0] != strings.TrimSpace(ref) {
		return "", "", false
	}
	return m[1], m[2], true
}

// pinFiles are the files whose image pins must agree: member files and their
// Dockerfiles, and the pipeline workflows. Release records are history and are
// not scanned.
func (l *Layout) pinFiles() ([]string, error) {
	var files []string
	walk := func(dir string, keep func(string) bool) error {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && keep(p) {
				files = append(files, p)
			}
			return nil
		})
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := walk(filepath.Join(l.Root, MaterializationsDir), func(string) bool { return true }); err != nil {
		return nil, err
	}
	if err := walk(filepath.Join(l.Root, ".github", "workflows"), func(p string) bool {
		return strings.HasSuffix(p, ".yml") || strings.HasSuffix(p, ".yaml")
	}); err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// Pins returns every digest-pinned image reference in the member files and
// workflows, in file order.
func (l *Layout) Pins() ([]Pin, error) {
	files, err := l.pinFiles()
	if err != nil {
		return nil, err
	}
	var out []Pin
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		n := 0
		for sc.Scan() {
			n++
			line := sc.Text()
			if t := strings.TrimSpace(line); strings.HasPrefix(t, "#") {
				continue
			}
			for _, m := range pinRE.FindAllStringSubmatch(line, -1) {
				out = append(out, Pin{File: l.Rel(f), Line: n, Image: m[1], Digest: m[2]})
			}
		}
		_ = fh.Close()
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("%s: %w", l.Rel(f), err)
		}
	}
	return out, nil
}

// ---- source revisions in workflows ----

// SourceRef is a source repository built at a fixed commit by a workflow: a
// mapping with repo|repository and revision|ref keys.
type SourceRef struct {
	File   string
	Line   int
	Repo   Repo
	Commit string
}

// SourceRefs scans the workflows for repository + commit pairs.
func (l *Layout) SourceRefs() ([]SourceRef, error) {
	dir := filepath.Join(l.Root, ".github", "workflows")
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []SourceRef
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !(strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")) {
			continue
		}
		p := filepath.Join(dir, name)
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var root yaml.Node
		if err := yaml.Unmarshal(b, &root); err != nil {
			return nil, fmt.Errorf("%s: %w", l.Rel(p), err)
		}
		var visit func(n *yaml.Node)
		visit = func(n *yaml.Node) {
			if n.Kind == yaml.MappingNode {
				var repo, commit string
				line := 0
				for i := 0; i+1 < len(n.Content); i += 2 {
					k, v := n.Content[i].Value, n.Content[i+1]
					if v.Kind != yaml.ScalarNode {
						continue
					}
					switch k {
					case "repo", "repository":
						repo = v.Value
					case "revision", "ref":
						if IsCommit(v.Value) {
							commit, line = v.Value, v.Line
						}
					}
				}
				if repo != "" && commit != "" && !strings.Contains(repo, "${{") {
					if r, err := ParseRepo(repo, l.Host(), l.Owner()); err == nil {
						out = append(out, SourceRef{File: l.Rel(p), Line: line, Repo: r, Commit: commit})
					}
				}
			}
			for _, c := range n.Content {
				visit(c)
			}
		}
		visit(&root)
	}
	return out, nil
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
