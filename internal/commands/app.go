package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"axon/internal/build"
	"axon/internal/check"
	"axon/internal/cli"
	"axon/internal/deploy"
	"axon/internal/descriptor"
	"axon/internal/layout"
	"axon/internal/scaffold"
	"axon/internal/sh"
	"axon/internal/tomledit"
	"axon/internal/ui"
)

func appVerbs() []*cli.Verb {
	dirOpt := []cli.Arg{{Name: "dir", Optional: true}}
	return []*cli.Verb{
		{Name: "app new", Args: []cli.Arg{{Name: "dir"}}, Group: "app", Summary: "scaffold an app with a seed module (Go or Rust server + Svelte UI)",
			Flags: []cli.Flag{{Name: "standalone", Kind: cli.Bool, Usage: "do not look for a cluster to join"},
				{Name: "lang", Kind: cli.String, Default: "go", Usage: "go | rust"},
				{Name: "name", Kind: cli.String, Usage: "app name (default <token>_app)"},
				{Name: "title", Kind: cli.String, Usage: "app title (default: title-cased)"},
				{Name: "module", Kind: cli.String, Usage: "seed module name (default: the token)"},
				{Name: "token", Kind: cli.String, Usage: "seed module token (default: from <dir>)"},
				{Name: "label", Kind: cli.String, Usage: "module label (default \"<Title> (<Lang>)\")"},
				{Name: "rings", Kind: cli.String, Default: "all", Usage: "extra rings: test,docs | all | none"},
				{Name: "project", Kind: cli.Bool, Usage: "scaffold a project (worker) app instead"}},
			Run: runAppNew},
		{Name: "app add module", Args: []cli.Arg{{Name: "name"}}, Group: "app", Summary: "add a module to the app and compose it",
			Flags: []cli.Flag{flagDir, {Name: "lang", Kind: cli.String, Default: "go", Usage: "go | rust"}, {Name: "rings", Kind: cli.String, Default: "all", Usage: "test,docs | all | none"}},
			Run:   runAppAddModule},
		{Name: "app remove module", Args: []cli.Arg{{Name: "name"}}, Group: "app", Summary: "stop composing a module (and with --purge delete it)",
			Flags: []cli.Flag{flagDir, {Name: "purge", Kind: cli.Bool, Usage: "also delete modules/<name> (asks first)"}}, Run: runAppRemoveModule},
		{Name: "app set", Args: []cli.Arg{{Name: "setting", Variadic: true}}, Group: "app", Summary: "set app descriptor keys (build-type=module|project, …); a bare key prints it",
			Flags: []cli.Flag{flagDir}, Run: runAppSet},
		{Name: "app check", Args: dirOpt, Group: "app", Summary: "toolchain table, descriptor validation and app structure (uncomposed modules)",
			Flags: []cli.Flag{flagDir, {Name: "fix", Kind: cli.Bool, Usage: "compose module dirs that exist but are not composed"}, flagJSON}, Run: runAppCheck},
		{Name: "app build", Args: dirOpt, Group: "app", Summary: "build this app's images (uses the cluster that declares it)",
			Flags: []cli.Flag{flagDir, {Name: "rebuild", Kind: cli.Bool, Usage: "ignore caches"}, {Name: "jobs", Kind: cli.Int, Usage: "parallel jobs"}, flagDryRun},
			Run:   runAppBuild},
		{Name: "app run", Args: dirOpt, Group: "app", Summary: "build this app and run it in the foreground against its cluster (Ctrl-C stops it)",
			Flags: []cli.Flag{flagDir, {Name: "rebuild", Kind: cli.Bool, Usage: "ignore caches"}, {Name: "detach", Kind: cli.Bool, Usage: "start in the background"}},
			Run:   runAppRun},
		{Name: "app test", Args: dirOpt, Group: "app", Summary: "run every module's tests (go test / cargo test / npm test)",
			Flags: []cli.Flag{flagDir}, Run: runAppTest},
	}
}

func appDir(c *cli.Ctx) string { return dirArg(c, 0) }

func findApp(c *cli.Ctx, dir string) (*layout.Found, *descriptor.AppFile, error) {
	f, err := layout.FindApp(dir, compat(c))
	if err != nil {
		abs, _ := filepath.Abs(dir)
		return nil, nil, cli.NoContext("no axonfile (or mycelfile) in %s — `%s` runs in an app directory; %s", abs, c.Verb.Name, layout.AppSearchHint(compat(c)))
	}
	af, err := descriptor.LoadAppFile(f.Path)
	return f, af, err
}

