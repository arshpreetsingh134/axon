package commands

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"axon/internal/build"
	"axon/internal/cli"
	"axon/internal/deploy"
	"axon/internal/descriptor"
	"axon/internal/gen"
	"axon/internal/release"
	"axon/internal/sh"
	"axon/internal/ui"
)

func releaseVerbs() []*cli.Verb {
	return []*cli.Verb{
		{Name: "release list", Group: "release", Summary: "list release records", Flags: []cli.Flag{flagDir, flagJSON}, Run: runReleaseList},
		{Name: "release show", Args: []cli.Arg{{Name: "version"}}, Group: "release", Summary: "show a release record", Flags: []cli.Flag{flagDir, flagJSON}, Run: runReleaseShow},
		{Name: "release check", Args: []cli.Arg{{Name: "version", Optional: true}}, Group: "release", Summary: "verify release digests (all releases when no version is given)",
			Flags: []cli.Flag{flagDir, flagJSON}, Run: runReleaseCheck},
		{Name: "release create", Group: "release", Summary: "record an immutable release from the last build: image refs, source commits, digest",
			Flags: []cli.Flag{flagDir, {Name: "version", Kind: cli.String, Usage: "version (default: next patch of the series)"},
				{Name: "series", Kind: cli.String, Usage: "series (default release/policy.toml series)"},
				{Name: "build", Kind: cli.Bool, Usage: "build every app first (runs pre_build hooks unless --no-hooks)"},
				{Name: "push", Kind: cli.Bool, Usage: "push every image to the registry and record it by digest (deployable anywhere)"},
				{Name: "registry", Kind: cli.String, Usage: "registry to push to (default [release] registry), e.g. ghcr.io/owner"},
				{Name: "platform", Kind: cli.String, Value: "<os/arch,...>", Usage: "with --build: image platforms (default [release] platforms); several build multi-architecture images and need --push"},
				{Name: "no-hooks", Kind: cli.Bool, Usage: "with --build: skip lifecycle hooks"},
				flagDryRun},
			Run: runReleaseCreate},
		{Name: "release lock", Group: "release", Summary: "pin every member app's source commit in release/sources.lock.toml",
			Flags: []cli.Flag{flagDir, flagCheck}, Run: runReleaseLock},
		{Name: "release checkout", Args: []cli.Arg{{Name: "version", Optional: true}}, Group: "release",
			Summary: "check out every member app at a release's source commits (or the lock file's); refuses dirty trees",
			Flags:   []cli.Flag{flagDir, flagDryRun}, Run: runReleaseCheckout},
	}
}

func runReleaseList(c *cli.Ctx) error {
	m, err := controller(c)
	if err != nil {
		return err
	}
	rs, err := release.List(m.Dir)
	if err != nil {
		return err
	}
	if c.Bool("json") {
		out := []map[string]any{}
		for _, r := range rs {
			out = append(out, r.Doc)
		}
		return ui.JSON(out)
	}
	if len(rs) == 0 {
		ui.Note("no releases yet — `axon release create`")
		return nil
	}
	rows := [][]string{{"VERSION", "CREATED", "BY", "TOOL", "DIGEST"}}
	for _, r := range rs {
		d := r.Str("digest")
		if len(d) > 19 {
			d = d[:19] + "…"
		}
		rows = append(rows, []string{r.Version(), r.Str("created"), r.Str("by"), r.Str("tool_version"), d})
	}
	ui.Table(ui.Stdout, rows, true)
	return nil
}

func loadRelease(m *descriptor.Model, v string) (*release.Record, error) {
	p := filepath.Join(release.Dir(m.Dir), v+".toml")
	if _, err := os.Stat(p); err != nil {
		return nil, fmt.Errorf("no release %q (%s)", v, rel2(m.Dir, p))
	}
	return release.Load(p)
}

