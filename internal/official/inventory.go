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
	"time"

	"axon/internal/layout"
)

// An Inventory is the list of key NAMES an official environment's app env
// secret holds. It never holds values. It lives only in axon's local state
// (plan decision D2), never in a repository:
//
//	<config>/axon/inventory/<cluster>/<env>.keys
type Inventory struct {
	Cluster string
	Env     string
	Keys    []string
	Source  string    // "live (<context>, secret <name>)" or "manual"
	Updated time.Time // zero when unknown
	Path    string
}

var safeSeg = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// InventoryPath is where the inventory for cluster/env is kept.
func InventoryPath(cluster, env string) string {
	return filepath.Join(layout.ConfigDir("axon"), "inventory", safeSeg.ReplaceAllString(cluster, "_"), safeSeg.ReplaceAllString(env, "_")+".keys")
}

var keyNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidKeyName reports whether s can name an environment variable.
func ValidKeyName(s string) bool { return keyNameRE.MatchString(s) }

// LoadInventory reads the inventory; ok is false when there is none yet.
func LoadInventory(cluster, env string) (inv *Inventory, ok bool, err error) {
	inv = &Inventory{Cluster: cluster, Env: env, Path: InventoryPath(cluster, env)}
	f, err := os.Open(inv.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return inv, false, nil
		}
		return nil, false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
		case strings.HasPrefix(line, "# source:"):
			inv.Source = strings.TrimSpace(strings.TrimPrefix(line, "# source:"))
		case strings.HasPrefix(line, "# updated:"):
			inv.Updated, _ = time.Parse(time.RFC3339, strings.TrimSpace(strings.TrimPrefix(line, "# updated:")))
		case strings.HasPrefix(line, "#"):
		case ValidKeyName(line):
			inv.Keys = append(inv.Keys, line)
		default:
			return nil, false, fmt.Errorf("%s: %q is not a key name (the inventory holds names only)", inv.Path, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, false, err
	}
	sort.Strings(inv.Keys)
	return inv, true, nil
}

// Save writes the inventory (owner-only permissions), replacing any old one.
func (inv *Inventory) Save() error {
	for _, k := range inv.Keys {
		if !ValidKeyName(k) {
			return fmt.Errorf("%q is not a key name", k)
		}
	}
	keys := append([]string(nil), inv.Keys...)
	sort.Strings(keys)
	keys = uniq(keys)
	inv.Keys = keys
	if inv.Updated.IsZero() {
		inv.Updated = time.Now().UTC()
	}
	var b strings.Builder
	b.WriteString("# axon key inventory: secret key NAMES only, never values (local state, not a repository file)\n")
	fmt.Fprintf(&b, "# cluster: %s\n# environment: %s\n# source: %s\n# updated: %s\n", inv.Cluster, inv.Env, or(inv.Source, "manual"), inv.Updated.Format(time.RFC3339))
	for _, k := range keys {
		b.WriteString(k + "\n")
	}
	if err := os.MkdirAll(filepath.Dir(inv.Path), 0o700); err != nil {
		return err
	}
	tmp := inv.Path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, inv.Path)
}

// Has reports whether the inventory names key.
func (inv *Inventory) Has(key string) bool {
	i := sort.SearchStrings(inv.Keys, key)
	return i < len(inv.Keys) && inv.Keys[i] == key
}

func uniq(sorted []string) []string {
	var out []string
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}
