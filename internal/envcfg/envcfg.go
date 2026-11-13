// Package envcfg manages environments/<env>.toml and environments/<env>.overrides.toml.
package envcfg

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"axon/internal/descriptor"
	"axon/internal/tomledit"
)

// Env is environments/<env>.toml.
type Env struct {
	Name         string            `toml:"name" json:"name"`
	Tier         string            `toml:"tier" json:"tier"`
	Namespace    string            `toml:"namespace" json:"namespace"`
	Release      string            `toml:"release" json:"release"`
	Context      string            `toml:"context" json:"context,omitempty"`
	Domain       string            `toml:"domain" json:"domain,omitempty"`
	TLS          bool              `toml:"tls" json:"tls,omitempty"`
	IngressClass string            `toml:"ingress_class" json:"ingress_class,omitempty"`
	HTTPPort     int               `toml:"http_port" json:"http_port,omitempty"`
	Postgres     string            `toml:"postgres" json:"postgres,omitempty"`
	Target       string            `toml:"target" json:"target,omitempty"` // axon: docker | k8s
	PromotesFrom string            `toml:"promotes_from" json:"promotes_from,omitempty"`
	Gates        map[string]string `toml:"gates" json:"gates,omitempty"`
	ExcludeApps  []string          `toml:"exclude_apps" json:"exclude_apps,omitempty"`
	Extensions   []string          `toml:"extensions" json:"extensions,omitempty"`
	Replicas     int               `toml:"replicas" json:"replicas,omitempty"` // axon: default replicas per workload
	AccountID    string            `toml:"account_id" json:"-"`
	Storage      struct {
		Durable struct {
			Backend string `toml:"backend" json:"backend,omitempty"`
			Size    string `toml:"size" json:"size,omitempty"`
			Reclaim string `toml:"reclaim" json:"reclaim,omitempty"`
		} `toml:"durable" json:"durable"`
	} `toml:"storage" json:"storage"`
	Path string `toml:"-" json:"path"`
}

// Overrides is environments/<env>.overrides.toml.
type Overrides struct {
	AppEnv map[string]map[string]string `toml:"app_env"`
	Images map[string]string            `toml:"images"`
	Hooks  descriptor.Hooks             `toml:"hooks"`
}

// Dir returns <controller>/environments.
func Dir(controller string) string { return filepath.Join(controller, "environments") }

// PathFor returns the env file path.
func PathFor(controller, name string) string { return filepath.Join(Dir(controller), name+".toml") }

// OverridesPath returns the overrides file path.
func OverridesPath(controller, name string) string {
	return filepath.Join(Dir(controller), name+".overrides.toml")
}

// List returns env names, sorted.
func List(controller string) ([]string, error) {
	ents, err := os.ReadDir(Dir(controller))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".toml") || strings.HasSuffix(n, ".overrides.toml") {
			continue
		}
		out = append(out, strings.TrimSuffix(n, ".toml"))
	}
	sort.Strings(out)
	return out, nil
}

// Load reads an env and fills defaults.
func Load(controller, cluster, name string) (*Env, error) {
	p := PathFor(controller, name)
	var e Env
	if _, err := os.Stat(p); err != nil {
		return nil, fmt.Errorf("environment %q not found (%s); create it with `axon env new %s`", name, p, name)
	}
	if _, err := toml.DecodeFile(p, &e); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	e.Path = p
	if e.Name == "" {
		e.Name = name
	}
	if e.Tier == "" {
		e.Tier = "dev"
	}
	if e.Namespace == "" {
		e.Namespace = cluster + "-" + name
	}
	if e.Release == "" && e.Tier == "dev" {
		e.Release = "local"
	}
	return &e, nil
}

// Default returns an in-memory dev env (used when environments/dev.toml is absent).
func Default(cluster, name string) *Env {
	e := &Env{Name: name, Tier: "dev", Namespace: cluster + "-" + name, Release: "local"}
	return e
}

// LoadOverrides reads the overrides file (missing → empty).
func LoadOverrides(controller, name string) (*Overrides, error) {
	p := OverridesPath(controller, name)
	var o Overrides
	if _, err := os.Stat(p); err != nil {
		return &o, nil
	}
	if _, err := toml.DecodeFile(p, &o); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &o, nil
}

// Render writes a fresh env file.
func Render(e *Env) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# environments/%s.toml — one deployment environment of this cluster.\n", e.Name)
	b.WriteString("# Edit with `axon env set " + e.Name + " key=value`, or by hand.\n")
	fmt.Fprintf(&b, "name      = %q\n", e.Name)
	fmt.Fprintf(&b, "tier      = %q          # dev | qa | uat | prod\n", e.Tier)
	fmt.Fprintf(&b, "namespace = %q\n", e.Namespace)
	fmt.Fprintf(&b, "release   = %q            # dev deploys the working tree (\"local\"); other tiers pin a release\n", e.Release)
	if e.Target != "" {
		fmt.Fprintf(&b, "target    = %q          # docker | k8s\n", e.Target)
	}
	if e.Context != "" {
		fmt.Fprintf(&b, "context   = %q\n", e.Context)
	}
	if e.Postgres != "" {
		fmt.Fprintf(&b, "postgres  = %q          # in-cluster | docker | external:<host>:<port>\n", e.Postgres)
	}
	if e.HTTPPort > 0 {
		fmt.Fprintf(&b, "http_port = %d\n", e.HTTPPort)
	}
	b.WriteString("\n[storage.durable]\nbackend = \"pvc\"\nsize    = \"20Gi\"\nreclaim = \"retain\"\n")
	return b.String()
}

