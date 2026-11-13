package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"axon/internal/build"
	"axon/internal/check"
	"axon/internal/cli"
	"axon/internal/deploy"
	"axon/internal/descriptor"
	"axon/internal/dotenv"
	"axon/internal/gen"
	"axon/internal/layout"
	"axon/internal/ports"
	"axon/internal/scaffold"
	"axon/internal/tomledit"
	"axon/internal/ui"
)

func clusterVerbs() []*cli.Verb {
	dirOpt := []cli.Arg{{Name: "dir", Optional: true}}
	return []*cli.Verb{
		{Name: "cluster new", Args: []cli.Arg{{Name: "dir"}}, Group: "cluster",
			Summary: "scaffold a cluster workspace: controller dir, descriptor, free host ports, signing key, env, scripts",
			Flags: []cli.Flag{{Name: "name", Kind: cli.String, Usage: "cluster name (default: derived from <dir>)"},
				{Name: "core-image", Kind: cli.String, Usage: "base image every app core is assembled FROM"}, flagDryRun},
			Run: runClusterNew},
		{Name: "cluster add app", Args: dirOpt, Group: "cluster", Summary: "add a member app ([[app]]) and regenerate",
			Flags: []cli.Flag{flagDir, {Name: "name", Kind: cli.String, Usage: "app name = sibling dir name (required)"},
				{Name: "from", Kind: cli.String, Usage: "source dir of the app (linked as <workspace>/<name>)"},
				{Name: "from-image", Kind: cli.String, Usage: "build the app around this released module image (or run it, for --kind project)"},
				{Name: "kind", Kind: cli.String, Default: "module", Usage: "module | iam | project"},
				{Name: "host", Kind: cli.String, Usage: "route host label (<host>.<domain>)"},
				{Name: "db", Kind: cli.String, Usage: "logical Postgres database"},
				{Name: "stateless", Kind: cli.Bool, Usage: "no database"},
				{Name: "token", Kind: cli.String, Usage: "module token for --from-image"},
				{Name: "port", Kind: cli.Int, Usage: "service port (project apps)"},
				{Name: "temporal", Kind: cli.String, Usage: "Temporal address for a worker (temporal:7233)"},
				{Name: "env", Kind: cli.Multi, Usage: "K=V[,K2=V2] added to [app.env] (repeatable)"},
				{Name: "command", Kind: cli.String, Usage: "override the image command (project + image)"},
				{Name: "mount", Kind: cli.String, Usage: "bind-mount this app-relative dir at /workspace"},
				{Name: "external-url", Kind: cli.String, Usage: "front-door URL of a project app (routes it when on the cluster domain)"},
				{Name: "builder", Kind: cli.Bool, Usage: "project app built FROM --from-image (a builder image) with its own Dockerfile"},
				{Name: "ext", Kind: cli.String, Hidden: true}, flagDryRun},
			Run: runClusterAddApp},
		{Name: "cluster add ext", Args: dirOpt, Group: "cluster", Summary: "add an extension service (a project app) wired into a base app's env",
			Flags: []cli.Flag{flagDir, {Name: "name", Kind: cli.String, Usage: "extension app name (required)"},
				{Name: "for", Kind: cli.String, Usage: "the app whose module calls this extension (required)"},
				{Name: "module", Kind: cli.String, Usage: "module token the extension serves"},
				{Name: "env-key", Kind: cli.String, Usage: "env var on the base app that receives the extension URL"},
				{Name: "demo", Kind: cli.String, Usage: "scaffold a demo extension service"},
				{Name: "role-stem", Kind: cli.String, Alias: []string{"tenant"}, Usage: "demo role-name stem"}, flagDryRun},
			Run: runClusterAddExt},
		{Name: "cluster add fabric", Args: []cli.Arg{{Name: "name"}, {Name: "dir", Optional: true}}, Group: "cluster",
			Summary: "add a fabric service (postgres, temporal, keycloak, traefik, tracing, clickhouse, kpi, iam)",
			Flags:   []cli.Flag{flagDir, {Name: "port", Kind: cli.Int, Usage: "host port (clickhouse)"}, flagDryRun}, Run: runClusterAddFabric},
		{Name: "cluster add product", Args: []cli.Arg{{Name: "name"}, {Name: "dir", Optional: true}}, Group: "cluster",
			Summary: "add a [product.<name>] image (deployed only with [deploy] products = true)",
			Flags: []cli.Flag{flagDir, {Name: "image", Kind: cli.String, Usage: "product image ref"}, {Name: "host", Kind: cli.String, Usage: "route host"},
				{Name: "mode", Kind: cli.String, Usage: "product mode"}, {Name: "kb", Kind: cli.String, Usage: "knowledge-base repo"}, {Name: "app", Kind: cli.String, Usage: "member app name"}},
			Run: runClusterAddProduct},
		{Name: "cluster remove app", Args: []cli.Arg{{Name: "name"}, {Name: "dir", Optional: true}}, Group: "cluster", Summary: "remove a member app (and with --purge its dir)",
			Flags: []cli.Flag{flagDir, {Name: "purge", Kind: cli.Bool, Usage: "also delete <workspace>/<name> (asks first)"}}, Run: runClusterRemove("app")},
		{Name: "cluster remove ext", Args: []cli.Arg{{Name: "name"}, {Name: "dir", Optional: true}}, Group: "cluster", Summary: "remove an extension app",
			Flags: []cli.Flag{flagDir, {Name: "purge", Kind: cli.Bool, Usage: "also delete its dir (asks first)"}}, Run: runClusterRemove("app")},
		{Name: "cluster remove fabric", Args: []cli.Arg{{Name: "name"}, {Name: "dir", Optional: true}}, Group: "cluster", Summary: "remove a fabric service",
			Flags: []cli.Flag{flagDir}, Run: runClusterRemove("fabric")},
		{Name: "cluster remove product", Args: []cli.Arg{{Name: "name"}, {Name: "dir", Optional: true}}, Group: "cluster", Summary: "remove a product",
			Flags: []cli.Flag{flagDir}, Run: runClusterRemove("product")},
		{Name: "cluster set", Args: []cli.Arg{{Name: "key=value", Variadic: true}}, Group: "cluster",
			Summary: "set descriptor keys (name, domain, network, core_image, fabric.<x>.<k>, product.<n>.<k>, app.<name>.<k>); a bare key prints it",
			Flags:   []cli.Flag{flagDir, flagCheck}, Run: runClusterSet},
		{Name: "cluster check", Args: dirOpt, Group: "cluster", Summary: "validate the descriptor, member apps, generated-file drift and security posture",
			Flags: []cli.Flag{flagDir, flagJSON}, Run: runClusterCheck},
		{Name: "cluster explain", Args: dirOpt, Group: "cluster", Summary: "print a Markdown architecture overview of the cluster (works without one, too)",
			Flags: []cli.Flag{flagDir}, Run: runClusterExplain},
		{Name: "cluster generate", Args: dirOpt, Group: "cluster", Summary: "regenerate fabric and app compose files (writes only what changed)",
			Flags: []cli.Flag{flagDir, flagCheck, {Name: "diff", Kind: cli.Bool, Usage: "show a line diff of each pending change"},
				{Name: "migrate", Kind: cli.Bool, Usage: "first rewrite legacy shapes (a clc member app → [product.clc])"}},
			Run: runClusterGenerate},
		{Name: "cluster build", Args: []cli.Arg{{Name: "app"}, {Name: "dir", Optional: true}}, Group: "cluster", Summary: "build one member app's images",
			Flags: []cli.Flag{flagDir, {Name: "rebuild", Kind: cli.Bool, Usage: "ignore caches"}, {Name: "jobs", Kind: cli.Int, Usage: "parallel jobs"}, flagDryRun},
			Run:   runClusterBuild},
		{Name: "cluster ports", Args: dirOpt, Group: "cluster", Summary: "show this machine's host-port registry and which ports are in use",
			Flags: []cli.Flag{flagDir, flagJSON}, Run: runClusterPorts},
	}
}