func runAppNew(c *cli.Ctx) error {
	dir, _ := filepath.Abs(c.Arg(0))
	l := layout.ForNew(compat(c))
	if _, err := layout.FindApp(dir, layout.ModeAuto); err == nil {
		return fmt.Errorf("%s already holds an app descriptor", dir)
	}
	tok := or(c.String("token"), scaffold.Token(or(c.String("module"), filepath.Base(dir))))
	name := or(c.String("name"), scaffold.Token(filepath.Base(dir))+"_app")
	title := or(c.String("title"), scaffold.Title(filepath.Base(dir)))
	w := &scaffold.Writer{Root: dir}
	if c.Bool("project") {
		if err := scaffold.Project(w, l, name, title); err != nil {
			return err
		}
	} else {
		lang := c.String("lang")
		if lang != "go" && lang != "rust" {
			return cli.Usage("app new: --lang must be go or rust")
		}
		err := scaffold.App(w, l, scaffold.AppData{Name: name, Title: title, Modules: []string{tok}},
			scaffold.ModuleData{Token: tok, Lang: lang, Label: c.String("label"), Title: scaffold.Title(tok)}, scaffold.ParseRings(c.String("rings")))
		if err != nil {
			return err
		}
	}
	if !c.Bool("project") && c.String("lang") == "go" {
		tidy(filepath.Join(dir, "modules", tok, "module", "service"))
	}
	ui.OK("app %q scaffolded → %s (%d files)", name, dir, len(w.Created))
	ui.Out("next:")
	ui.Out("  axon cluster add app --name %s --from %s --host %s   # from your cluster dir", filepath.Base(dir), dir, descriptor.Slug(tok))
	ui.Out("  axon app check %s", c.Arg(0))
	return nil
}

func runAppAddModule(c *cli.Ctx) error {
	f, af, err := findApp(c, or(c.String("dir"), "."))
	if err != nil {
		return err
	}
	if af.BuildType != "app" {
		return fmt.Errorf("%s is build_type=%q; modules belong to an app", f.Path, af.BuildType)
	}
	tok := scaffold.Token(c.Arg(0))
	src := "modules/" + tok + "/module"
	for _, ce := range af.Compose {
		if ce.Source == src {
			return fmt.Errorf("module %q is already composed", tok)
		}
	}
	w := &scaffold.Writer{Root: f.Dir}
	if err := scaffold.Module(w, f.Layout, scaffold.ModuleData{Token: tok, Lang: c.String("lang")}, scaffold.ParseRings(c.String("rings"))); err != nil {
		return err
	}
	b, _ := os.ReadFile(f.Path)
	d := tomledit.Parse(string(b))
	d.AppendBlock(fmt.Sprintf("[[compose]]\nsource = %q", src))
	if err := os.WriteFile(f.Path, []byte(d.String()), 0o644); err != nil {
		return err
	}
	if c.String("lang") == "go" {
		tidy(filepath.Join(f.Dir, "modules", tok, "module", "service"))
	}
	ui.OK("module %q added (%d files) and composed", tok, len(w.Created))
	return nil
}

func runAppRemoveModule(c *cli.Ctx) error {
	f, _, err := findApp(c, or(c.String("dir"), "."))
	if err != nil {
		return err
	}
	tok := scaffold.Token(c.Arg(0))
	b, _ := os.ReadFile(f.Path)
	d := tomledit.Parse(string(b))
	if !d.RemoveArrayElem("compose", "source", "modules/"+tok+"/module") {
		return fmt.Errorf("module %q is not composed", tok)
	}
	if err := os.WriteFile(f.Path, []byte(d.String()), 0o644); err != nil {
		return err
	}
	ui.OK("module %q is no longer composed", tok)
	p := filepath.Join(f.Dir, "modules", tok)
	if c.Bool("purge") {
		if !ui.Confirm("Delete "+p+"?", c.G.Yes) {
			return fmt.Errorf("not deleting %s without confirmation (pass --yes)", p)
		}
		if err := os.RemoveAll(p); err != nil {
			return err
		}
		ui.Sub("deleted %s", p)
	} else {
		ui.Note("its files remain; delete with: rm -rf %s", p)
	}
	return nil
}

