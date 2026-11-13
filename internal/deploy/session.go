package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"axon/internal/build"
	"axon/internal/descriptor"
	"axon/internal/envcfg"
	"axon/internal/gen"
	"axon/internal/layout"
	"axon/internal/sh"
	"axon/internal/ui"
	y "axon/internal/yamlx"
)

// Options configure a lifecycle run.
type Options struct {
	Verb          string
	Env           string
	Target        string // docker | k8s | "" (resolve)
	Context       string // kube context
	Build         bool
	Rebuild       bool
	DryRun        bool
	KeepOnFailure bool
	Force         bool
	Jobs          int
	Timeout       time.Duration
	Apps          []string // limit the build/rollout to these apps
	NoHooks       bool
	Yes           bool
	Pull          bool // pre-pull missing external images
}

// Session is one lifecycle run against one environment.
type Session struct {
	M           *descriptor.Model
	Env         *envcfg.Env
	Ov          *envcfg.Overrides
	O           Options
	Target      string
	KubeContext string
	Namespace   string
	Secrets     map[string]string
	PG          gen.PG
	PGMode      string // compose | docker | in-cluster | external
	PGContainer string
	Tags        map[string]string
	Refs        map[string]string // image repo → registry reference (release deploys)
	DryRun      bool
	started     time.Time
	// pgPassDefaulted: POSTGRES_PASSWORD came from the compat default, not .env
	pgPassDefaulted bool
}