func runClusterNew(c *cli.Ctx) error {
	ws, _ := filepath.Abs(c.Arg(0))
	name := c.String("name")
	if name == "" {
		name = descriptor.Slug(filepath.Base(ws))
	}
	if !descriptor.ValidLabel(name) {
		return cli.Usage("cluster new: name %q is not a DNS label (use --name)", name)
	}
	l := layout.ForNew(compat(c))
	ctrl := filepath.Join(ws, name+"."+l.Brand)
	if _, err := os.Stat(filepath.Join(ctrl, l.Infra, l.File)); err == nil {
		return fmt.Errorf("%s already exists", filepath.Join(ctrl, l.Infra, l.File))
	}
	var set ports.Set
	if c.Bool("dry-run") {
		set = ports.Set{ports.Starts[0], ports.Starts[1], ports.Starts[2], ports.Starts[3], ports.Starts[4], ports.Starts[5]}
	} else {
		var err error
		set, err = ports.Allocate(name, ports.Options{
			Path:      filepath.Join(layout.ConfigDir("axon"), "ports.toml"),
			ExtraRead: []string{filepath.Join(layout.ConfigDir("mycel"), "ports.toml")},
			Tool:      "axon",
		})
		if err != nil {
			return err
		}
	}
	ui.Sub("allocated free host ports — %s", set.Summary())
	w := &scaffold.Writer{Root: ctrl, DryRun: c.Bool("dry-run")}
	if err := scaffold.Cluster(w, l, scaffold.ClusterData{Name: name, CoreImage: c.String("core-image"), Ports: set}); err != nil {
		return err
	}
	if w.DryRun {
		for _, f := range w.Created {
			ui.Out("would create %s", filepath.Join(name+"."+l.Brand, f))
		}
		return nil
	}
	infra := filepath.Join(ctrl, l.Infra)
	envPath := filepath.Join(infra, ".env")
	for _, k := range []string{"POSTGRES_PASSWORD", "OIDC_CLIENT_SECRET", "KEYCLOAK_ADMIN_PASSWORD", "SEED_ADMIN_PASSWORD"} {
		if err := dotenv.Set(envPath, k, deploy.RandomSecret(24)); err != nil {
			return err
		}
	}
	dotenv.Set(envPath, "SEED_ADMIN_EMAIL", "admin@"+name+".local")
	key, err := deploy.NewSigningKeyPEM()
	if err != nil {
		return err
	}
	if err := deploy.WriteSigningKey(filepath.Join(infra, "secrets", "iam_signing_key.pem"), key); err != nil {
		return err
	}
	f, err := layout.FindCluster(ctrl, layout.ModeAuto)
	if err != nil {
		return err
	}
	m, err := descriptor.Load(f)
	if err != nil {
		return err
	}
	if _, err := generate(m, false); err != nil {
		return err
	}
	ui.OK("cluster %q scaffolded → %s (%d files + signing key; secrets in %s/.env)", name, ctrl, len(w.Created), l.Infra)
	ui.Out("next:")
	ui.Out("  cd %s", rel2(mustWd(), ctrl))
	if c.String("core-image") == "" {
		ui.Out("  axon cluster set core_image=<registry>/<core image>:<tag>")
	}
	ui.Out("  axon cluster add app --name <app> --from <path> --host <host> --db <db>")
	ui.Out("  axon up --build")
	return nil
}

