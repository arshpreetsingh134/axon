// Package layout maps axon's file and resource names, and the mycel-compatible
// alternatives it understands (compat mode).
//
// Native axon names: axonfile, .axon.infra/, .axon.infra/.axon/ (state),
// ~/.config/axon/. Compat (mycel) names: mycelfile, .mycel.infra/,
// .mycel.infra/.mycel/, ~/.config/mycel/. A repo's layout is detected from the
// files present, so axon works on an unchanged mycel repo; generated artifacts go
// into that repo's existing (gitignored) generated paths.
package layout

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Layout is a naming scheme.
type Layout struct {
	// Prefix overrides Brand for runtime names (Kubernetes objects, the database
	// role, ClickHouse users) — set to "mycel" after `axon migrate` so existing
	// data and objects stay reachable.
	Prefix string
	Brand  string // "axon" | "mycel"
	File   string // descriptor file name
	Infra  string // infra dir name
	State  string // state dir name inside the infra dir
}

var (
	Axon  = Layout{Brand: "axon", File: "axonfile", Infra: ".axon.infra", State: ".axon"}
	Mycel = Layout{Brand: "mycel", File: "mycelfile", Infra: ".mycel.infra", State: ".mycel"}
)

// Compat reports whether this is the mycel-compatible layout.
func (l Layout) Compat() bool { return l.Brand == "mycel" }

// K8s returns a brand-prefixed Kubernetes object name ("axon-app-env").
// Compat clusters keep mycel's object names so an existing namespace is
// updated in place rather than duplicated.
func (l Layout) K8s(suffix string) string { return l.Runtime() + "-" + suffix }

// Runtime is the prefix of runtime names: Prefix when set, else Brand.
func (l Layout) Runtime() string {
	if l.Prefix != "" {
		return l.Prefix
	}
	return l.Brand
}

// Mode values for --compat / AXON_COMPAT.
const (
	ModeAuto  = "auto"  // prefer axon files, fall back to mycel files
	ModeMycel = "mycel" // prefer mycel files; scaffold with mycel names
	ModeOff   = "off"   // ignore mycel files entirely
)

// NormalizeMode validates a compat mode string.
func NormalizeMode(m string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "", "auto":
		return ModeAuto, nil
	case "mycel", "on", "true", "1":
		return ModeMycel, nil
	case "off", "false", "0", "none":
		return ModeOff, nil
	}
	return "", fmt.Errorf("unknown compat mode %q (auto, mycel or off)", m)
}

// Candidates lists layouts to try, in order, for a compat mode.
func Candidates(mode string) []Layout {
	switch mode {
	case ModeOff:
		return []Layout{Axon}
	case ModeMycel:
		return []Layout{Mycel, Axon}
	}
	return []Layout{Axon, Mycel}
}

// ForNew returns the layout used when scaffolding something new.
func ForNew(mode string) Layout {
	if mode == ModeMycel {
		return Mycel
	}
	return Axon
}

// Found is a located descriptor.
type Found struct {
	Layout Layout
	Path   string // descriptor file
	Dir    string // the directory that owns it (cluster controller or app dir)
}

// InfraDir is <Dir>/<infra>.
func (f *Found) InfraDir() string { return filepath.Join(f.Dir, f.Layout.Infra) }

// StateDir is <Dir>/<infra>/<state>.
func (f *Found) StateDir() string { return filepath.Join(f.InfraDir(), f.Layout.State) }

func exists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// BuildType reads only the build_type key of a descriptor.
func BuildType(path string) string {
	var v struct {
		BuildType string `toml:"build_type"`
	}
	if _, err := toml.DecodeFile(path, &v); err != nil {
		return ""
	}
	return v.BuildType
}

// FindCluster locates a cluster descriptor for dir:
// <dir>/<infra>/<file>, then <dir>/<file> (build_type = "cluster").
func FindCluster(dir, mode string) (*Found, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for _, l := range Candidates(mode) {
		for _, p := range []string{filepath.Join(abs, l.Infra, l.File), filepath.Join(abs, l.File)} {
			if exists(p) && BuildType(p) == "cluster" {
				return &Found{Layout: l, Path: p, Dir: abs}, nil
			}
		}
	}
	return nil, os.ErrNotExist
}

// FindClusterUpward looks in dir and its parents (so verbs work from subdirs).
func FindClusterUpward(dir, mode string) (*Found, error) {
	abs, _ := filepath.Abs(dir)
	for d := abs; ; d = filepath.Dir(d) {
		if f, err := FindCluster(d, mode); err == nil {
			return f, nil
		}
		if filepath.Dir(d) == d {
			return nil, os.ErrNotExist
		}
	}
}

// FindApp locates an app/module/project descriptor in dir: <dir>/<file>,
// <dir>/<infra>/<file>, <dir>/module/<file>. Cluster descriptors are skipped.
func FindApp(dir, mode string) (*Found, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for _, l := range Candidates(mode) {
		for _, p := range []string{
			filepath.Join(abs, l.File),
			filepath.Join(abs, l.Infra, l.File),
			filepath.Join(abs, "module", l.File),
		} {
			if exists(p) {
				if bt := BuildType(p); bt != "cluster" {
					owner := abs
					return &Found{Layout: l, Path: p, Dir: owner}, nil
				}
			}
		}
	}
	return nil, os.ErrNotExist
}

// AppSearchHint is the message used when no app descriptor is found.
func AppSearchHint(mode string) string {
	names := []string{}
	for _, l := range Candidates(mode) {
		names = append(names, l.File)
	}
	return fmt.Sprintf("looked for %s in ./, ./.axon.infra/ (or ./.mycel.infra/) and ./module/; pass --dir <path>", strings.Join(names, " or "))
}

// ConfigDir returns the user config dir for a brand, honoring XDG_CONFIG_HOME.
func ConfigDir(brand string) string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, brand)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", brand)
}

// Env returns the first non-empty value of AXON_<key>, then (compat) MYCEL_<key>.
func Env(key string) string {
	if v := os.Getenv("AXON_" + key); v != "" {
		return v
	}
	return os.Getenv("MYCEL_" + key)
}
