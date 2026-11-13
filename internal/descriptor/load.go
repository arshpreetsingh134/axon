package descriptor

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"axon/internal/layout"
)

// LoadCluster decodes a cluster descriptor and returns undecoded (unknown) keys.
func LoadCluster(path string) (*Cluster, []string, error) {
	var c Cluster
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	var unknown []string
	for _, k := range md.Undecoded() {
		unknown = append(unknown, k.String())
	}
	if c.BuildType != "cluster" {
		return nil, nil, fmt.Errorf("%s: build_type is %q, want \"cluster\"", path, c.BuildType)
	}
	return &c, unknown, nil
}

// LoadAppFile decodes a member descriptor.
func LoadAppFile(path string) (*AppFile, error) {
	var a AppFile
	if _, err := toml.DecodeFile(path, &a); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &a, nil
}

// Overlay is <infra>/axon.toml: axon-only settings that sit beside an unchanged
// descriptor (so a mycel repo keeps working with mycel).
type Overlay struct {
	Hooks  Hooks             `toml:"hooks"`
	Images map[string]string `toml:"images"`
	Deploy DeployDefaults    `toml:"deploy"`
}

// OverlayName is the overlay file name inside the infra dir.
const OverlayName = "axon.toml"

// LoadOverlay reads the overlay if present.
func LoadOverlay(infraDir string) (*Overlay, error) {
	p := filepath.Join(infraDir, OverlayName)
	var o Overlay
	if _, err := os.Stat(p); err != nil {
		return &o, nil
	}
	if _, err := toml.DecodeFile(p, &o); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &o, nil
}

// Module is a resolved module of a cored app.
type Module struct {
	Token  string
	Image  string // image-sourced module (after substitutions)
	SrcDir string // source module dir (absolute)
	Spec   *ModuleSpec
	DB     bool
	Lang   string
	Source string // [[compose]].source as written
}

// IsImage reports whether the module ships as a released image.
func (m *Module) IsImage() bool { return m.Image != "" }

// AppModel is a resolved [[app]].
type AppModel struct {
	Decl    *App
	Dir     string
	Found   *layout.Found
	File    *AppFile
	Modules []*Module
	Problem string // why it can't be resolved (missing dir, bad descriptor)
}

// Model is a fully resolved cluster.
type Model struct {
	Found    *layout.Found
	L        layout.Layout
	Dir      string // controller dir
	Infra    string // controller infra dir
	State    string // controller state dir
	Root     string // workspace root (parent of the controller dir)
	C        *Cluster
	Apps     []*AppModel
	Skipped  []string // apps left out by [deploy].skip or disabled
	Unknown  []string // unknown descriptor keys
	Overlay  *Overlay
	ImageMap map[string]string
}

// Load resolves a cluster model from a located descriptor.
func Load(f *layout.Found) (*Model, error) {
	c, unknown, err := LoadCluster(f.Path)
	if err != nil {
		return nil, err
	}
	m := &Model{
		Found: f, L: f.Layout, Dir: f.Dir, Infra: f.InfraDir(), State: f.StateDir(),
		Root: filepath.Dir(f.Dir), C: c, Unknown: unknown,
	}
	ov, err := LoadOverlay(m.Infra)
	if err != nil {
		return nil, err
	}
	m.Overlay = ov
	c.Hooks = c.Hooks.Merge(ov.Hooks)
	m.ImageMap = map[string]string{}
	for k, v := range c.Images {
		m.ImageMap[k] = v
	}
	for k, v := range ov.Images {
		m.ImageMap[k] = v
	}
	mergeDeploy(&c.Deploy, ov.Deploy)
	applySkipFabric(c)
	if c.Deploy.RuntimePrefix != "" {
		m.L.Prefix = c.Deploy.RuntimePrefix
	}
	if c.Name == "" {
		c.Name = strings.TrimSuffix(strings.TrimSuffix(filepath.Base(f.Dir), "."+f.Layout.Brand), ".axon")
	}
	skip := map[string]bool{}
	for _, s := range c.Deploy.Skip {
		skip[s] = true
	}
	for i := range c.Apps {
		a := &c.Apps[i]
		if a.Disabled || skip[a.Name] {
			m.Skipped = append(m.Skipped, a.Name)
			continue
		}
		m.Apps = append(m.Apps, m.resolveApp(a))
	}
	return m, nil
}

func mergeDeploy(d *DeployDefaults, o DeployDefaults) {
	if o.Env != "" {
		d.Env = o.Env
	}
	if o.Target != "" {
		d.Target = o.Target
	}
	if o.Jobs > 0 {
		d.Jobs = o.Jobs
	}
	if o.Timeout != "" {
		d.Timeout = o.Timeout
	}
	if o.Context != "" {
		d.Context = o.Context
	}
	d.Products = d.Products || o.Products
	d.Skip = append(d.Skip, o.Skip...)
	d.SkipFabric = append(d.SkipFabric, o.SkipFabric...)
	if o.RuntimePrefix != "" {
		d.RuntimePrefix = o.RuntimePrefix
	}
	if o.Postgres != "" {
		d.Postgres = o.Postgres
	}
}