func mustWd() string { d, _ := os.Getwd(); return d }

// generate writes generated files and scaffolds missing journeys.
func generate(m *descriptor.Model, check bool) ([]deploy.Change, error) {
	s, err := deploy.Open(m, deploy.Options{Verb: "cluster generate", Target: "docker"})
	if err != nil {
		return nil, err
	}
	changes, err := s.Generate(check)
	if err != nil {
		return nil, err
	}
	if !check {
		w := &scaffold.Writer{Root: m.Dir}
		for _, a := range m.Apps {
			if a.Decl.Cored() && a.Decl.Host != "" {
				if err := scaffold.Journey(w, m.L, a.Decl.Name, a.Decl.Host); err != nil {
					return nil, err
				}
			}
		}
		for _, f := range w.Created {
			ui.Sub("scaffolded %s", f)
		}
	}
	return changes, nil
}

func runClusterGenerate(c *cli.Ctx) error {
	m, err := loadCluster(c, dirArg(c, 0))
	if err != nil {
		return err
	}
	if c.Bool("migrate") {
		if err := migrateLegacy(m); err != nil {
			return err
		}
		if m, err = descriptor.Load(m.Found); err != nil {
			return err
		}
	}
	checkOnly := c.Bool("check") || c.Bool("diff")
	changes, err := generate(m, checkOnly)
	if err != nil {
		return err
	}
	n := 0
	for _, ch := range changes {
		if !ch.Changed {
			continue
		}
		n++
		if checkOnly {
			ui.Out("%s %s", ui.Yellow("~"), ch.Path)
			if c.Bool("diff") {
				ui.Raw(deploy.Diff(m.Dir, ch))
			}
		} else {
			ui.Sub("generated %s", ch.Path)
		}
	}
	if n == 0 {
		ui.Out("up to date")
		return nil
	}
	if c.Bool("check") {
		return cli.Failed()
	}
	if !checkOnly {
		ui.OK("%d file(s) regenerated", n)
	}
	return nil
}

// migrateLegacy rewrites a clc member app into [product.clc].
func migrateLegacy(m *descriptor.Model) error {
	b, err := os.ReadFile(m.Found.Path)
	if err != nil {
		return err
	}
	d := tomledit.Parse(string(b))
	changed := false
	for _, a := range m.C.Apps {
		if strings.HasSuffix(a.Name, ".clc") && !d.HasTable("product.clc") {
			d.RemoveArrayElem("app", "name", a.Name)
			block := fmt.Sprintf("[product.clc]\napp  = %q\nhost = %q", a.Name, or(a.Host, "clc"))
			d.AppendBlock(block)
			changed = true
			ui.Sub("migrated [[app]] %s → [product.clc]", a.Name)
		}
	}
	if !changed {
		return nil
	}
	return os.WriteFile(m.Found.Path, []byte(d.String()), 0o644)
}