func runAppSet(c *cli.Ctx) error {
	f, af, err := findApp(c, or(c.String("dir"), "."))
	if err != nil {
		return err
	}
	if len(c.Args) == 0 {
		return cli.Usage("app set: missing required argument(s): setting")
	}
	b, _ := os.ReadFile(f.Path)
	orig := string(b)
	d := tomledit.Parse(orig)
	for _, kv := range c.Args {
		k, v, hasV := strings.Cut(kv, "=")
		key := strings.ReplaceAll(k, "-", "_")
		table := ""
		if i := strings.LastIndex(key, "."); i > 0 {
			table, key = key[:i], key[i+1:]
		}
		if !hasV {
			val, _ := d.Get(table, key)
			ui.Out("%s", val)
			continue
		}
		if key == "build_type" && v != "app" && v != "module" && v != "project" {
			return cli.Usage("app set: build-type must be app, module or project")
		}
		d.Set(table, key, tomledit.EncodeGuess(v))
	}
	if d.String() == orig {
		return nil
	}
	tmp := f.Path + ".axon-tmp"
	os.WriteFile(tmp, []byte(d.String()), 0o644)
	defer os.Remove(tmp)
	naf, err := descriptor.LoadAppFile(tmp)
	if err != nil {
		return fmt.Errorf("reverted: %w", err)
	}
	if ps := descriptor.ValidateAppFile(naf); descriptor.Blocking(ps) > 0 {
		return fmt.Errorf("reverted: %s", ps[0].Cause)
	}
	_ = af
	if err := os.WriteFile(f.Path, []byte(d.String()), 0o644); err != nil {
		return err
	}
	ui.OK("updated %s", f.Path)
	return nil
}

func runAppCheck(c *cli.Ctx) error {
	tools := check.Toolchain()
	f, af, err := findApp(c, appDir(c))
	if err != nil {
		return err
	}
	var fs []check.Finding
	fs = append(fs, check.FromProblems(descriptor.ValidateAppFile(af))...)
	composed := map[string]bool{}
	for _, ce := range af.Compose {
		composed[strings.TrimSuffix(ce.Source, "/")] = true
		if ce.Image == "" {
			if _, err := os.Stat(filepath.Join(f.Dir, ce.Source)); err != nil {
				fs = append(fs, check.Finding{ID: "APP_MODULE_MISSING", Concern: "conformance", Severity: "blocking", Subject: ce.Source, Cause: "composed but missing on disk"})
			}
		}
	}
	var uncomposed []string
	if ents, err := os.ReadDir(filepath.Join(f.Dir, "modules")); err == nil {
		for _, e := range ents {
			if !e.IsDir() && e.Type()&os.ModeSymlink == 0 {
				continue
			}
			src := "modules/" + e.Name() + "/module"
			if _, err := os.Stat(filepath.Join(f.Dir, src)); err == nil && !composed[src] {
				uncomposed = append(uncomposed, src)
			}
		}
	}
	sort.Strings(uncomposed)
	for _, u := range uncomposed {
		if c.Bool("fix") {
			b, _ := os.ReadFile(f.Path)
			d := tomledit.Parse(string(b))
			d.AppendBlock(fmt.Sprintf("[[compose]]\nsource = %q", u))
			os.WriteFile(f.Path, []byte(d.String()), 0o644)
			ui.Sub("composed %s", u)
			continue
		}
		fs = append(fs, check.Finding{ID: "APP_MODULE_NOT_COMPOSED", Concern: "conformance", Severity: "risk", Subject: u, Cause: "module dir exists but is not composed", Fix: "axon app check --fix"})
	}
	cfg := filepath.Join(layout.ConfigDir("axon"), "config.toml")
	if c.Bool("json") {
		if fs == nil {
			fs = []check.Finding{}
		}
		ui.JSON(map[string]any{"toolchain": tools, "findings": fs})
		if b, _, _ := check.Count(fs); b > 0 {
			return cli.Failed()
		}
		return nil
	}
	rows := [][]string{{"TOOL", "STATUS", "VERSION", "PATH"}}
	for _, t := range tools {
		st := ui.Green("ok")
		if !t.OK {
			st = ui.Yellow("MISSING")
		}
		rows = append(rows, []string{t.Name, st, t.Version, t.Path})
	}
	cst := ui.Green("ok")
	if _, err := os.Stat(cfg); err != nil {
		cst = ui.Dim("absent (optional)")
	}
	rows = append(rows, []string{"config", cst, "", cfg})
	ui.Table(ui.Stdout, rows, true)
	ui.Out("")
	check.Print(fs)
	if b, _, _ := check.Count(fs); b > 0 {
		return cli.Failed()
	}
	return nil
}