func runReleaseShow(c *cli.Ctx) error {
	m, err := controller(c)
	if err != nil {
		return err
	}
	r, err := loadRelease(m, c.Arg(0))
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return ui.JSON(r.Doc)
	}
	b, _ := os.ReadFile(r.Path)
	ui.Raw(string(b))
	return nil
}

func runReleaseCheck(c *cli.Ctx) error {
	m, err := controller(c)
	if err != nil {
		return err
	}
	var rs []*release.Record
	if v := c.Arg(0); v != "" {
		r, err := loadRelease(m, v)
		if err != nil {
			return err
		}
		rs = []*release.Record{r}
	} else if rs, err = release.List(m.Dir); err != nil {
		return err
	}
	type result struct {
		Version  string   `json:"version"`
		Status   string   `json:"status"` // ok | mismatch | unverifiable
		Want     string   `json:"want"`
		Got      string   `json:"got"`
		Producer string   `json:"producer"`
		Unpinned []string `json:"unpinned,omitempty"`
	}
	var results []result
	bad := 0
	for _, r := range rs {
		want, got, prod := release.Verify(r)
		res := result{Version: r.Version(), Want: want, Got: got, Producer: prod}
		for _, t := range []string{"modules", "cores", "fabric", "products", "images"} {
			for k, v := range r.Table(t) {
				if !strings.Contains(v, "@sha256:") && !strings.Contains(v, ":") {
					res.Unpinned = append(res.Unpinned, t+"."+k)
				}
			}
		}
		switch {
		case !release.MadeByAxon(r):
			res.Status = "unverifiable"
		case want == got:
			res.Status = "ok"
		default:
			res.Status = "mismatch"
			bad++
		}
		results = append(results, res)
	}
	if c.Bool("json") {
		ui.JSON(results)
	} else {
		for _, r := range results {
			switch r.Status {
			case "ok":
				ui.Out("%s %s digest %s", ui.Green("✓"), r.Version, r.Want)
			case "mismatch":
				ui.Out("%s %s digest mismatch: recorded %s, computed %s — the record was edited", ui.Red("✗"), r.Version, r.Want, r.Got)
			default:
				ui.Out("%s %s produced by %s: its digest uses that tool's encoding and is not re-verified (structure checked)", ui.Dim("·"), r.Version, r.Producer)
			}
		}
	}
	if bad > 0 {
		return cli.Failed()
	}
	return nil
}