func runClusterCheck(c *cli.Ctx) error {
	m, err := loadCluster(c, dirArg(c, 0))
	if err != nil {
		return err
	}
	fs := check.FromProblems(descriptor.ValidateCluster(m))
	if changes, err := generate(m, true); err == nil {
		var stale []string
		for _, ch := range changes {
			if ch.Changed {
				stale = append(stale, ch.Path)
			}
		}
		if len(stale) > 0 {
			fs = append(fs, check.Finding{ID: "CONFORMANCE_CONFIG_DRIFT", Concern: "conformance", Severity: "risk", Subject: "generated files",
				Cause: fmt.Sprintf("%d generated file(s) are stale", len(stale)), Fix: "axon cluster generate"})
		}
	}
	for _, am := range m.Apps {
		if am.File != nil {
			for _, p := range descriptor.ValidateAppFile(am.File) {
				p.Subject = am.Decl.Name + " " + p.Subject
				fs = append(fs, check.FromProblems([]descriptor.Problem{p})...)
			}
		}
	}
	fs = append(fs, check.Security(m, check.GitIgnored(m.Dir))...)
	fs = append(fs, check.DevX(m)...)
	if len(m.Skipped) > 0 {
		fs = append(fs, check.Finding{ID: "CONFORMANCE_APPS_SKIPPED", Concern: "conformance", Severity: "note", Subject: "deploy",
			Cause: "left out of deploys: " + strings.Join(m.Skipped, ", ")})
	}
	if m.L.Compat() {
		fs = append(fs, check.Finding{ID: "COMPAT_MYCEL_LAYOUT", Concern: "conformance", Severity: "note", Subject: m.L.Infra,
			Cause: "a mycel repo, used in compat mode (mycel keeps working on it)", Fix: "axon migrate --apply to switch to axon names"})
	}
	if c.Bool("json") {
		if fs == nil {
			fs = []check.Finding{}
		}
		ui.JSON(map[string]any{"findings": fs})
	} else {
		check.Print(fs)
	}
	if b, _, _ := check.Count(fs); b > 0 {
		return cli.Failed()
	}
	return nil
}

func runClusterBuild(c *cli.Ctx) error {
	m, err := loadCluster(c, dirArg(c, 1))
	if err != nil {
		return err
	}
	app := c.Arg(0)
	if m.AppByName(app) == nil {
		return fmt.Errorf("no app %q in %s", app, m.C.Name)
	}
	res, err := build.Run(m, build.Options{Apps: []string{app}, Rebuild: c.Bool("rebuild"), Jobs: c.Int("jobs"), DryRun: c.Bool("dry-run")})
	if err != nil {
		return err
	}
	if !c.Bool("dry-run") {
		deploy.RecordTags(m.State, res.Tags)
	}
	for _, img := range res.Images {
		state := "built"
		if img.Cached {
			state = "unchanged"
		}
		ui.Out("%s  %s", img.Ref(), state)
	}
	return nil
}

// ── membership mutations ───────────────────────────────────────────────────

func editDescriptor(m *descriptor.Model, dry bool, fn func(*tomledit.Doc) error) error {
	b, err := os.ReadFile(m.Found.Path)
	if err != nil {
		return err
	}
	d := tomledit.Parse(string(b))
	if err := fn(d); err != nil {
		return err
	}
	tmp := m.Found.Path + ".axon-tmp"
	if err := os.WriteFile(tmp, []byte(d.String()), 0o644); err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, _, err := descriptor.LoadCluster(tmp); err != nil {
		return fmt.Errorf("the edit would make the descriptor invalid (reverted): %w", err)
	}
	if dry {
		ui.Raw(deployDiff(string(b), d.String()))
		return nil
	}
	return os.WriteFile(m.Found.Path, []byte(d.String()), 0o644)
}

func deployDiff(a, b string) string { return lineDiffStr(a, b) }

func lineDiffStr(a, b string) string {
	al := strings.Split(a, "\n")
	in := map[string]bool{}
	for _, l := range al {
		in[l] = true
	}
	var out strings.Builder
	for _, l := range strings.Split(b, "\n") {
		if !in[l] {
			out.WriteString("+ " + l + "\n")
		}
	}
	return out.String()
}

func regen(m *descriptor.Model) error {
	m2, err := descriptor.Load(m.Found)
	if err != nil {
		return err
	}
	changes, err := generate(m2, false)
	if err != nil {
		return err
	}
	for _, ch := range changes {
		if ch.Changed {
			ui.Sub("generated %s", ch.Path)
		}
	}
	return nil
}