// clusterForApp finds the cluster that declares the app at dir: the app's parent
// (workspace) holds the controller as a sibling.
func clusterForApp(c *cli.Ctx, appDir string) (*descriptor.Model, string, error) {
	abs, _ := filepath.Abs(appDir)
	ws := filepath.Dir(abs)
	name := filepath.Base(abs)
	ents, _ := os.ReadDir(ws)
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		f, err := layout.FindCluster(filepath.Join(ws, e.Name()), compat(c))
		if err != nil {
			continue
		}
		m, err := descriptor.Load(f)
		if err != nil {
			continue
		}
		if m.C.App(name) != nil {
			return m, name, nil
		}
	}
	return nil, "", fmt.Errorf("no cluster in %s declares %q — add it with `axon cluster add app --name %s --from %s` from the cluster dir", ws, name, name, abs)
}

func runAppBuild(c *cli.Ctx) error {
	m, name, err := clusterForApp(c, appDir(c))
	if err != nil {
		return err
	}
	res, err := build.Run(m, build.Options{Apps: []string{name}, Rebuild: c.Bool("rebuild"), Jobs: c.Int("jobs"), DryRun: c.Bool("dry-run")})
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

func runAppRun(c *cli.Ctx) error {
	m, name, err := clusterForApp(c, dirArg(c, 0))
	if err != nil {
		return err
	}
	o := deploy.Options{Verb: "app run", Build: true, Rebuild: c.Bool("rebuild"), Apps: []string{name}, Force: c.Bool("force")}
	if c.Verb.Name == "up" {
		if opts, err := deployOptions(c, "up", c.String("env")); err == nil {
			o = opts
			o.Build, o.Apps = true, []string{name}
		}
	}
	s, err := deploy.Open(m, o)
	if err != nil {
		return err
	}
	if err := s.Up(); err != nil {
		return err
	}
	if c.Has("detach") && c.Bool("detach") {
		return nil
	}
	if c.Verb.Name == "up" {
		return nil
	}
	ui.Note("following %s logs — Ctrl-C to stop following (the app keeps running; `axon down` stops it)", name)
	return s.Logs(name, "", true, "20")
}

func runAppTest(c *cli.Ctx) error {
	f, af, err := findApp(c, appDir(c))
	if err != nil {
		return err
	}
	var dirs []string
	for _, ce := range af.Compose {
		if ce.Image != "" {
			continue
		}
		mod := filepath.Join(f.Dir, ce.Source)
		dirs = append(dirs, filepath.Join(mod, "service"), filepath.Join(filepath.Dir(mod), "module_test"), filepath.Join(mod, "ui"))
	}
	ran, failed := 0, 0
	for _, d := range dirs {
		var cmd sh.Cmd
		switch {
		case exists(filepath.Join(d, "go.mod")):
			cmd = sh.Cmd{Name: "go", Args: []string{"test", "./..."}}
		case exists(filepath.Join(d, "Cargo.toml")):
			cmd = sh.Cmd{Name: "cargo", Args: []string{"test", "--quiet"}}
		case exists(filepath.Join(d, "package.json")) && hasNpmTest(filepath.Join(d, "package.json")):
			cmd = sh.Cmd{Name: "npm", Args: []string{"test", "--silent"}}
		default:
			continue
		}
		cmd.Dir, cmd.Timeout, cmd.Stream, cmd.Prefix = d, 20*time.Minute, true, "  │ "
		ran++
		ui.Step("%s: %s", rel2(f.Dir, d), cmd.String())
		if _, err := sh.Run(cmd); err != nil {
			failed++
			ui.Fail("%s", rel2(f.Dir, d))
		} else {
			ui.OK("%s", rel2(f.Dir, d))
		}
	}
	if ran == 0 {
		ui.Note("no test suites found")
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d test suite(s) failed", failed, ran)
	}
	return nil
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func hasNpmTest(pkg string) bool {
	b, _ := os.ReadFile(pkg)
	return strings.Contains(string(b), `"test"`)
}

// tidy resolves a scaffolded Go module's dependencies (the module SDK).
func tidy(dir string) {
	if _, err := sh.Run(sh.Cmd{Name: "go", Args: []string{"mod", "tidy"}, Dir: dir, Timeout: 5 * time.Minute}); err != nil {
		ui.Warn("go mod tidy in %s failed (offline?): run it before building\n%v", dir, err)
		return
	}
	ui.Sub("resolved Go dependencies (module SDK %s)", scaffold.ModuleSDKVersion)
}
