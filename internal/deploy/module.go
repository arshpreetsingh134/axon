package deploy

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"axon/internal/build"
	"axon/internal/descriptor"
	"axon/internal/gen"
	"axon/internal/ui"
)

// Target is one thing a module deploy rebuilds and rolls: a module of a cored app,
// or (Token "") a worker app built from its own Dockerfile.
type Target struct {
	App   *descriptor.AppModel
	Token string
}

func (t Target) String() string {
	if t.Token == "" {
		return t.App.Decl.Name
	}
	return t.App.Decl.Name + "/" + t.Token
}

// ResolveModules maps --module specs onto the model. A spec is "<app>/<token>", a
// module token that only one app composes, or the name of a worker app.
func ResolveModules(m *descriptor.Model, specs []string) ([]Target, error) {
	var out []Target
	seen := map[string]bool{}
	add := func(t Target) {
		if !seen[t.String()] {
			seen[t.String()] = true
			out = append(out, t)
		}
	}
	for _, spec := range specs {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		if appName, tok, ok := strings.Cut(spec, "/"); ok {
			a := findApp(m, appName)
			if a == nil {
				return nil, fmt.Errorf("--module %s: no app %q in this cluster (%s)", spec, appName, appNames(m))
			}
			mod, err := buildableModule(a, tok)
			if err != nil {
				return nil, fmt.Errorf("--module %s: %w", spec, err)
			}
			add(Target{App: a, Token: mod.Token})
			continue
		}
		// a bare token: the module of that name, wherever it is unique
		var hits []Target
		for _, a := range m.Apps {
			if !a.Decl.Cored() {
				continue
			}
			for _, mod := range a.Modules {
				if mod.Token == spec {
					hits = append(hits, Target{App: a, Token: spec})
				}
			}
		}
		switch {
		case len(hits) == 1:
			if _, err := buildableModule(hits[0].App, spec); err != nil {
				return nil, fmt.Errorf("--module %s: %w", spec, err)
			}
			add(hits[0])
			continue
		case len(hits) > 1:
			var opts []string
			for _, h := range hits {
				opts = append(opts, h.String())
			}
			return nil, fmt.Errorf("--module %s: several apps compose a module %q; name one: %s", spec, spec, strings.Join(opts, ", "))
		}
		// an app name: only a worker app (its own image) is a target on its own
		if a := findApp(m, spec); a != nil {
			switch {
			case a.Decl.Cored():
				return nil, fmt.Errorf("--module %s is an app with modules (%s); name a module: %s/<token>", spec, moduleTokens(a), spec)
			case a.Decl.Image != "":
				return nil, fmt.Errorf("--module %s runs the prebuilt image %s; there is nothing to build", spec, a.Decl.Image)
			case a.Problem != "":
				return nil, fmt.Errorf("--module %s: %s", spec, a.Problem)
			}
			add(Target{App: a})
			continue
		}
		return nil, fmt.Errorf("--module %s: no module or worker app of that name (modules: %s)", spec, allTokens(m))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--module needs a module token, <app>/<token> or a worker app name")
	}
	return out, nil
}

func findApp(m *descriptor.Model, name string) *descriptor.AppModel {
	for _, a := range m.Apps {
		if a.Decl.Name == name {
			return a
		}
	}
	return nil
}

func buildableModule(a *descriptor.AppModel, tok string) (*descriptor.Module, error) {
	if !a.Decl.Cored() {
		return nil, fmt.Errorf("%s is a worker app, not an app with modules; name it on its own: --module %s", a.Decl.Name, a.Decl.Name)
	}
	if a.Problem != "" {
		return nil, fmt.Errorf("%s: %s", a.Decl.Name, a.Problem)
	}
	for _, mod := range a.Modules {
		if mod.Token != tok {
			continue
		}
		if mod.IsImage() {
			return nil, fmt.Errorf("module %s of %s runs the prebuilt image %s; there is nothing to build", tok, a.Decl.Name, mod.Image)
		}
		if mod.SrcDir == "" {
			return nil, fmt.Errorf("module %s of %s has no source directory", tok, a.Decl.Name)
		}
		return mod, nil
	}
	return nil, fmt.Errorf("%s has no module %q (modules: %s)", a.Decl.Name, tok, moduleTokens(a))
}

func moduleTokens(a *descriptor.AppModel) string {
	var ts []string
	for _, mod := range a.Modules {
		ts = append(ts, mod.Token)
	}
	return strings.Join(ts, ", ")
}