func runClusterAddApp(c *cli.Ctx) error {
	m, err := loadCluster(c, dirArg(c, 0))
	if err != nil {
		return err
	}
	name := c.String("name")
	if name == "" {
		return cli.Usage("cluster add app: --name is required")
	}
	if m.C.App(name) != nil {
		return fmt.Errorf("app %q is already a member", name)
	}
	kind := c.String("kind")
	if kind != "module" && kind != "iam" && kind != "project" {
		return cli.Usage("cluster add app: --kind must be module, iam or project")
	}
	from, fromImage := c.String("from"), c.String("from-image")
	if from == "" && fromImage == "" {
		return cli.Usage("cluster add app: --from <dir> is required unless --from-image is given")
	}
	env := map[string]string{}
	for _, e := range c.Strings("env") {
		for _, kv := range strings.Split(e, ",") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return cli.Usage("cluster add app: --env wants K=V, got %q", kv)
			}
			env[strings.TrimSpace(k)] = v
		}
	}
	if kind == "project" && c.String("temporal") != "" && m.C.Fabric.Temporal == nil {
		return fmt.Errorf("[fabric.temporal] must be present for an app that declares temporal (axon cluster add fabric temporal)")
	}
	appDir := filepath.Join(m.Root, name)
	dry := c.Bool("dry-run")
	// place the app dir
	if from != "" {
		src, _ := filepath.Abs(from)
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("--from %s: %w", from, err)
		}
		if src != appDir {
			if _, err := os.Lstat(appDir); err == nil {
				return fmt.Errorf("%s already exists", appDir)
			}
			if !dry {
				if err := os.Symlink(src, appDir); err != nil {
					return err
				}
				ui.Sub("linked %s → %s", rel2(m.Root, appDir), src)
			}
		}
		if f, err := layout.FindApp(src, compat(c)); err == nil {
			if af, err := descriptor.LoadAppFile(f.Path); err == nil && af.Module != nil && kind != "iam" && kind != "module" {
				return cli.Usage("cluster add app: --kind override: only iam is valid for a build_type=module source")
			}
		}
	} else if kind != "project" || c.Bool("builder") {
		tok := c.String("token")
		if tok == "" {
			tok = scaffold.Token(strings.SplitN(filepath.Base(strings.SplitN(fromImage, ":", 2)[0]), "@", 2)[0])
		}
		w := &scaffold.Writer{Root: appDir, DryRun: dry}
		if kind == "project" {
			body := fmt.Sprintf("build_type = \"project\"\n\n[app]\nname  = %q\ntitle = %q\n\n[project]\ndockerfile = \"Dockerfile\"\n\n[[project.build]]\nname    = \"image\"\ncommand = \"docker build .\"\n", name, scaffold.Title(name))
			if err := w.File(m.L.File, body, 0o644); err != nil {
				return err
			}
			if err := w.File("Dockerfile", "FROM "+fromImage+"\n", 0o644); err != nil {
				return err
			}
		} else {
			body := fmt.Sprintf("# %s — an app composing the released module image %s.\nbuild_type = \"app\"\n\n[app]\nname               = %q\ntitle              = %q\nsearch_placeholder = \"Search %s…\"\n\n[home]\ntitle = \"Welcome to %s\"\nbody  = \"This app composes a released module image.\"\n\n[[compose]]\nsource = \"modules/%s/module\"\nimage  = %q\ntoken  = %q\ndb     = %v\n",
				m.L.File, fromImage, name, scaffold.Title(name), scaffold.Title(name), scaffold.Title(name), tok, fromImage, tok, !c.Bool("stateless"))
			if err := w.File(m.L.File, body, 0o644); err != nil {
				return err
			}
			if err := w.File(filepath.Join("modules", ".gitkeep"), "", 0o644); err != nil {
				return err
			}
		}
		for _, f := range w.Created {
			ui.Sub("created %s", filepath.Join(rel2(m.Root, appDir), f))
		}
	}
	// the [[app]] block
	var b strings.Builder
	b.WriteString("[[app]]\n")
	fmt.Fprintf(&b, "name = %q\n", name)
	if kind != "module" {
		fmt.Fprintf(&b, "kind = %q\n", kind)
	}
	short := strings.ToLower(name[strings.LastIndex(name, ".")+1:])
	if kind != "project" {
		host := or(c.String("host"), descriptor.Slug(short))
		fmt.Fprintf(&b, "host = %q\n", host)
		if !c.Bool("stateless") {
			fmt.Fprintf(&b, "db   = %q\n", or(c.String("db"), strings.ReplaceAll(descriptor.Slug(name), "-", "_")))
		}
	} else {
		if c.String("db") != "" {
			fmt.Fprintf(&b, "db = %q\n", c.String("db"))
		}
		if fromImage != "" && !c.Bool("builder") {
			fmt.Fprintf(&b, "image = %q\n", fromImage)
		}
		if t := c.String("temporal"); t != "" {
			fmt.Fprintf(&b, "temporal = %q\n", t)
		}
		if c.Int("port") > 0 {
			fmt.Fprintf(&b, "port = %d\n", c.Int("port"))
		}
		if v := c.String("command"); v != "" {
			fmt.Fprintf(&b, "command = %q\n", v)
		}
		if v := c.String("mount"); v != "" {
			fmt.Fprintf(&b, "mount = %q\n", v)
		}
		if v := c.String("external-url"); v != "" {
			fmt.Fprintf(&b, "external_url = %q\n", v)
		}
	}
	if len(env) > 0 {
		b.WriteString("[app.env]\n")
		for _, k := range dotenv.Keys(env) {
			fmt.Fprintf(&b, "%s = %q\n", k, env[k])
		}
	}
	if err := editDescriptor(m, dry, func(d *tomledit.Doc) error { d.AppendBlock(b.String()); return nil }); err != nil {
		return err
	}
	if dry {
		return nil
	}
	ui.OK("added app %q to %s", name, m.C.Name)
	return regen(m)
}