func gitHead(dir string) string {
	out, err := sh.Output("git", "-C", dir, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return out
}

func gitDirty(dir string) bool {
	out, err := sh.Output("git", "-C", dir, "status", "--porcelain")
	return err == nil && strings.TrimSpace(out) != ""
}

func runReleaseCreate(c *cli.Ctx) error {
	m, err := controller(c)
	if err != nil {
		return err
	}
	existing, err := release.List(m.Dir)
	if err != nil {
		return err
	}
	pol := release.LoadPolicy(m.Dir)
	series := or(c.String("series"), pol.Series)
	version := strings.TrimPrefix(c.String("version"), "v")
	if version == "" {
		version = release.Next(series, pol.FirstPatch, existing)
	}
	if !descriptor.SafeName(version) {
		return cli.Usage("release create: version %q is not safe", version)
	}
	p := filepath.Join(release.Dir(m.Dir), version+".toml")
	if _, err := os.Stat(p); err == nil {
		return fmt.Errorf("release %s already exists; records are immutable", version)
	}
	registry := strings.TrimSuffix(or(c.String("registry"), m.C.Release.Registry), "/")
	if c.Bool("push") && registry == "" {
		return cli.Usage("release create: --push needs a registry: set [release] registry = \"ghcr.io/<owner>\" or pass --registry")
	}
	platforms := m.C.Release.Platforms
	if p := c.String("platform"); p != "" {
		platforms = nil
		for _, x := range strings.Split(p, ",") {
			if x = strings.TrimSpace(x); x != "" {
				platforms = append(platforms, x)
			}
		}
	}
	for _, p := range platforms {
		if _, err := build.PlatformArch(p); err != nil {
			return cli.Usage("release create: %v", err)
		}
	}
	multi := len(platforms) > 1
	if multi && (!c.Bool("build") || !c.Bool("push")) {
		return cli.Usage("release create: multi-architecture images (%s) are built and pushed in one step: pass --build --push", strings.Join(platforms, ", "))
	}
	s, err := deploy.Open(m, deploy.Options{Verb: "release create", Target: "docker", NoHooks: c.Bool("no-hooks")})
	if err != nil {
		return err
	}
	images, ids := map[string]any{}, map[string]any{}
	if multi && !c.Bool("dry-run") {
		if err := s.RunHooks("pre_build"); err != nil {
			return err
		}
		if m, err = descriptor.Load(m.Found); err != nil {
			return err
		}
		res, err := build.Run(m, build.Options{Platforms: platforms, Push: registry})
		if err != nil {
			return err
		}
		s.Tags = map[string]string{}
		for _, img := range res.Images {
			if img.Pushed == "" {
				continue
			}
			images[img.Repo] = img.Pushed
			s.Tags[img.Repo] = img.Tag
			verb := "pushed"
			if img.Cached {
				verb = "already in the registry:"
			}
			ui.Sub("%s %s", verb, img.Pushed)
		}
		ui.OK("built %d multi-architecture image(s) for %s", len(images), strings.Join(platforms, ", "))
	} else if c.Bool("build") && !c.Bool("dry-run") {
		if err := s.RunHooks("pre_build"); err != nil {
			return err
		}
		if m, err = descriptor.Load(m.Found); err != nil {
			return err
		}
		res, err := build.Run(m, build.Options{Platforms: platforms})
		if err != nil {
			return err
		}
		deploy.RecordTags(m.State, res.Tags)
		s.Tags = res.Tags
		ui.OK("built %d image(s)", len(res.Images))
	}
	missing := 0
	repos := make([]string, 0, len(s.Tags))
	for repo := range s.Tags {
		if multi {
			break // already pushed by digest
		}
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		ref := repo + ":" + s.Tags[repo]
		id, err := sh.Output("docker", "image", "inspect", "--format", "{{.Id}}", ref)
		if err != nil {
			missing++
			continue
		}
		ids[repo] = id
		images[repo] = ref
		if !c.Bool("push") || c.Bool("dry-run") {
			continue
		}
		pushed, err := pushImage(ref, registry+"/"+repo, s.Tags[repo], version)
		if err != nil {
			return err
		}
		images[repo] = pushed
		ui.Sub("pushed %s", pushed)
	}
	if len(images) == 0 {
		return fmt.Errorf("no built images recorded yet — run `axon up --build`, or `axon release create --build`")
	}
	in := s.Input()
	modules, sources, fabric, cores := map[string]any{}, map[string]any{}, map[string]any{}, map[string]any{}
	for _, a := range m.Apps {
		if a.Problem != "" {
			continue
		}
		if h := gitHead(a.Dir); h != "" {
			sources[a.Decl.Name] = h
		}
		for _, mod := range a.Modules {
			if mod.IsImage() {
				modules[a.Decl.Name+"/"+mod.Token] = in.ResolveRef(mod.Image)
			}
		}
		if a.Decl.Cored() {
			repo := descriptor.ServiceImage(m.C.Name, a.Decl.Name, "core")
			if v, ok := images[repo]; ok {
				cores[a.Decl.Name] = v
			}
		}
	}
	f := m.C.Fabric
	add := func(k, img, def string) {
		fabric[k] = m.SubstImage(or(img, def))
	}
	if f.Postgres != nil {
		add("postgres", f.Postgres.Image, gen.ImgPostgres)
	}
	if f.Temporal != nil {
		add("temporal", f.Temporal.Image, gen.ImgTemporal)
	}
	if f.Keycloak != nil {
		add("keycloak", f.Keycloak.Image, gen.ImgKeycloak)
	}
	add("traefik", "", gen.ImgTraefik)
	if f.Tracing != nil {
		add("tempo", f.Tracing.TempoImage, gen.ImgTempo)
		add("grafana", f.Tracing.GrafanaImage, gen.ImgGrafana)
	}
	if f.Clickhouse != nil {
		add("clickhouse", f.Clickhouse.Image, gen.ImgClickhouse)
	}
	by := os.Getenv("USER")
	if n, err := sh.Output("git", "-C", m.Dir, "config", "user.name"); err == nil && n != "" {
		by = n
	} else if u, err := user.Current(); err == nil {
		by = u.Username
	}
	doc := map[string]any{
		"schema_version": int64(release.SchemaVersion),
		"version":        version,
		"created":        time.Now().UTC().Format(time.RFC3339),
		"by":             by,
		"commit":         gitHead(m.Dir),
		"tool_version":   "axon/" + Version,
		"platforms":      recordPlatforms(platforms),
		"images":         images,
		"image_ids":      ids,
		"modules":        modules,
		"cores":          cores,
		"fabric":         fabric,
		"sources":        sources,
	}
	if m.C.CoreImage != "" {
		doc["core_image"] = m.SubstImage(m.C.CoreImage)
	}
	if registry != "" && c.Bool("push") {
		doc["registry"] = registry
	}
	text := release.Render(doc)
	if c.Bool("dry-run") {
		ui.Raw(text)
		return nil
	}
	var dirty []string
	for _, a := range m.Apps {
		if a.Problem == "" && gitDirty(a.Dir) {
			dirty = append(dirty, a.Decl.Name)
		}
	}
	if len(dirty) > 0 {
		ui.Warn("uncommitted changes in %s — the recorded commits do not fully describe the images", strings.Join(dirty, ", "))
	}
	if missing > 0 {
		ui.Warn("%d recorded build(s) are no longer present locally and were left out", missing)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(text), 0o444); err != nil {
		return err
	}
	ui.OK("release %s → %s (%d images, %d sources)", version, rel2(m.Dir, p), len(images), len(sources))
	ui.Out("%s", release.Digest(doc))
	return nil
}

func recordPlatforms(ps []string) []any {
	if len(ps) == 0 {
		return []any{"linux/" + buildArch()}
	}
	out := make([]any, len(ps))
	for i, p := range ps {
		out[i] = p
	}
	return out
}

func buildArch() string {
	out, err := sh.Output("docker", "info", "--format", "{{.Architecture}}")
	if err != nil || out == "" {
		return "unknown"
	}
	return map[string]string{"aarch64": "arm64", "x86_64": "amd64"}[out]
}

type sourcesLock struct {
	Sources map[string]string `toml:"sources"`
}

func lockPath(m *descriptor.Model) string {
	return filepath.Join(m.Dir, "release", "sources.lock.toml")
}

func currentSources(m *descriptor.Model) map[string]string {
	out := map[string]string{}
	for _, a := range m.Apps {
		if a.Problem == "" {
			if h := gitHead(a.Dir); h != "" {
				out[a.Decl.Name] = h
			}
		}
	}
	return out
}

func runReleaseLock(c *cli.Ctx) error {
	m, err := controller(c)
	if err != nil {
		return err
	}
	cur := currentSources(m)
	var old sourcesLock
	_, _ = toml.DecodeFile(lockPath(m), &old)
	var drift []string
	for k, v := range cur {
		if old.Sources[k] != v {
			drift = append(drift, k)
		}
	}
	for k := range old.Sources {
		if _, ok := cur[k]; !ok {
			drift = append(drift, k)
		}
	}
	sort.Strings(drift)
	if c.Bool("check") {
		if len(drift) == 0 {
			ui.Out("up to date")
			return nil
		}
		for _, d := range drift {
			ui.Out("~ %s %s → %s", d, short(old.Sources[d]), short(cur[d]))
		}
		return cli.Failed()
	}
	var b strings.Builder
	b.WriteString("# Source commits of every member app — written by `axon release lock`.\n[sources]\n")
	var keys []string
	for k := range cur {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %q = %q\n", k, cur[k])
	}
	if err := os.MkdirAll(filepath.Dir(lockPath(m)), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(lockPath(m), []byte(b.String()), 0o644); err != nil {
		return err
	}
	ui.OK("locked %d source(s) → %s", len(cur), rel2(m.Dir, lockPath(m)))
	return nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "(none)"
	}
	return s
}

func runReleaseCheckout(c *cli.Ctx) error {
	m, err := controller(c)
	if err != nil {
		return err
	}
	want := map[string]string{}
	if v := c.Arg(0); v != "" {
		r, err := loadRelease(m, v)
		if err != nil {
			return err
		}
		want = r.Table("sources")
	} else {
		var l sourcesLock
		if _, err := toml.DecodeFile(lockPath(m), &l); err != nil {
			return fmt.Errorf("no %s (run `axon release lock`) and no version given", rel2(m.Dir, lockPath(m)))
		}
		want = l.Sources
	}
	var dirty []string
	for _, a := range m.Apps {
		if _, ok := want[a.Decl.Name]; ok && gitDirty(a.Dir) {
			dirty = append(dirty, a.Decl.Name)
		}
	}
	if len(dirty) > 0 {
		return fmt.Errorf("uncommitted changes in %s; commit or stash them first", strings.Join(dirty, ", "))
	}
	for _, a := range m.Apps {
		commit, ok := want[a.Decl.Name]
		if !ok {
			continue
		}
		if gitHead(a.Dir) == commit {
			ui.Sub("%s already at %s", a.Decl.Name, short(commit))
			continue
		}
		if c.Bool("dry-run") {
			ui.Sub("would check out %s at %s", a.Decl.Name, short(commit))
			continue
		}
		if _, err := sh.Run(sh.Cmd{Name: "git", Args: []string{"-C", a.Dir, "checkout", "--quiet", commit}}); err != nil {
			return fmt.Errorf("%s: %w", a.Decl.Name, err)
		}
		ui.OK("%s → %s", a.Decl.Name, short(commit))
	}
	return nil
}

// pushImage tags ref as <target>:<tag> and <target>:<version>, pushes both and
// returns the pullable digest reference <target>@sha256:….
func pushImage(ref, target, tag, version string) (string, error) {
	var digest string
	for _, t := range []string{tag, version} {
		dst := target + ":" + t
		if _, err := sh.Run(sh.Cmd{Name: "docker", Args: []string{"tag", ref, dst}}); err != nil {
			return "", err
		}
		r, err := sh.Run(sh.Cmd{Name: "docker", Args: []string{"push", dst}, Timeout: 30 * time.Minute})
		if err != nil {
			return "", fmt.Errorf("push %s (logged in? `docker login %s`): %w", dst, strings.SplitN(target, "/", 2)[0], err)
		}
		if m := digestRE.FindStringSubmatch(r.Stdout); m != nil {
			digest = m[1]
		}
	}
	if digest == "" {
		out, err := sh.Output("docker", "image", "inspect", "--format", "{{range .RepoDigests}}{{println .}}{{end}}", target+":"+tag)
		if err == nil {
			for _, l := range strings.Fields(out) {
				if strings.HasPrefix(l, target+"@") {
					return l, nil
				}
			}
		}
		return "", fmt.Errorf("pushed %s but could not read its digest", target)
	}
	return target + "@" + digest, nil
}

var digestRE = regexp.MustCompile(`digest: (sha256:[0-9a-f]{64})`)