// Open prepares a session: loads the environment and resolves the target.
func Open(m *descriptor.Model, o Options) (*Session, error) {
	if o.Env == "" {
		o.Env = m.C.Deploy.Env
	}
	if o.Env == "" {
		o.Env = "dev"
	}
	if !descriptor.SafeName(o.Env) {
		return nil, fmt.Errorf("environment name %q is not safe ([a-z0-9][a-z0-9.-]*)", o.Env)
	}
	env, err := envcfg.Load(m.Dir, m.C.Name, o.Env)
	if err != nil {
		if o.Env != "dev" {
			return nil, err
		}
		env = envcfg.Default(m.C.Name, o.Env) // a dev env works without a file
	}
	if env.AccountID != "" {
		ui.Debug("environments/%s.toml account_id is ignored (axon has no accounts or tenants)", o.Env)
	}
	if env.Domain != "" {
		m.C.Domain = env.Domain
	}
	if env.HTTPPort > 0 {
		if m.C.Fabric.Traefik == nil {
			m.C.Fabric.Traefik = &descriptor.Traefik{}
		}
		m.C.Fabric.Traefik.HTTP = env.HTTPPort
	}
	ov, err := envcfg.LoadOverrides(m.Dir, o.Env)
	if err != nil {
		return nil, err
	}
	m.C.Hooks = m.C.Hooks.Merge(ov.Hooks)
	s := &Session{M: m, Env: env, Ov: ov, O: o, DryRun: o.DryRun, Namespace: env.Namespace, started: time.Now(), Secrets: map[string]string{}}
	s.KubeContext = firstNonEmpty(o.Context, env.Context, m.C.Deploy.Context)
	s.Target, err = s.resolveTarget()
	if err != nil {
		return nil, err
	}
	s.Tags = loadTags(m.State)
	return s, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// resolveTarget: flag, env file, descriptor default, where the cluster is observed
// running, then k8s when a kube context answers, else docker.
func (s *Session) resolveTarget() (string, error) {
	t := firstNonEmpty(s.O.Target, layout.Env("TARGET"), s.Env.Target, s.M.C.Deploy.Target)
	switch t {
	case "docker", "k8s":
		return t, nil
	case "kubernetes":
		return "k8s", nil
	case "":
	default:
		return "", fmt.Errorf("unknown target %q (docker or k8s)", t)
	}
	if s.composeRunning(descriptor.NormProject(s.M.C.Name)) {
		return "docker", nil
	}
	if s.kubeReachable() && s.k8sHasWorkloads() {
		return "k8s", nil
	}
	if s.Env.Context != "" {
		return "k8s", nil
	}
	return "docker", nil
}

// Input builds the generator input for this session.
func (s *Session) Input() *gen.Input {
	return &gen.Input{M: s.M, Env: s.Env, Ov: s.Ov, Target: s.Target, PG: s.PG, Tags: s.Tags, Refs: s.Refs}
}

func loadTags(stateDir string) map[string]string {
	out := map[string]string{}
	if b, err := os.ReadFile(filepath.Join(stateDir, "images.json")); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// RecordTags merges built image tags into the state, so later deploys and
// `release create` use them.
func RecordTags(stateDir string, tags map[string]string) {
	all := loadTags(stateDir)
	for k, v := range tags {
		all[k] = v
	}
	saveTags(stateDir, all)
}

func saveTags(stateDir string, tags map[string]string) {
	b, _ := json.MarshalIndent(tags, "", "  ")
	_ = os.MkdirAll(stateDir, 0o755)
	_ = os.WriteFile(filepath.Join(stateDir, "images.json"), b, 0o644)
}

// Change is one generated file and whether it differs from disk.
type Change struct {
	Path    string // relative to the controller dir
	Changed bool
	content string
}

// Files renders every generated file (paths relative to the controller dir).
func (s *Session) Files() map[string]string {
	if s.PG.Host == "" {
		s.PG = gen.PG{Host: "postgres", Port: 5432, User: gen.PGUser(s.M), Password: "${POSTGRES_PASSWORD}"}
	}
	saved := s.Target
	s.Target = "docker"
	in := s.Input()
	s.Target = saved
	files := map[string]string{}
	for p, c := range gen.FabricFiles(in) {
		files[filepath.Join(s.M.L.Infra, p)] = c
	}
	for _, a := range s.M.Apps {
		if a.Problem != "" && (a.Decl.Cored() || a.Decl.Image == "") {
			continue
		}
		files[filepath.Join(s.M.L.Infra, gen.AppComposeFile(s.M, a.Decl.Name))] = gen.AppCompose(in, a)
	}
	if s.M.C.Deploy.Products {
		for _, n := range sortedProducts(s.M.C) {
			p := s.M.C.Product[n]
			if p.Image != "" {
				app := p.App
				if app == "" {
					app = n
				}
				files[filepath.Join(s.M.L.Infra, gen.AppComposeFile(s.M, app))] = gen.ProductCompose(in, n, p)
			}
		}
	}
	return files
}

func sortedProducts(c *descriptor.Cluster) []string {
	var ns []string
	for n := range c.Product {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	return ns
}

// Generate writes generated files (only when changed). With check, nothing is
// written and the changed paths are returned.
func (s *Session) Generate(check bool) ([]Change, error) {
	files := s.Files()
	var paths []string
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var out []Change
	for _, p := range paths {
		full := filepath.Join(s.M.Dir, p)
		cur, err := os.ReadFile(full)
		changed := err != nil || string(cur) != files[p]
		out = append(out, Change{Path: p, Changed: changed, content: files[p]})
		if changed && !check && !s.DryRun {
			if _, err := build.WriteIfChanged(full, files[p], 0o644); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// Diff returns a simple line diff for changed files (for --diff).
func Diff(dir string, ch Change) string {
	old, _ := os.ReadFile(filepath.Join(dir, ch.Path))
	return lineDiff(string(old), ch.content)
}

func lineDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	inA := map[string]int{}
	for _, l := range al {
		inA[l]++
	}
	inB := map[string]int{}
	for _, l := range bl {
		inB[l]++
	}
	var out strings.Builder
	for _, l := range al {
		if inB[l] == 0 {
			out.WriteString(ui.Red("- "+l) + "\n")
		} else {
			inB[l]--
		}
	}
	for _, l := range bl {
		if inA[l] == 0 {
			out.WriteString(ui.Green("+ "+l) + "\n")
		} else {
			inA[l]--
		}
	}
	return out.String()
}

// runHooks executes a lifecycle stage's commands in the controller dir.
func (s *Session) runHooks(stage string) error {
	cmds := s.M.C.Hooks.Stage(stage)
	if len(cmds) == 0 || s.O.NoHooks {
		return nil
	}
	env := []string{
		"AXON_CLUSTER=" + s.M.C.Name, "AXON_ENV=" + s.Env.Name, "AXON_TARGET=" + s.Target,
		"AXON_NAMESPACE=" + s.Namespace, "AXON_DOMAIN=" + s.M.C.DomainOrDefault(),
		fmt.Sprintf("AXON_HTTP_PORT=%d", s.M.C.HTTPPort()), "AXON_CONTROLLER_DIR=" + s.M.Dir,
		"AXON_ROOT=" + s.M.Root, "AXON_INFRA_DIR=" + s.M.Infra, "AXON_KUBE_CONTEXT=" + s.KubeContext,
		"AXON_STAGE=" + stage,
	}
	for _, c := range cmds {
		if s.DryRun {
			ui.Sub("would run %s hook: %s", stage, c)
			continue
		}
		ui.Step("hook %s: %s", stage, c)
		start := time.Now()
		if _, err := sh.Run(sh.Cmd{Name: "sh", Args: []string{"-c", c}, Dir: s.M.Dir, Env: env, Stream: true, Prefix: "  │ "}); err != nil {
			return fmt.Errorf("%s hook %q: %w", stage, c, err)
		}
		ui.Debug("hook took %s", time.Since(start).Round(time.Second))
	}
	return nil
}

// reload re-resolves the model (hooks may link app dirs into place).
func (s *Session) reload() error {
	m, err := descriptor.Load(s.M.Found)
	if err != nil {
		return err
	}
	m.C.Hooks = m.C.Hooks.Merge(s.Ov.Hooks)
	s.M = m
	return nil
}

// Up runs the full deploy pipeline.
func (s *Session) Up() (err error) {
	var lock *Lock
	if !s.DryRun {
		lock, err = AcquireLock(s.M.State, s.O.Verb, s.Target, s.Env.Name, s.O.Force)
		if err != nil {
			return err
		}
		defer func() {
			outcome := "ok"
			if err != nil {
				outcome = "failed"
			}
			lock.Release(outcome)
		}()
	}
	where := s.Target
	if s.Target == "k8s" {
		where = fmt.Sprintf("k8s (context %s, namespace %s)", or(s.KubeContext, "current"), s.Namespace)
	}
	verb := "deploying"
	if s.DryRun {
		verb = "plan for"
	}
	ui.Step("%s %s/%s → %s", verb, s.M.C.Name, s.Env.Name, where)
	if s.Env.Tier != "dev" && s.Env.Release != "" && s.Env.Release != "local" && s.O.Build {
		return fmt.Errorf("--build deploys the working tree, which only a dev-tier environment may do (%s is %s, release %s)", s.Env.Name, s.Env.Tier, s.Env.Release)
	}
	if err := s.runHooks("pre_generate"); err != nil {
		return err
	}
	if s.O.Build {
		if err := s.runHooks("pre_build"); err != nil {
			return err
		}
	}
	if err := s.reload(); err != nil {
		return err
	}
	if probs := descriptor.ValidateCluster(s.M); descriptor.Blocking(probs) > 0 {
		for _, p := range probs {
			if p.Severity == "blocking" {
				ui.Fail("%s: %s", p.Subject, p.Cause)
			}
		}
		return fmt.Errorf("the descriptor has %d blocking problem(s); fix them or run `axon cluster check`", descriptor.Blocking(probs))
	}
	if s.Target == "k8s" {
		if err := s.k8sPreflight(); err != nil {
			return err
		}
	} else if err := s.dockerPreflight(); err != nil {
		return err
	}
	if err := s.resolveSecrets(s.readClusterSecret); err != nil {
		return err
	}
	if err := s.preparePostgres(); err != nil {
		return err
	}
	if s.O.Build {
		ui.Step("build")
		res, err := build.Run(s.M, build.Options{Jobs: s.O.Jobs, Rebuild: s.O.Rebuild, Apps: s.O.Apps, DryRun: s.DryRun})
		if err != nil {
			return err
		}
		for k, v := range res.Tags {
			s.Tags[k] = v
		}
		built := 0
		for _, img := range res.Images {
			if img.Built {
				built++
			}
		}
		if !s.DryRun {
			ui.OK("%d image(s): %d built, %d unchanged (%s)", len(res.Images), built, res.Skipped, res.Took.Round(time.Second))
			saveTags(s.M.State, s.Tags)
		}
		if err := s.runHooks("post_build"); err != nil {
			return err
		}
	} else if err := s.checkBuiltImages(); err != nil {
		return err
	}
	if err := s.pullMissing(); err != nil {
		return err
	}
	if s.Target == "k8s" {
		err = s.upK8s()
	} else {
		err = s.upDocker()
	}
	if err != nil {
		return err
	}
	if err := s.runHooks("post_deploy"); err != nil {
		return err
	}
	if s.DryRun {
		ui.OK("plan complete — nothing was changed")
		return nil
	}
	s.printFrontDoors(true)
	ui.OK("deployed %s/%s → %s in %s", s.M.C.Name, s.Env.Name, s.Target, time.Since(s.started).Round(time.Second))
	return nil
}

func or(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// checkBuiltImages fails early when an app image axon builds has never been built.
func (s *Session) checkBuiltImages() error {
	var missing []string
	for _, a := range s.M.Apps {
		if a.Problem != "" {
			continue
		}
		var repos []string
		if a.Decl.Cored() {
			repos = append(repos, descriptor.ServiceImage(s.M.C.Name, a.Decl.Name, "core"))
			for _, mod := range a.Modules {
				if !mod.IsImage() {
					repos = append(repos, descriptor.ServiceImage(s.M.C.Name, a.Decl.Name, mod.Token))
				}
			}
		} else if a.Decl.Image == "" {
			repos = append(repos, descriptor.WorkerImage(s.M.C.Name, a.Decl.Name))
		}
		for _, r := range repos {
			if s.Refs[r] != "" {
				continue // pulled from the registry by the runtime
			}
			ref := s.Input().Image(r)
			if !imageExists(ref) {
				missing = append(missing, ref)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("%d app image(s) are not built yet (%s…); rerun with --build", len(missing), missing[0])
	}
	return nil
}

func imageExists(ref string) bool {
	_, err := sh.Run(sh.Cmd{Name: "docker", Args: []string{"image", "inspect", "--format", "{{.Id}}", ref}, Timeout: 30 * time.Second})
	return err == nil
}

// externalImages lists images the deploy runs but does not build.
func (s *Session) externalImages() []string {
	in := s.Input()
	set := map[string]bool{}
	for _, a := range s.M.Apps {
		if a.Problem != "" && a.Decl.Cored() {
			continue
		}
		for _, mod := range a.Modules {
			if mod.IsImage() {
				set[in.ResolveRef(mod.Image)] = true
			}
		}
		if !a.Decl.Cored() && a.Decl.Image != "" {
			set[in.ResolveRef(a.Decl.Image)] = true
		}
	}
	var out []string
	for k := range set {
		repo := k
		if i := strings.LastIndex(k, ":"); i > strings.LastIndex(k, "/") {
			repo = k[:i]
		}
		if _, built := s.Tags[repo]; built {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pullMissing pre-pulls external images that are not present locally, in
// parallel, and reports every failure at once.
func (s *Session) pullMissing() error {
	var missing []string
	for _, img := range s.externalImages() {
		if !imageExists(img) {
			missing = append(missing, img)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if s.DryRun {
		for _, m := range missing {
			ui.Sub("would pull %s", m)
		}
		return nil
	}
	ui.Step("pull %d missing image(s)", len(missing))
	errs := make([]string, len(missing))
	done := make(chan int)
	for i, img := range missing {
		go func(i int, img string) {
			if _, err := sh.Run(sh.Cmd{Name: "docker", Args: []string{"pull", "-q", img}, Timeout: 20 * time.Minute}); err != nil {
				errs[i] = img
			}
			done <- i
		}(i, img)
	}
	for range missing {
		<-done
	}
	var failed []string
	for _, e := range errs {
		if e != "" {
			failed = append(failed, e)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("cannot pull %d image(s) (not present locally, registry refused): %s\n  hint: `docker login <registry>`, or substitute them under [images] in %s",
			len(failed), strings.Join(failed, ", "), filepath.Join(s.M.L.Infra, descriptor.OverlayName))
	}
	return nil
}

// FrontDoor is a routed endpoint and its probe result.
type FrontDoor struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Status  int    `json:"status"`
	OK      bool   `json:"ok"`
	Modules string `json:"modules,omitempty"` // "serving/total" for cored apps
}

// FrontDoors probes every routed host through the front door.
func (s *Session) FrontDoors() []FrontDoor {
	in := s.Input()
	var out []FrontDoor
	for _, r := range gen.Routes(in, s.M.C.Deploy.Products) {
		path := "/"
		if strings.HasPrefix(r.Upstream, "http://app-") {
			if a := s.appByCore(r.Upstream); a != nil && a.Decl.Cored() {
				path = "/healthz"
			}
		}
		code := probe(s.M.C.HTTPPort(), r.Host, path)
		fd := FrontDoor{Name: r.Name, URL: fmt.Sprintf("http://%s:%d", r.Host, s.M.C.HTTPPort()), Status: code, OK: code > 0 && code < 500}
		if path == "/healthz" && fd.OK {
			serving, total := moduleHealth(s.M.C.HTTPPort(), r.Host)
			if total > 0 {
				fd.Modules = fmt.Sprintf("%d/%d", serving, total)
				fd.OK = serving == total
			}
		}
		out = append(out, fd)
	}
	return out
}

func (s *Session) appByCore(upstream string) *descriptor.AppModel {
	for _, a := range s.M.Apps {
		if strings.HasPrefix(upstream, "http://"+descriptor.CoreName(a.Decl.Name)+":") {
			return a
		}
	}
	return nil
}

func (s *Session) printFrontDoors(wait bool) {
	deadline := time.Now().Add(90 * time.Second)
	var fds []FrontDoor
	for {
		fds = s.FrontDoors()
		allOK := true
		for _, f := range fds {
			allOK = allOK && f.OK
		}
		if allOK || !wait || time.Now().After(deadline) {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if len(fds) == 0 {
		return
	}
	ui.Out("front doors:")
	var rows [][]string
	for _, f := range fds {
		mark := ui.Green("✓")
		if !f.OK {
			mark = ui.Red("✗")
		}
		st := "unreachable"
		if f.Status > 0 {
			st = fmt.Sprint(f.Status)
		}
		if f.Modules != "" {
			st += " · modules " + f.Modules + " serving"
		}
		rows = append(rows, []string{"  " + f.Name, f.URL, mark + " " + st})
	}
	ui.Table(ui.Stdout, rows, false)
}

// writeManifest stores a rendered manifest for inspection (never secrets).
func (s *Session) writeManifest(name string, docs []any) error {
	text, err := y.MarshalAll(docs)
	if err != nil {
		return err
	}
	p := filepath.Join(s.M.Infra, "k8s", "axon", s.Env.Name+"-"+name+".yaml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	_, err = build.WriteIfChanged(p, gen.Header("#", s.M.L.Infra+"/"+s.M.L.File)+text, 0o644)
	return err
}