func runClusterAddExt(c *cli.Ctx) error {
	m, err := loadCluster(c, dirArg(c, 0))
	if err != nil {
		return err
	}
	name, forApp := c.String("name"), c.String("for")
	if name == "" || forApp == "" {
		return cli.Usage("cluster add ext: --name and --for are required")
	}
	base := m.C.App(forApp)
	if base == nil {
		return fmt.Errorf("--for %q is not a member app", forApp)
	}
	if m.C.App(name) != nil {
		return fmt.Errorf("app %q is already a member", name)
	}
	dry := c.Bool("dry-run")
	appDir := filepath.Join(m.Root, name)
	w := &scaffold.Writer{Root: appDir, DryRun: dry}
	if err := scaffold.Project(w, m.L, name, scaffold.Title(name)); err != nil {
		return err
	}
	if demo := c.String("demo"); demo != "" {
		stem := or(c.String("role-stem"), m.C.Name)
		_ = w.File("README.md", fmt.Sprintf("# %s\n\nDemo extension %q for %s (role stem %q).\n", name, demo, forApp, stem), 0o644)
	}
	envKey := c.String("env-key")
	if envKey == "" {
		envKey = strings.ToUpper(strings.ReplaceAll(or(c.String("module"), "ext"), "-", "_")) + "_EXT_URL"
	}
	url := fmt.Sprintf("http://%s:8090", descriptor.CoreName(name))
	err = editDescriptor(m, dry, func(d *tomledit.Doc) error {
		d.AppendBlock(fmt.Sprintf("[[app]]\nname = %q\nkind = \"project\"\nport = 8090", name))
		return nil
	})
	if err != nil {
		return err
	}
	if dry {
		return nil
	}
	ui.OK("added extension %q (project app); set %s = %q under [app.env] of %s", name, envKey, url, forApp)
	return regen(m)
}

var fabricBlocks = map[string]func(m *descriptor.Model, port int) string{
	"postgres": func(m *descriptor.Model, p int) string {
		return fmt.Sprintf("[fabric.postgres]\nimage = %q\nport  = %d", gen.ImgPostgres, nz(p, 5545))
	},
	"temporal": func(m *descriptor.Model, p int) string {
		return fmt.Sprintf("[fabric.temporal]\nimage   = %q\nshards  = 128\nport    = %d\nui_port = %d", gen.ImgTemporal, nz(p, 7233), 8233)
	},
	"keycloak": func(m *descriptor.Model, p int) string {
		return fmt.Sprintf("[fabric.keycloak]\nimage  = %q\nrealm  = %q\nclient = %q\nsecret = \"${OIDC_CLIENT_SECRET}\"", gen.ImgKeycloak, m.C.Name, m.C.Name+"-iam")
	},
	"traefik": func(m *descriptor.Model, p int) string {
		return fmt.Sprintf("[fabric.traefik]\nhttp = %d", nz(p, 8090))
	},
	"tracing": func(m *descriptor.Model, p int) string {
		return fmt.Sprintf("[fabric.tracing]\ntempo_image   = %q\ngrafana_image = %q\ngrafana_port  = %d", gen.ImgTempo, gen.ImgGrafana, nz(p, 3000))
	},
	"clickhouse": func(m *descriptor.Model, p int) string {
		return fmt.Sprintf("# The business-event store behind the kpi module (only kpi gets a credential).\n[fabric.clickhouse]\nimage     = %q\nhttp_port = %d\ndatabase  = %q\nvolume    = { name = \"clickhouse-data\", kind = \"durable\", mount = \"/var/lib/clickhouse\" }", gen.ImgClickhouse, nz(p, 8123), m.L.Runtime()+"_kpi")
	},
	"kpi": func(m *descriptor.Model, p int) string { return "[fabric.kpi]\nbackend = \"kpi:7373\"" },
	"iam": func(m *descriptor.Model, p int) string {
		return fmt.Sprintf("[fabric.iam]\nissuer      = %q\naudience    = %q\nbackend     = \"mycel_iam:7373\"\nsigning_key = %q", m.C.Name, m.C.Name+"-app", m.L.Infra+"/secrets/iam_signing_key.pem")
	},
}

func nz(v, d int) int {
	if v > 0 {
		return v
	}
	return d
}

func runClusterAddFabric(c *cli.Ctx) error {
	m, err := loadCluster(c, dirArg(c, 1))
	if err != nil {
		return err
	}
	name := c.Arg(0)
	mk, ok := fabricBlocks[name]
	if !ok {
		var names []string
		for k := range fabricBlocks {
			names = append(names, k)
		}
		sort.Strings(names)
		return cli.Usage("cluster add fabric: unknown fabric %q (%s)", name, strings.Join(names, ", "))
	}
	if c.Has("port") && name != "clickhouse" {
		return cli.Usage("cluster add fabric: --port applies only to clickhouse")
	}
	port := c.Int("port")
	if name == "clickhouse" && port == 0 {
		reg, _ := ports.Load(filepath.Join(layout.ConfigDir("axon"), "ports.toml"))
		if s, ok := reg[m.C.Name]; ok {
			port = s.Clickhouse()
		}
	}
	err = editDescriptor(m, c.Bool("dry-run"), func(d *tomledit.Doc) error {
		if d.HasTable("fabric." + name) {
			return fmt.Errorf("[fabric.%s] already exists", name)
		}
		d.InsertBlockBeforeArray("app", mk(m, port))
		return nil
	})
	if err != nil || c.Bool("dry-run") {
		return err
	}
	ui.OK("added [fabric.%s]", name)
	return regen(m)
}

