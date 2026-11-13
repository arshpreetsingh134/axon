// Package scaffold writes new clusters, apps, modules and project workers from
// embedded templates.
package scaffold

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"unicode"

	"axon/internal/layout"
	"axon/internal/ports"
)

//go:embed templates
var fs embed.FS

func render(name string, data any) (string, error) {
	b, err := fs.ReadFile("templates/" + name)
	if err != nil {
		return "", err
	}
	t, err := template.New(name).Parse(string(b))
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := t.Execute(&out, data); err != nil {
		return "", err
	}
	return out.String(), nil
}

// Writer records created files.
type Writer struct {
	Root    string
	Created []string
	DryRun  bool
}

// File writes a file unless it exists (scaffolds never overwrite).
func (w *Writer) File(rel, content string, mode os.FileMode) error {
	p := filepath.Join(w.Root, rel)
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	w.Created = append(w.Created, rel)
	if w.DryRun {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), mode)
}

// Template renders and writes a template.
func (w *Writer) Template(rel, tmpl string, data any, mode os.FileMode) error {
	s, err := render(tmpl, data)
	if err != nil {
		return err
	}
	return w.File(rel, s, mode)
}

// Title turns "acme.policy_app" into "Acme Policy App".
func Title(s string) string {
	f := strings.FieldsFunc(s, func(r rune) bool { return r == '.' || r == '_' || r == '-' || r == ' ' })
	for i, w := range f {
		r := []rune(w)
		r[0] = unicode.ToUpper(r[0])
		f[i] = string(r)
	}
	return strings.Join(f, " ")
}

// Token derives a module token from a name ("acme.web-ui" → "acme_web_ui").
func Token(s string) string {
	s = strings.ToLower(filepath.Base(s))
	return strings.NewReplacer(".", "_", "-", "_", " ", "_").Replace(s)
}

// ClusterData feeds cluster templates.
type ClusterData struct {
	Name, File, Infra, State, CoreImage string
	Ports                               ports.Set
}

// Cluster scaffolds a cluster controller dir (descriptor, env, scripts, docs).
func Cluster(w *Writer, l layout.Layout, d ClusterData) error {
	d.File, d.Infra, d.State = l.File, l.Infra, l.State
	steps := []struct {
		rel, tmpl string
		mode      os.FileMode
	}{
		{filepath.Join(l.Infra, l.File), "cluster/descriptor.toml.tmpl", 0o644},
		{filepath.Join(l.Infra, "scripts", "provision-keycloak.sh"), "cluster/provision-keycloak.sh.tmpl", 0o755},
		{filepath.Join(l.Infra, "scripts", "seed-iam.sql"), "cluster/seed-iam.sql.tmpl", 0o644},
		{"RUNBOOK.md", "cluster/RUNBOOK.md.tmpl", 0o644},
		{".gitignore", "cluster/gitignore.tmpl", 0o644},
	}
	for _, s := range steps {
		if err := w.Template(s.rel, s.tmpl, d, s.mode); err != nil {
			return err
		}
	}
	if err := w.File(filepath.Join(l.Infra, ".env.example"), envExample, 0o644); err != nil {
		return err
	}
	if err := w.File(filepath.Join(l.Infra, "secrets", ".gitkeep"), "", 0o644); err != nil {
		return err
	}
	if err := w.File(filepath.Join(l.Infra, "journeys", ".gitkeep"), "", 0o644); err != nil {
		return err
	}
	if err := w.File(filepath.Join("release", "policy.toml"), "# Release numbering for `axon release create`.\nseries = \"0.1\"\nfirst_patch = 0\n", 0o644); err != nil {
		return err
	}
	env := fmt.Sprintf(`# environments/dev.toml — local development (working tree, release "local").
name      = "dev"
tier      = "dev"
namespace = "%s-dev"
release   = "local"
target    = "docker"          # docker | k8s
`, d.Name)
	return w.File(filepath.Join("environments", "dev.toml"), env, 0o644)
}

const envExample = `# Copy to .env (gitignored) and fill in. Descriptors reference these as "${NAME}".
POSTGRES_PASSWORD=
OIDC_CLIENT_SECRET=
KEYCLOAK_ADMIN_PASSWORD=
SEED_ADMIN_EMAIL=
SEED_ADMIN_PASSWORD=
`

// Journey scaffolds journeys/<app>_smoke.toml (never overwrites).
func Journey(w *Writer, l layout.Layout, app, host string) error {
	return w.Template(filepath.Join(l.Infra, "journeys", app+"_smoke.toml"), "cluster/journey.toml.tmpl",
		map[string]string{"App": app, "Host": host}, 0o644)
}