// SetKey applies one `env set` key=value. It returns the file that changed.
func SetKey(controller string, e *Env, key, value string) (string, error) {
	if strings.HasPrefix(key, "app.") {
		parts := splitAppKey(key)
		if parts == nil {
			return "", fmt.Errorf("app keys look like app.<app>.<KEY>=value")
		}
		return editOverrides(controller, e.Name, func(d *tomledit.Doc) {
			d.Set("app_env."+quoteKey(parts[0]), parts[1], tomledit.Quote(value))
		})
	}
	if strings.HasPrefix(key, "image.") {
		from := strings.TrimPrefix(key, "image.")
		return editOverrides(controller, e.Name, func(d *tomledit.Doc) { d.Set("images", from, tomledit.Quote(value)) })
	}
	return editEnv(e.Path, func(d *tomledit.Doc) error {
		switch key {
		case "domain", "release", "context", "namespace", "tier", "postgres", "target", "ingress_class":
			if key == "release" && !descriptor.SafeName(value) {
				return fmt.Errorf("environment release assignment requires safe environment and version names")
			}
			if key == "tier" && !oneOf(value, "dev", "qa", "uat", "prod") {
				return fmt.Errorf("tier must be dev, qa, uat or prod")
			}
			if key == "target" && !oneOf(value, "docker", "k8s") {
				return fmt.Errorf("target must be docker or k8s")
			}
			d.Set("", key, tomledit.Quote(value))
		case "http-port", "http_port", "replicas":
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("%s wants a number", key)
			}
			d.Set("", strings.ReplaceAll(key, "-", "_"), strconv.Itoa(n))
		case "tls", "ha", "nat", "network-policy":
			b, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("%s wants true or false", key)
			}
			d.Set("", strings.ReplaceAll(key, "-", "_"), strconv.FormatBool(b))
		case "nodes":
			n := strings.SplitN(value, ",", 2)
			d.Set("", "nodes", tomledit.EncodeGuess(n[0]))
			if len(n) == 2 && strings.HasPrefix(n[1], "max=") {
				d.Set("", "max_nodes", tomledit.EncodeGuess(strings.TrimPrefix(n[1], "max=")))
			}
		case "exclude-apps", "extensions", "access":
			var items []string
			for _, s := range strings.Split(value, ",") {
				if s = strings.TrimSpace(s); s != "" {
					items = append(items, s)
				}
			}
			d.Set("", strings.ReplaceAll(key, "-", "_"), tomledit.Encode(items))
		case "promotes-from":
			d.Set("", "promotes_from", tomledit.Quote(value))
		case "kubernetes", "github":
			d.Set("", key, tomledit.Quote(value))
		default:
			if strings.HasPrefix(key, "gate.") {
				d.Set("gates", strings.TrimPrefix(key, "gate."), tomledit.Quote(value))
				return nil
			}
			return fmt.Errorf("unknown env key %q", key)
		}
		return nil
	})
}

// UnsetKey removes one key.
func UnsetKey(controller string, e *Env, key string) (string, error) {
	if strings.HasPrefix(key, "app.") {
		parts := splitAppKey(key)
		if parts == nil {
			return "", fmt.Errorf("app keys look like app.<app>.<KEY>")
		}
		return editOverrides(controller, e.Name, func(d *tomledit.Doc) { d.Unset("app_env."+quoteKey(parts[0]), parts[1]) })
	}
	if strings.HasPrefix(key, "image.") {
		return editOverrides(controller, e.Name, func(d *tomledit.Doc) { d.Unset("images", strings.TrimPrefix(key, "image.")) })
	}
	return editEnv(e.Path, func(d *tomledit.Doc) error {
		switch {
		case strings.HasPrefix(key, "gate."):
			d.Unset("gates", strings.TrimPrefix(key, "gate."))
		case key == "promotes-from":
			d.Unset("", "promotes_from")
		case key == "governance":
			d.RemoveTable("governance")
		default:
			if !d.Unset("", strings.ReplaceAll(key, "-", "_")) {
				return fmt.Errorf("key %q is not set", key)
			}
		}
		return nil
	})
}

// splitAppKey splits "app.<app name, may contain dots>.<KEY>" at the last dot.
func splitAppKey(key string) []string {
	rest := strings.TrimPrefix(key, "app.")
	i := strings.LastIndex(rest, ".")
	if i <= 0 || i == len(rest)-1 {
		return nil
	}
	return []string{rest[:i], rest[i+1:]}
}

func quoteKey(k string) string {
	if strings.ContainsAny(k, ". ") {
		return strconv.Quote(k)
	}
	return k
}

func oneOf(v string, opts ...string) bool {
	for _, o := range opts {
		if v == o {
			return true
		}
	}
	return false
}

func editEnv(path string, fn func(*tomledit.Doc) error) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	d := tomledit.Parse(string(b))
	if err := fn(d); err != nil {
		return "", err
	}
	// validate the result still decodes; revert on failure
	var e Env
	if _, err := toml.Decode(d.String(), &e); err != nil {
		return "", fmt.Errorf("edit would make %s invalid: %w", path, err)
	}
	return path, os.WriteFile(path, []byte(d.String()), 0o644)
}

func editOverrides(controller, env string, fn func(*tomledit.Doc)) (string, error) {
	p := OverridesPath(controller, env)
	b, _ := os.ReadFile(p)
	text := string(b)
	if text == "" {
		text = fmt.Sprintf("# environments/%s.overrides.toml — per-environment overrides.\n# [app_env.\"<app>\"] KEY = \"value\" wins over [app.env]; [images] substitutes image refs.\n", env)
	}
	d := tomledit.Parse(text)
	fn(d)
	var o Overrides
	if _, err := toml.Decode(d.String(), &o); err != nil {
		return "", fmt.Errorf("edit would make %s invalid: %w", p, err)
	}
	return p, os.WriteFile(p, []byte(d.String()), 0o644)
}