func runClusterAddProduct(c *cli.Ctx) error {
	m, err := loadCluster(c, dirArg(c, 1))
	if err != nil {
		return err
	}
	name := c.Arg(0)
	var b strings.Builder
	fmt.Fprintf(&b, "[product.%s]\n", name)
	for _, k := range []string{"image", "host", "app", "kb", "mode"} {
		if v := c.String(k); v != "" {
			fmt.Fprintf(&b, "%s = %q\n", k, v)
		}
	}
	err = editDescriptor(m, false, func(d *tomledit.Doc) error {
		if d.HasTable("product." + name) {
			return fmt.Errorf("[product.%s] already exists", name)
		}
		d.AppendBlock(b.String())
		return nil
	})
	if err != nil {
		return err
	}
	ui.OK("added [product.%s] (deployed only with [deploy] products = true)", name)
	return regen(m)
}

func runClusterRemove(what string) func(*cli.Ctx) error {
	return func(c *cli.Ctx) error {
		m, err := loadCluster(c, dirArg(c, 1))
		if err != nil {
			return err
		}
		name := c.Arg(0)
		purgeDir, purgeLink := "", false
		if what == "app" && c.Bool("purge") {
			p := filepath.Join(m.Root, name)
			if st, err := os.Lstat(p); err == nil {
				purgeDir, purgeLink = p, st.Mode()&os.ModeSymlink != 0
				if !purgeLink && !ui.Confirm("Delete "+p+" and everything in it?", c.G.Yes) {
					return fmt.Errorf("not deleting %s without confirmation (pass --yes); nothing was changed", p)
				}
			}
		}
		err = editDescriptor(m, false, func(d *tomledit.Doc) error {
			var ok bool
			switch what {
			case "app":
				ok = d.RemoveArrayElem("app", "name", name)
			case "fabric":
				if name == "postgres" {
					return fmt.Errorf("[fabric.postgres] is required")
				}
				ok = d.RemoveTable("fabric." + name)
			case "product":
				ok = d.RemoveTable("product." + name)
			}
			if !ok {
				return fmt.Errorf("no %s %q in the descriptor", what, name)
			}
			return nil
		})
		if err != nil {
			return err
		}
		ui.OK("removed %s %q", what, name)
		if purgeDir != "" {
			if purgeLink {
				os.Remove(purgeDir)
				ui.Sub("removed link %s", purgeDir)
			} else {
				if err := os.RemoveAll(purgeDir); err != nil {
					return err
				}
				ui.Sub("deleted %s", purgeDir)
			}
		} else if what == "app" {
			ui.Note("the app dir %s is untouched (pass --purge to delete it)", filepath.Join(m.Root, name))
		}
		return regen(m)
	}
}

func runClusterSet(c *cli.Ctx) error {
	m, err := loadCluster(c, or(c.String("dir"), "."))
	if err != nil {
		return err
	}
	if len(c.Args) == 0 {
		return cli.Usage("cluster set: missing required argument(s): key=value")
	}
	b, _ := os.ReadFile(m.Found.Path)
	text := string(b)
	changed := false
	for _, kv := range c.Args {
		k, v, hasV := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "app.") {
			rest := strings.TrimPrefix(k, "app.")
			i := strings.LastIndex(rest, ".")
			if i <= 0 {
				return cli.Usage("cluster set: app keys look like app.<name>.<key>")
			}
			appName, field := rest[:i], rest[i+1:]
			a := m.C.App(appName)
			if a == nil {
				return fmt.Errorf("no [[app]] named %q", appName)
			}
			if !hasV {
				ui.Out("%v", appField(a, field))
				continue
			}
			if c.Bool("check") {
				ui.Out("would set %s = %s", k, v)
				continue
			}
			if text, err = setAppKey(text, appName, field, v); err != nil {
				return err
			}
			changed = true
			continue
		}
		table, key := "", k
		if strings.Count(k, ".") >= 1 && (strings.HasPrefix(k, "fabric.") || strings.HasPrefix(k, "product.") || strings.HasPrefix(k, "deploy.") || strings.HasPrefix(k, "hooks.")) {
			i := strings.LastIndex(k, ".")
			table, key = k[:i], k[i+1:]
		}
		doc := tomledit.Parse(text)
		if !hasV {
			val, _ := doc.Get(table, key)
			ui.Out("%s", val)
			continue
		}
		if c.Bool("check") {
			ui.Out("would set %s = %s", k, v)
			continue
		}
		enc := tomledit.EncodeGuess(v)
		if key == "core_image" || key == "name" || key == "domain" || key == "network" || key == "image" || key == "host" {
			enc = tomledit.Quote(v)
		}
		doc.Set(table, key, enc)
		text = doc.String()
		changed = true
	}
	if !changed {
		return nil
	}
	if err := editDescriptor(m, false, func(d *tomledit.Doc) error { *d = *tomledit.Parse(text); return nil }); err != nil {
		return err
	}
	ui.OK("updated %s", rel2(m.Dir, m.Found.Path))
	return regen(m)
}

