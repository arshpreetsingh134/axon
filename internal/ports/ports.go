// Package ports allocates free host ports for a cluster and records them in a
// per-user registry so clusters on one machine never collide.
//
// Registry: <config>/ports.toml — a comment header, then one line per cluster:
//
//	<cluster> = [postgres, traefik, temporal, temporal-ui, grafana, clickhouse]
//
// axon also reads mycel's registry (~/.config/mycel/ports.toml), so clusters
// managed by either tool reserve each other's ports.
package ports

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Service order and default start ports.
var (
	Services = []string{"postgres", "traefik", "temporal", "temporal-ui", "grafana", "clickhouse"}
	Starts   = []int{5545, 8090, 7233, 8233, 3000, 8123}
)

const scanWidth = 400

// Set is a cluster's allocated ports in Services order.
type Set [6]int

func (s Set) Postgres() int   { return s[0] }
func (s Set) Traefik() int    { return s[1] }
func (s Set) Temporal() int   { return s[2] }
func (s Set) TemporalUI() int { return s[3] }
func (s Set) Grafana() int    { return s[4] }
func (s Set) Clickhouse() int { return s[5] }

// Summary renders the allocation line.
func (s Set) Summary() string {
	return fmt.Sprintf("postgres %d · traefik %d · temporal %d/%d · grafana %d · clickhouse %d",
		s[0], s[1], s[2], s[3], s[4], s[5])
}

// Registry is the parsed registry file.
type Registry map[string]Set

var lineRE = regexp.MustCompile(`^\s*("?[^"=\s]+"?)\s*=\s*\[([^\]]*)\]`)

// Load reads a registry (missing file → empty).
func Load(path string) (Registry, error) {
	r := Registry{}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		m := lineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := strings.Trim(m[1], `"`)
		var s Set
		for i, p := range strings.Split(m[2], ",") {
			if i >= len(s) {
				break
			}
			n, _ := strconv.Atoi(strings.TrimSpace(p))
			s[i] = n
		}
		r[name] = s
	}
	return r, nil
}

// Write renders the registry deterministically.
func (r Registry) Write(path, tool string) error {
	var names []string
	for n := range r {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, "# Host ports allocated per cluster by %s. One line per cluster:\n", tool)
	fmt.Fprintf(&b, "#   <cluster> = [%s]\n", strings.Join(Services, ", "))
	b.WriteString("# Edit only while no cluster is being created; remove a line to free its ports.\n")
	for _, n := range names {
		s := r[n]
		key := n
		if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(n) {
			key = strconv.Quote(n)
		}
		fmt.Fprintf(&b, "%s = [%d, %d, %d, %d, %d, %d]\n", key, s[0], s[1], s[2], s[3], s[4], s[5])
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Free reports whether port binds on both 127.0.0.1 and 0.0.0.0.
func Free(port int) bool {
	for _, host := range []string{"127.0.0.1", "0.0.0.0"} {
		l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			return false
		}
		l.Close()
	}
	return true
}

// FreeFunc is swappable in tests.
var FreeFunc = Free

// Options configure Allocate.
type Options struct {
	Path      string   // axon registry
	ExtraRead []string // other registries to honor (mycel's)
	Tool      string
	Preferred *Set // keep these if free (e.g. ports already in a descriptor)
}

// Allocate locks the registry, reuses or allocates ports for cluster, and
// writes the registry only if the entry changed.
func Allocate(cluster string, o Options) (Set, error) {
	if err := os.MkdirAll(filepath.Dir(o.Path), 0o700); err != nil {
		return Set{}, err
	}
	lf, err := os.OpenFile(o.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return Set{}, err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return Set{}, fmt.Errorf("lock %s: %w", o.Path+".lock", err)
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)

	reg, err := Load(o.Path)
	if err != nil {
		return Set{}, err
	}
	reserved := map[int]bool{}
	for n, s := range reg {
		if n == cluster {
			continue
		}
		for _, p := range s {
			reserved[p] = true
		}
	}
	var foreign Set
	for _, extra := range o.ExtraRead {
		other, err := Load(extra)
		if err != nil {
			continue
		}
		for n, s := range other {
			if n == cluster {
				foreign = s
				continue
			}
			for _, p := range s {
				reserved[p] = true
			}
		}
	}
	existing, had := reg[cluster]
	var out Set
	var failed []string
	for i, svc := range Services {
		switch {
		case had && existing[i] > 0:
			out[i] = existing[i]
		case foreign[i] > 0:
			out[i] = foreign[i]
		case o.Preferred != nil && o.Preferred[i] > 0 && !reserved[o.Preferred[i]]:
			out[i] = o.Preferred[i]
		default:
			p := scan(Starts[i], reserved)
			if p == 0 {
				failed = append(failed, fmt.Sprintf("%s (searched %d–%d)", svc, Starts[i], Starts[i]+scanWidth))
				continue
			}
			out[i] = p
		}
		reserved[out[i]] = true
	}
	if len(failed) > 0 {
		return Set{}, fmt.Errorf("no free host port for %s", strings.Join(failed, ", "))
	}
	if !had || existing != out {
		reg[cluster] = out
		if err := reg.Write(o.Path, o.Tool); err != nil {
			return Set{}, err
		}
	}
	return out, nil
}

func scan(start int, reserved map[int]bool) int {
	for p := start; p < start+scanWidth && p < 65535; p++ {
		if reserved[p] {
			continue
		}
		if FreeFunc(p) {
			return p
		}
	}
	return 0
}

// Release removes a cluster's entry.
func Release(cluster, path, tool string) error {
	reg, err := Load(path)
	if err != nil {
		return err
	}
	if _, ok := reg[cluster]; !ok {
		return nil
	}
	delete(reg, cluster)
	return reg.Write(path, tool)
}