// applySkipFabric removes fabric services named in [deploy].skip_fabric.
func applySkipFabric(c *Cluster) {
	for _, f := range c.Deploy.SkipFabric {
		switch f {
		case "temporal":
			c.Fabric.Temporal = nil
		case "keycloak":
			c.Fabric.Keycloak = nil
		case "tracing", "tempo", "grafana":
			c.Fabric.Tracing = nil
		case "clickhouse":
			c.Fabric.Clickhouse = nil
		}
	}
}

// SubstImage applies [images] substitutions.
func (m *Model) SubstImage(img string) string {
	if v, ok := m.ImageMap[img]; ok && v != "" {
		return v
	}
	return img
}

func (m *Model) resolveApp(a *App) *AppModel {
	am := &AppModel{Decl: a, Dir: filepath.Join(m.Root, a.Name)}
	if st, err := os.Stat(am.Dir); err != nil || !st.IsDir() {
		if a.KindOrDefault() == "project" && a.Image != "" {
			return am // a prebuilt image needs no dir
		}
		am.Problem = fmt.Sprintf("app dir %s is missing", am.Dir)
		return am
	}
	found, err := layout.FindApp(am.Dir, layout.ModeAuto)
	if err != nil {
		if a.KindOrDefault() == "project" && a.Image != "" {
			return am
		}
		am.Problem = fmt.Sprintf("no axonfile/mycelfile in %s", am.Dir)
		return am
	}
	am.Found = found
	af, err := LoadAppFile(found.Path)
	if err != nil {
		am.Problem = err.Error()
		return am
	}
	am.File = af
	if !a.Cored() {
		return am
	}
	for _, ce := range af.Compose {
		mod := &Module{Source: ce.Source, Token: ce.Token}
		if ce.Image != "" {
			mod.Image = m.SubstImage(ce.Image)
		} else {
			mod.SrcDir = filepath.Join(am.Dir, filepath.FromSlash(ce.Source))
			if mf, err := layout.FindApp(mod.SrcDir, layout.ModeAuto); err == nil {
				if mfile, err := LoadAppFile(mf.Path); err == nil && mfile.Module != nil {
					mod.Spec = mfile.Module
					if mod.Token == "" {
						mod.Token = mfile.Module.Token
					}
					mod.Lang = mfile.Module.Language
				}
			}
		}
		if mod.Token == "" {
			// modules/<token>/module → <token>
			parts := strings.Split(strings.Trim(ce.Source, "/"), "/")
			if len(parts) >= 2 && parts[len(parts)-1] == "module" {
				mod.Token = parts[len(parts)-2]
			} else {
				mod.Token = parts[len(parts)-1]
			}
		}
		if mod.Lang == "" {
			mod.Lang = "go"
		}
		mod.DB = a.DB != ""
		if ce.DB != nil {
			mod.DB = *ce.DB && a.DB != ""
		}
		am.Modules = append(am.Modules, mod)
	}
	return am
}

// AppByName finds a resolved app.
func (m *Model) AppByName(name string) *AppModel {
	for _, a := range m.Apps {
		if a.Decl.Name == name {
			return a
		}
	}
	return nil
}

// Tokens returns every module token in the cluster.
func (m *Model) Tokens() []string {
	var out []string
	for _, a := range m.Apps {
		for _, mod := range a.Modules {
			out = append(out, mod.Token)
		}
	}
	sort.Strings(out)
	return out
}

// HasToken reports whether any app composes token.
func (m *Model) HasToken(tok string) bool {
	for _, t := range m.Tokens() {
		if t == tok {
			return true
		}
	}
	return false
}

// ── naming ──────────────────────────────────────────────────────────────────

var nonProject = regexp.MustCompile(`[^a-z0-9_-]`)

// Slug turns "acme.all.all.policy_app" into "acme-all-all-policy-app".
func Slug(s string) string {
	return strings.ToLower(strings.NewReplacer(".", "-", "_", "-").Replace(s))
}

// DNS makes a token a DNS label ("mycel_iam" → "mycel-iam").
func DNS(token string) string { return strings.ToLower(strings.ReplaceAll(token, "_", "-")) }

// ComposeProject is the compose project of a member app: <cluster>_<app>.
func ComposeProject(cluster, app string) string { return cluster + "_" + app }

// NormProject applies compose's project-name normalization.
func NormProject(p string) string { return nonProject.ReplaceAllString(strings.ToLower(p), "") }

// ServiceImage is the image repo of a built service of a cored app
// (matches compose's default "<project>-<service>").
func ServiceImage(cluster, app, svc string) string {
	return NormProject(ComposeProject(cluster, app)) + "-" + svc
}

// WorkerImage is the image repo of a project worker.
func WorkerImage(cluster, app string) string { return strings.ToLower(cluster + "_" + app) }

// CoreName is the deployment/alias name of an app core: app-<slug>.
func CoreName(app string) string { return "app-" + Slug(app) }

// ValidLabel reports whether s is a DNS label.
func ValidLabel(s string) bool {
	return regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`).MatchString(s)
}

// SafeName reports whether s is a safe env/release name.
func SafeName(s string) bool { return regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`).MatchString(s) }