func appField(a *descriptor.App, f string) any {
	switch f {
	case "kind":
		return a.KindOrDefault()
	case "host":
		return a.Host
	case "db":
		return a.DB
	case "port":
		return a.Port
	case "image":
		return a.Image
	case "replicas":
		return a.Replicas
	case "temporal":
		return a.Temporal
	case "command":
		return a.Command
	case "mount":
		return a.Mount
	case "external_url":
		return a.ExternalURL
	case "disabled":
		return a.Disabled
	}
	return a.Env[f]
}

// appBlockKeys are the [[app]] keys; any other key set on an app goes to its [app.env].
var appBlockKeys = map[string]bool{"name": true, "kind": true, "host": true, "db": true, "port": true, "temporal": true,
	"image": true, "command": true, "mount": true, "external_url": true, "replicas": true, "disabled": true}

func isHeader(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), "[") }

func kvKey(l string) string {
	t := strings.TrimSpace(l)
	if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "[") {
		return ""
	}
	k, _, ok := strings.Cut(t, "=")
	if !ok {
		return ""
	}
	return strings.Trim(strings.TrimSpace(k), `"`)
}

// setAppKey sets one key of the [[app]] named app, as text (comments survive):
// [[app]] keys in the block itself, anything else in its [app.env] table.
func setAppKey(text, app, field, value string) (string, error) {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	start := -1
	for i := 0; i < len(lines) && start < 0; i++ {
		if strings.TrimSpace(lines[i]) != "[[app]]" {
			continue
		}
		for j := i + 1; j < len(lines) && !isHeader(lines[j]); j++ {
			if kvKey(lines[j]) == "name" {
				_, v, _ := strings.Cut(lines[j], "=")
				if strings.Trim(strings.TrimSpace(strings.SplitN(v, "#", 2)[0]), `"'`) == app {
					start = i
				}
			}
		}
	}
	if start < 0 {
		return "", fmt.Errorf("no [[app]] named %q", app)
	}
	// block = [start, end): until the next header that is not an [app.*] sub-table
	end := len(lines)
	for j := start + 1; j < len(lines); j++ {
		t := strings.TrimSpace(lines[j])
		if strings.HasPrefix(t, "[") && !strings.HasPrefix(t, "[app.") {
			end = j
			break
		}
	}
	for end > start+1 && (strings.TrimSpace(lines[end-1]) == "" || strings.HasPrefix(strings.TrimSpace(lines[end-1]), "#")) {
		end--
	}
	insert := func(at int, l ...string) {
		lines = append(lines[:at], append(l, lines[at:]...)...)
	}
	setIn := func(from, to int, enc string) {
		last := from - 1
		for j := from; j < to; j++ {
			if kvKey(lines[j]) == field {
				lines[j] = field + " = " + enc
				return
			}
			if kvKey(lines[j]) != "" {
				last = j
			}
		}
		insert(last+1, field+" = "+enc)
	}
	if appBlockKeys[field] {
		mainEnd := end
		for j := start + 1; j < end; j++ {
			if isHeader(lines[j]) {
				mainEnd = j
				break
			}
		}
		enc := tomledit.EncodeGuess(value)
		if field == "host" || field == "db" || field == "image" || field == "name" || field == "command" {
			enc = tomledit.Quote(value)
		}
		setIn(start+1, mainEnd, enc)
	} else {
		envAt := -1
		for j := start + 1; j < end; j++ {
			if strings.TrimSpace(lines[j]) == "[app.env]" {
				envAt = j
				break
			}
		}
		if envAt < 0 {
			insert(end, "[app.env]", field+" = "+tomledit.Quote(value))
		} else {
			envEnd := end
			for j := envAt + 1; j < end; j++ {
				if isHeader(lines[j]) {
					envEnd = j
					break
				}
			}
			setIn(envAt+1, envEnd, tomledit.Quote(value))
		}
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func runClusterPorts(c *cli.Ctx) error {
	reg, err := ports.Load(filepath.Join(layout.ConfigDir("axon"), "ports.toml"))
	if err != nil {
		return err
	}
	mreg, _ := ports.Load(filepath.Join(layout.ConfigDir("mycel"), "ports.toml"))
	type row struct {
		Cluster string    `json:"cluster"`
		Tool    string    `json:"tool"`
		Ports   ports.Set `json:"ports"`
	}
	rows := []row{}
	for n, s := range reg {
		rows = append(rows, row{n, "axon", s})
	}
	for n, s := range mreg {
		rows = append(rows, row{n, "mycel", s})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Cluster < rows[j].Cluster })
	if c.Bool("json") {
		return ui.JSON(rows)
	}
	t := [][]string{append([]string{"CLUSTER", "TOOL"}, ports.Services...)}
	for _, r := range rows {
		line := []string{r.Cluster, r.Tool}
		for _, p := range r.Ports {
			cell := strconv.Itoa(p)
			if p > 0 && !ports.Free(p) {
				cell += "*"
			}
			line = append(line, cell)
		}
		t = append(t, line)
	}
	ui.Table(ui.Stdout, t, true)
	ui.Out("(* = in use right now)")
	return nil
}