// ModuleSDKVersion is the module SDK a scaffolded Go module depends on.
var ModuleSDKVersion = "v0.0.3"

// ModuleData feeds module templates.
type ModuleData struct {
	Token, Lang, Element, Label, Title, File, GoModule string
}

// Module scaffolds modules/<token>/module (+ module_test and docs per rings).
func Module(w *Writer, l layout.Layout, d ModuleData, rings map[string]bool) error {
	if d.Lang == "" {
		d.Lang = "go"
	}
	if d.Element == "" {
		d.Element = strings.ReplaceAll(d.Token, "_", "-") + "-app"
	}
	if d.Title == "" {
		d.Title = Title(d.Token)
	}
	if d.Label == "" {
		lang := map[string]string{"go": "Go", "rust": "Rust"}[d.Lang]
		d.Label = fmt.Sprintf("%s (%s)", d.Title, lang)
	}
	d.File = l.File
	d.GoModule = d.Token + "_service"
	base := filepath.Join("modules", d.Token)
	mod := filepath.Join(base, "module")
	if err := w.Template(filepath.Join(mod, l.File), "module/descriptor.toml.tmpl", d, 0o644); err != nil {
		return err
	}
	files := map[string]string{
		filepath.Join(mod, ".dockerignore"):                           "ui/node_modules\nui/dist\nservice/target\n",
		filepath.Join(mod, "README.md"):                               fmt.Sprintf("# %s module\n\nToken `%s`: Postgres schema, `/m/%s/*` routes and the `<%s>` element.\n\n- `service/` — the server (%s)\n- `ui/` — the UI bundle (Svelte + Vite → `ui/dist/entry.js`)\n- `database/` — schema and migrations\n", d.Title, d.Token, d.Token, d.Element, d.Lang),
		filepath.Join(mod, "database", "schema.sql"):                  fmt.Sprintf("-- %s schema (the module owns schema %q).\nCREATE SCHEMA IF NOT EXISTS %s;\n", d.Token, d.Token, d.Token),
		filepath.Join(mod, "database", "migrations", "0001_init.sql"): fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s;\nCREATE TABLE IF NOT EXISTS %s.item (\n  id         BIGSERIAL PRIMARY KEY,\n  name       TEXT NOT NULL,\n  created_at TIMESTAMPTZ NOT NULL DEFAULT now()\n);\n", d.Token, d.Token),
		filepath.Join(mod, "ui", "package.json"):                      fmt.Sprintf("{\n  \"name\": \"@app-mod/%s\",\n  \"private\": true,\n  \"version\": \"0.1.0\",\n  \"type\": \"module\",\n  \"scripts\": { \"build\": \"vite build\" },\n  \"devDependencies\": {\n    \"@sveltejs/vite-plugin-svelte\": \"^5.0.3\",\n    \"svelte\": \"^5.16.0\",\n    \"vite\": \"^6.0.11\"\n  }\n}\n", d.Token),
	}
	for rel, c := range files {
		if err := w.File(rel, c, 0o644); err != nil {
			return err
		}
	}
	for rel, tmpl := range map[string]string{
		filepath.Join(mod, "ui", "vite.config.ts"):    "module/ui/vite.config.ts.tmpl",
		filepath.Join(mod, "ui", "src", "App.svelte"): "module/ui/App.svelte.tmpl",
		filepath.Join(mod, "ui", "src", "entry.ts"):   "module/ui/entry.ts.tmpl",
	} {
		if err := w.Template(rel, tmpl, d, 0o644); err != nil {
			return err
		}
	}
	switch d.Lang {
	case "go":
		svc := filepath.Join(mod, "service")
		if err := w.File(filepath.Join(svc, "go.mod"), fmt.Sprintf("module %s\n\ngo 1.26\n\nrequire github.com/mycelpf/mycel_module_sdk %s\n", d.GoModule, ModuleSDKVersion), 0o644); err != nil {
			return err
		}
		for rel, tmpl := range map[string]string{
			filepath.Join(svc, "main.go"):                "module/go/main.go.tmpl",
			filepath.Join(svc, "logic", "logic.go"):      "module/go/logic.go.tmpl",
			filepath.Join(svc, "logic", "logic_test.go"): "module/go/logic_test.go.tmpl",
			filepath.Join(mod, "Dockerfile"):             "module/go/Dockerfile.tmpl",
		} {
			if err := w.Template(rel, tmpl, d, 0o644); err != nil {
				return err
			}
		}
		if err := w.File(filepath.Join(svc, "logic", "openapi.json"), openAPI(d), 0o644); err != nil {
			return err
		}
	case "rust":
		svc := filepath.Join(mod, "service")
		if err := w.File(filepath.Join(svc, "Cargo.toml"), fmt.Sprintf("[package]\nname = %q\nversion = \"0.1.0\"\nedition = \"2021\"\n\n[dependencies]\n", d.Token), 0o644); err != nil {
			return err
		}
		if err := w.Template(filepath.Join(svc, "src", "main.rs"), "module/rust/main.rs.tmpl", d, 0o644); err != nil {
			return err
		}
		if err := w.Template(filepath.Join(mod, "Dockerfile"), "module/rust/Dockerfile.tmpl", d, 0o644); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported --lang %q (go or rust)", d.Lang)
	}
	if rings["test"] {
		mt := filepath.Join(base, "module_test")
		if err := w.File(filepath.Join(mt, "README.md"), fmt.Sprintf("# %s module tests\n\nBlack-box tests that run against a deployed cluster (`axon app test`).\n", d.Title), 0o644); err != nil {
			return err
		}
		if d.Lang == "go" {
			if err := w.File(filepath.Join(mt, "go.mod"), fmt.Sprintf("module %s_test\n\ngo 1.23\n", d.Token), 0o644); err != nil {
				return err
			}
			test := fmt.Sprintf(`package service_test

import (
	"net/http"
	"os"
	"testing"
)

// TestHello calls the module through the front door when AXON_FRONT_DOOR is set.
func TestHello(t *testing.T) {
	base := os.Getenv("AXON_FRONT_DOOR")
	if base == "" {
		t.Skip("set AXON_FRONT_DOOR (e.g. http://web.lvh.me:8090) to run against a cluster")
	}
	resp, err := http.Get(base + "/m/%s/hello")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		t.Fatalf("status %%d", resp.StatusCode)
	}
}
`, d.Token)
			if err := w.File(filepath.Join(mt, "service", "api_test.go"), test, 0o644); err != nil {
				return err
			}
		}
	}
	if rings["docs"] {
		docs := filepath.Join(base, "docs")
		for name, body := range map[string]string{
			"README.md":   fmt.Sprintf("# %s\n\nWhat the module is for and who uses it.\n", d.Title),
			"DESIGN.md":   fmt.Sprintf("# %s — design\n\nData model, invariants and the routes under `/m/%s/`.\n", d.Title, d.Token),
			"overview.md": fmt.Sprintf("# %s — overview\n\nA one-page tour for new contributors.\n", d.Title),
		} {
			if err := w.File(filepath.Join(docs, name), body, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

func openAPI(d ModuleData) string {
	return fmt.Sprintf(`{
  "openapi": "3.0.3",
  "info": { "title": %q, "version": "0.1.0" },
  "paths": {
    "/m/%s/hello": {
      "get": {
        "parameters": [{ "name": "name", "in": "query", "schema": { "type": "string" } }],
        "responses": { "200": { "description": "a greeting" } }
      }
    }
  }
}
`, d.Title+" API", d.Token)
}

// AppData feeds app templates.
type AppData struct {
	Name, Title, File string
	Modules           []string
}

// App scaffolds an app dir with its seed module.
func App(w *Writer, l layout.Layout, d AppData, mod ModuleData, rings map[string]bool) error {
	d.File = l.File
	if err := w.Template(l.File, "app/descriptor.toml.tmpl", d, 0o644); err != nil {
		return err
	}
	if err := w.Template("README.md", "app/README.md.tmpl", d, 0o644); err != nil {
		return err
	}
	if err := w.File(".gitignore", "**/node_modules/\n**/ui/dist/\n"+l.Infra+"/"+l.State+"/\n.DS_Store\n", 0o644); err != nil {
		return err
	}
	return Module(w, l, mod, rings)
}

// Project scaffolds a project (worker) app with its own Dockerfile.
func Project(w *Writer, l layout.Layout, name, title string) error {
	desc := fmt.Sprintf(`# %s — a PROJECT app: a headless worker built from its own Dockerfile.
build_type = "project"

[app]
name  = %q
title = %q

[project]
dockerfile = "Dockerfile"

[[project.build]]
name    = "image"
command = "docker build -t %s ."

[project.run]
command = "axon up --build"
`, l.File, name, title, strings.ToLower(name))
	if err := w.File(l.File, desc, 0o644); err != nil {
		return err
	}
	return w.File("Dockerfile", "FROM alpine:3\nCMD [\"sh\", \"-c\", \"echo worker started; sleep infinity\"]\n", 0o644)
}

// ParseRings parses --rings test,docs|all|none.
func ParseRings(s string) map[string]bool {
	out := map[string]bool{}
	switch s {
	case "", "all":
		out["test"], out["docs"] = true, true
	case "none":
	default:
		for _, r := range strings.Split(s, ",") {
			out[strings.TrimSpace(r)] = true
		}
	}
	return out
}