func allTokens(m *descriptor.Model) string {
	var ts []string
	for _, a := range m.Apps {
		if a.Decl.Cored() {
			for _, mod := range a.Modules {
				if !mod.IsImage() {
					ts = append(ts, a.Decl.Name+"/"+mod.Token)
				}
			}
		} else if a.Decl.Image == "" {
			ts = append(ts, a.Decl.Name)
		}
	}
	if len(ts) == 0 {
		return "none built from source"
	}
	sort.Strings(ts)
	return strings.Join(ts, ", ")
}

func appNames(m *descriptor.Model) string {
	var ns []string
	for _, a := range m.Apps {
		ns = append(ns, a.Decl.Name)
	}
	return strings.Join(ns, ", ")
}

// workloads returns, per app, the Kubernetes Deployments and compose services a
// target set touches: each named module's server and its app's core (rolled only
// when its image changed), or the worker itself.
func workloads(targets []Target) (deploys []string, services map[string][]string) {
	services = map[string][]string{}
	seen := map[string]bool{}
	addDeploy := func(n string) {
		if !seen[n] {
			seen[n] = true
			deploys = append(deploys, n)
		}
	}
	for _, t := range targets {
		app := t.App.Decl.Name
		if t.Token == "" {
			addDeploy(descriptor.CoreName(app))
			services[app] = append(services[app], descriptor.Slug(app))
			continue
		}
		addDeploy(descriptor.DNS(t.Token))
		services[app] = append(services[app], t.Token)
	}
	for _, t := range targets {
		if t.Token != "" {
			app := t.App.Decl.Name
			addDeploy(descriptor.CoreName(app))
			if !contains(services[app], "core") {
				services[app] = append(services[app], "core")
			}
		}
	}
	return deploys, services
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// upModules is the fast path behind --module: rebuild the named modules (or worker
// apps) and roll only their workloads into an environment that is already deployed.
// Fabric, databases, Keycloak, the IAM seed and the post_fabric/post_deploy hooks
// are skipped; pre_generate, pre_build and post_build hooks still run (they assemble
// sources).
func (s *Session) upModules() error {
	if s.Env.Tier != "dev" && s.Env.Release != "" && s.Env.Release != "local" {
		return fmt.Errorf("--module builds the working tree, which only a dev-tier environment may do (%s is %s, release %s)", s.Env.Name, s.Env.Tier, s.Env.Release)
	}
	if err := s.runHooks("pre_generate"); err != nil {
		return err
	}
	if err := s.runHooks("pre_build"); err != nil {
		return err
	}
	if err := s.reload(); err != nil {
		return err
	}
	targets, err := ResolveModules(s.M, s.O.Modules)
	if err != nil {
		return err
	}
	var names, only []string
	for _, t := range targets {
		names = append(names, t.String())
		only = append(only, t.String())
	}
	where := s.Target
	if s.Target == "k8s" {
		where = fmt.Sprintf("k8s (context %s, namespace %s)", or(s.KubeContext, "current"), s.Namespace)
	}
	verb := "module deploy"
	if s.DryRun {
		verb = "plan for module deploy"
	}
	ui.Step("%s %s/%s: %s → %s", verb, s.M.C.Name, s.Env.Name, strings.Join(names, ", "), where)
	deploys, services := workloads(targets)

	if !s.DryRun {
		if s.Target == "k8s" {
			if err := s.k8sPreflight(); err != nil {
				return err
			}
		} else if err := s.dockerPreflight(); err != nil {
			return err
		}
		if err := s.requireDeployed(deploys, services); err != nil {
			return err
		}
	}
	if err := s.resolveSecrets(s.readClusterSecret); err != nil {
		return err
	}
	if err := s.preparePostgres(); err != nil {
		return err
	}

	ui.Step("build")
	res, err := build.Run(s.M, build.Options{Jobs: s.O.Jobs, Rebuild: s.O.Rebuild, Only: only, DryRun: s.DryRun})
	if err != nil {
		return err
	}
	changed := map[string]bool{}
	for _, img := range res.Images {
		if s.Tags[img.Repo] != img.Tag {
			changed[img.Repo] = true
		}
	}
	for k, v := range res.Tags {
		s.Tags[k] = v
	}
	if !s.DryRun {
		built := 0
		for _, img := range res.Images {
			if img.Built {
				built++
			}
		}
		ui.OK("%d image(s): %d built, %d unchanged (%s)", len(res.Images), built, res.Skipped, res.Took.Round(time.Second))
		saveTags(s.M.State, s.Tags)
	}
	if err := s.runHooks("post_build"); err != nil {
		return err
	}

	if s.Target == "k8s" {
		err = s.rollK8s(deploys)
	} else {
		err = s.rollDocker(services)
	}
	if err != nil {
		return err
	}
	s.reportSkipped()
	if s.DryRun {
		ui.OK("plan complete — nothing was changed")
		return nil
	}
	ui.OK("deployed %s in %s (%d image(s) changed; every other workload untouched)", strings.Join(names, ", "),
		time.Since(s.started).Round(time.Second), len(changed))
	return nil
}

// requireDeployed refuses a module deploy into an environment that is not up yet.
func (s *Session) requireDeployed(deploys []string, services map[string][]string) error {
	if s.Target == "k8s" {
		args := append([]string{"get", "deployment", "-o", "name"}, deploys...)
		if _, err := s.kubectl("", args...); err != nil {
			return fmt.Errorf("%s/%s is not deployed in namespace %s yet (missing: %s) — run a full `axon up --build` first",
				s.M.C.Name, s.Env.Name, s.Namespace, strings.Join(deploys, ", "))
		}
		return nil
	}
	for app := range services {
		proj := descriptor.NormProject(descriptor.ComposeProject(s.M.C.Name, app))
		if !s.composeRunning(proj) {
			return fmt.Errorf("%s is not running (compose project %s) — run a full `axon up --build` first", app, proj)
		}
	}
	return nil
}

// rollK8s re-applies only the target Deployments and waits for them. A Deployment
// whose pod template did not change (an unchanged core, say) does not restart.
func (s *Session) rollK8s(deploys []string) error {
	if s.DryRun {
		for _, d := range deploys {
			ui.Sub("would apply deployment %s and wait for its rollout", d)
		}
		return nil
	}
	ips, err := s.liveServices()
	if err != nil {
		return err
	}
	in, k := s.k8sInput(ips)
	docs := gen.K8sWorkloads(in, k)
	labels := map[string]string{"app.kubernetes.io/managed-by": "axon", "axon.dev/cluster": descriptor.Slug(s.M.C.Name)}
	pullSecret, err := s.applyPullSecret(labels)
	if err != nil {
		return err
	}
	gen.AddPullSecret(docs, pullSecret)
	want := map[string]bool{}
	for _, d := range deploys {
		want[d] = true
	}
	var pick []any
	found := map[string]bool{}
	for _, d := range docs {
		if kind, name := docName(d); kind == "Deployment" && want[name] {
			pick = append(pick, d)
			found[name] = true
		}
	}
	for _, d := range deploys {
		if !found[d] {
			return fmt.Errorf("deployment %s is not generated for this cluster; check the descriptor", d)
		}
	}
	ui.Step("roll %s", strings.Join(deploys, ", "))
	if err := s.apply(pick, "modules"); err != nil {
		return err
	}
	return s.waitRollouts(deploys, nil, !s.O.KeepOnFailure)
}

// rollDocker recreates only the target compose services (no dependencies).
func (s *Session) rollDocker(services map[string][]string) error {
	apps := make([]string, 0, len(services))
	for a := range services {
		apps = append(apps, a)
	}
	sort.Strings(apps)
	if s.DryRun {
		for _, a := range apps {
			ui.Sub("would recreate %s: %s", a, strings.Join(services[a], ", "))
		}
		return nil
	}
	if _, err := s.Generate(false); err != nil {
		return err
	}
	for _, a := range apps {
		proj := descriptor.NormProject(descriptor.ComposeProject(s.M.C.Name, a))
		ui.Step("roll %s: %s", a, strings.Join(services[a], ", "))
		args := append([]string{"up", "-d", "--no-deps", "--wait", "--wait-timeout", strconv.Itoa(int(s.timeout().Seconds()))}, services[a]...)
		if _, err := s.compose(proj, s.appCompose(a), false, args...); err != nil {
			return s.composeFailure(proj, s.appCompose(a), err)
		}
	}
	return nil
}

// reportSkipped says what a module deploy left out, so a stale step is never a surprise.
func (s *Session) reportSkipped() {
	steps := []string{"fabric", "databases", "Keycloak", "IAM seed"}
	for _, stage := range []string{"post_fabric", "post_deploy"} {
		if n := len(s.M.C.Hooks.Stage(stage)); n > 0 && !s.O.NoHooks {
			steps = append(steps, fmt.Sprintf("%d %s hook(s)", n, stage))
		}
	}
	ui.Sub("skipped (module deploy): %s — run a full `axon up --build` after changing the descriptor, secrets, fabric or hooks", strings.Join(steps, ", "))
}
