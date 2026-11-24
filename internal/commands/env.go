package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"axon/internal/check"
	"axon/internal/cli"
	"axon/internal/descriptor"
	"axon/internal/dotenv"
	"axon/internal/envcfg"
	"axon/internal/gen"
	"axon/internal/journey"
	"axon/internal/layout"
	"axon/internal/release"
	"axon/internal/secrets"
	"axon/internal/ui"
)

var deployFlags = []cli.Flag{
	flagDir,
	{Name: "build", Kind: cli.Bool, Usage: "build changed images from the working tree first (dev tier only)"},
	{Name: "rebuild", Kind: cli.Bool, Usage: "with --build: ignore build caches and rebuild everything"},
	{Name: "target", Kind: cli.String, Value: "docker|k8s", Usage: "where to deploy (default: env file, then where the cluster runs)"},
	{Name: "context", Kind: cli.String, Value: "<kube-context>", Usage: "kube context for the k8s target"},
	{Name: "app", Kind: cli.Multi, Value: "<name>", Usage: "limit the build and rollout to this app (repeatable)"},
	{Name: "module", Kind: cli.Multi, Value: "<[app/]token>", Usage: "fast path: rebuild and roll only this module (or worker app) in an environment that is already up; skips fabric, databases, Keycloak, IAM seed and post-deploy hooks (repeatable)"},
	{Name: "jobs", Kind: cli.Int, Usage: "parallel build/rollout jobs (default: CPUs/2, 2–6)"},
	{Name: "timeout", Kind: cli.String, Value: "<duration>", Usage: "per-step health/rollout timeout, e.g. 15m", Default: "10m"},
	{Name: "keep-on-failure", Kind: cli.Bool, Usage: "leave failed workloads in place instead of rolling them back"},
	{Name: "no-hooks", Kind: cli.Bool, Usage: "skip lifecycle hooks"},
	flagDryRun, flagForce,
	{Name: "verbose", Kind: cli.Bool, Hidden: true},
}

func deployOptions(c *cli.Ctx, verb, env string) (deployOpts, error) {
	o := deployOpts{Verb: verb, Env: env}
	o.Target = c.String("target")
	o.Context = c.String("context")
	o.Build, o.Rebuild = c.Bool("build"), c.Bool("rebuild")
	o.DryRun, o.Force, o.KeepOnFailure = c.Bool("dry-run"), c.Bool("force"), c.Bool("keep-on-failure")
	o.NoHooks, o.Yes = c.Bool("no-hooks"), c.G.Yes
	o.Jobs = c.Int("jobs")
	o.Apps = c.Strings("app")
	for _, m := range c.Strings("module") {
		for _, part := range strings.Split(m, ",") {
			if part = strings.TrimSpace(part); part != "" {
				o.Modules = append(o.Modules, part)
			}
		}
	}
	if len(o.Modules) > 0 {
		if len(o.Apps) > 0 {
			return o, cli.Usage("%s: --module and --app cannot be combined (--module already names the app: <app>/<token>)", c.Verb.Name)
		}
		o.Build = true // a module deploy always builds what it rolls
	}
	if t := c.String("timeout"); t != "" {
		d, err := time.ParseDuration(t)
		if err != nil {
			return o, cli.Usage("%s: --timeout wants a duration like 10m, got %q", c.Verb.Name, t)
		}
		o.Timeout = d
	}
	if c.Bool("verbose") {
		ui.Verbose = true
	}
	if o.Rebuild {
		o.Build = true
	}
	return o, nil
}

func envVerbs() []*cli.Verb {
	envArg := []cli.Arg{{Name: "env"}}
	return []*cli.Verb{
		{Name: "env new", Args: envArg, Group: "env", Summary: "create environments/<env>.toml",
			Flags: []cli.Flag{flagDir, {Name: "tier", Kind: cli.String, Default: "dev", Usage: "dev | qa | uat | prod"},
				{Name: "namespace", Kind: cli.String, Usage: "Kubernetes namespace (default <cluster>-<env>)"},
				{Name: "context", Kind: cli.String, Usage: "kube context"},
				{Name: "release", Kind: cli.String, Usage: "release to deploy (dev defaults to \"local\")"},
				{Name: "postgres", Kind: cli.String, Usage: "k8s only: in-cluster | docker | external:<host>:<port>"},
				{Name: "http-port", Kind: cli.Int, Usage: "front-door port override"},
				{Name: "target", Kind: cli.String, Usage: "docker | k8s"}, flagCheck},
			Run: runEnvNew},
		{Name: "env list", Group: "env", Summary: "list environments", Flags: []cli.Flag{flagDir, flagJSON}, Run: runEnvList},
		{Name: "env show", Args: envArg, Group: "env", Summary: "show an environment (with defaults and overrides)", Flags: []cli.Flag{flagDir, flagJSON}, Run: runEnvShow},
		{Name: "env set", Args: []cli.Arg{{Name: "env"}, {Name: "key=value", Variadic: true}}, Group: "env",
			Summary: "set env keys (domain, release, target, context, postgres, tier, replicas, gate.<level>, app.<app>.<KEY>, image.<ref>, …); a bare key prints it",
			Flags:   []cli.Flag{flagDir, flagCheck}, Run: runEnvSet},
		{Name: "env unset", Args: []cli.Arg{{Name: "env"}, {Name: "key", Variadic: true}}, Group: "env", Summary: "remove env keys",
			Flags: []cli.Flag{flagDir, flagCheck}, Run: runEnvUnset},
		{Name: "env check", Args: envArg, Group: "env", Summary: "validate an environment before deploying: env file, overrides, release, every ${VAR} it needs",
			Help: "Validate an environment offline. For an official environment of a pipeline-released cluster (github/workflow.toml " +
				"and a non-dev tier), every [app.env] value that is exactly \"${NAME}\" must be a key of the environment's " +
				"pre-provisioned app env secret, the rule the deploy pipeline enforces. The keys are compared with the " +
				"environment's key inventory, which holds names only and lives in axon's local state (see `axon env keys`); " +
				"--live refreshes it first with your own kube context.",
			Flags: []cli.Flag{flagDir, flagJSON, {Name: "live", Kind: cli.Bool, Usage: "official environments: refresh the key inventory from the cluster first (key names only)"},
				{Name: "context", Kind: cli.String, Value: "<kube-context>", Usage: "with --live: your kube context for the cluster (default: the env's context)"}},
			Run: runEnvCheck},
		{Name: "env keys", Args: envArg, Group: "env", Summary: "show or update an official environment's key inventory (secret key names only, kept in axon's local state)",
			Help: "The key inventory lists the key NAMES an official environment's pre-provisioned app env secret holds, never their " +
				"values. It is kept under axon's user config directory, never in a repository, and `axon env check` compares the " +
				"descriptor's required keys with it. --live reads the names from the cluster with your own kube context " +
				"(kubectl get secret, printing names only); --add and --remove edit it by hand.",
			Flags: []cli.Flag{flagDir, flagJSON, {Name: "live", Kind: cli.Bool, Usage: "read the key names from the cluster"},
				{Name: "context", Kind: cli.String, Value: "<kube-context>", Usage: "with --live: your kube context for the cluster (default: the env's context)"},
				{Name: "add", Kind: cli.Multi, Value: "<NAME,…>", Usage: "add key names (comma list, repeatable)"},
				{Name: "remove", Kind: cli.Multi, Value: "<NAME,…>", Usage: "remove key names (comma list, repeatable)"}},
			Run: runEnvKeys},
		{Name: "env diagnose", Args: envArg, Group: "env", Summary: "diagnose a deployed environment across toolchain, conformance, function, security, business and devx",
			Flags: []cli.Flag{flagDir, flagJSON, {Name: "concern", Kind: cli.Multi, Value: "<a,b>", Usage: "limit to concerns (comma list, repeatable)"},
				{Name: "target", Kind: cli.String, Usage: "docker | k8s"}, {Name: "context", Kind: cli.String, Usage: "kube context"}},
			Run: runEnvDiagnose},
		{Name: "env deploy", Args: []cli.Arg{{Name: "env"}, {Name: "release", Optional: true}}, Group: "env",
			Summary: "deploy an environment: generate, (build), fabric, databases, Keycloak, apps, seed, verify",
			Help: "Deploy an environment end to end. With a <release> (a version in releases/), deploy exactly the images it " +
				"records: images recorded by registry digest are pulled from the registry. On Kubernetes, setting " +
				"AXON_PULL_REGISTRY, AXON_PULL_USERNAME and AXON_PULL_PASSWORD creates an image pull secret that every " +
				"workload uses (the generated deploy pipeline sets them). ${VAR} values come from the axon secret store, " +
				"then .env, then the process environment.",
			Flags: deployFlags, Run: runEnvDeploy},
		{Name: "env plan", Args: envArg, Group: "env", Summary: "show what `env deploy` would do (same as --dry-run)",
			Flags: deployFlags, Run: func(c *cli.Ctx) error { return deployLike(c, "env plan", c.Arg(0), true) }},
		{Name: "env undeploy", Args: envArg, Group: "env", Summary: "tear an environment down (data volumes are kept unless --volumes)",
			Flags: []cli.Flag{flagDir, {Name: "volumes", Kind: cli.Bool, Usage: "also delete data volumes (asks for confirmation)"},
				{Name: "target", Kind: cli.String, Usage: "docker | k8s"}, {Name: "context", Kind: cli.String, Usage: "kube context"},
				flagDryRun, flagForce, {Name: "no-hooks", Kind: cli.Bool, Usage: "skip lifecycle hooks"}, {Name: "verbose", Kind: cli.Bool, Hidden: true}},
			Run: runEnvUndeploy},
		{Name: "env status", Args: envArg, Group: "env", Summary: "workloads, health verdict and front-door URLs",
			Flags: []cli.Flag{flagDir, flagJSON, {Name: "target", Kind: cli.String, Usage: "docker | k8s"}, {Name: "context", Kind: cli.String, Usage: "kube context"},
				{Name: "watch", Kind: cli.Bool, Usage: "refresh every 3s until interrupted"}},
			Run: runEnvStatus},
		{Name: "env logs", Args: []cli.Arg{{Name: "env"}, {Name: "app"}}, Group: "env", Summary: "show an app's logs (core by default; --service for a module; app \"fabric\" for the fabric)",
			Flags: []cli.Flag{flagDir, {Name: "follow", Short: "f", Kind: cli.Bool, Usage: "stream new lines"},
				{Name: "tail", Kind: cli.String, Default: "100", Usage: "lines to show, or \"all\""},
				{Name: "service", Kind: cli.String, Usage: "a module token or fabric service"},
				{Name: "target", Kind: cli.String, Usage: "docker | k8s"}, {Name: "context", Kind: cli.String, Usage: "kube context"}},
			Run: runEnvLogs},
		{Name: "env restart", Args: []cli.Arg{{Name: "env"}, {Name: "app"}}, Group: "env", Summary: "restart an app's core and modules (docker or k8s)",
			Flags: []cli.Flag{flagDir, {Name: "target", Kind: cli.String, Usage: "docker | k8s"}, {Name: "context", Kind: cli.String, Usage: "kube context"}, {Name: "verbose", Kind: cli.Bool, Hidden: true}},
			Run:   runEnvRestart},
		{Name: "env test", Args: envArg, Group: "env", Summary: "run the probe gate (/api/system) and the journeys against a deployed environment",
			Flags: []cli.Flag{flagDir, flagJSON, {Name: "journey", Kind: cli.Multi, Usage: "run only this journey (file name without .toml)"},
				{Name: "skip-probe", Kind: cli.Bool, Usage: "skip the /api/system probe gate"},
				{Name: "list", Kind: cli.Bool, Usage: "list journeys (offline)"}, {Name: "lint", Kind: cli.Bool, Usage: "validate journeys (offline)"}},
			Run: runEnvTest},
		{Name: "env iam-keys", Args: envArg, Group: "env", Summary: "show the cluster IAM signing key (location, mode, fingerprint, gitignore)",
			Flags: []cli.Flag{flagDir, flagCheck}, Run: runIAMKeys},
		{Name: "env rotate-iam-key", Args: envArg, Group: "env", Summary: "generate a new IAM signing key (the old one is kept as .prev); redeploy to apply",
			Flags: []cli.Flag{flagDir, flagForce, {Name: "finish", Kind: cli.Bool, Usage: "delete the previous key after the redeploy"},
				{Name: "reset", Kind: cli.Bool, Usage: "restore the previous key"}}, Run: runRotateIAMKey},
	}
}

func upVerb() *cli.Verb {
	flags := append([]cli.Flag{{Name: "env", Kind: cli.String, Value: "<env>", Usage: "environment (default [deploy].env, else dev)"}}, deployFlags...)
	return &cli.Verb{Name: "up", Group: "general", Args: []cli.Arg{{Name: "dir", Optional: true}},
		Summary: "one command to run everything: in a cluster dir, deploy the dev environment (add --build to build first); in an app dir, run the app",
		Help: "In a cluster controller directory (or below it), `axon up` deploys an environment — dev unless --env or [deploy].env say otherwise — " +
			"running every step: lifecycle hooks, generate, build (with --build), fabric, databases, Keycloak, apps, IAM seed and a front-door check. " +
			"With --module <[app/]token> (repeatable, or a worker app name) it takes the fast path instead: it rebuilds only those modules " +
			"(and their app's core, when a UI changed) and rolls only their workloads into the running environment, skipping fabric, databases, " +
			"Keycloak, the IAM seed and the post_fabric/post_deploy hooks; pre_build hooks still run. " +
			"In an app directory it builds and runs that app against its cluster (`axon app run`).",
		Flags: flags,
		Run: func(c *cli.Ctx) error {
			dir := dirArg(c, 0)
			if _, err := layout.FindClusterUpward(dir, compat(c)); err == nil {
				return deployLike(c, "up", c.String("env"), false)
			}
			if _, err := layout.FindApp(dir, compat(c)); err == nil {
				return runAppRun(c)
			}
			abs, _ := filepath.Abs(dir)
			return cli.NoContext("no axonfile (or mycelfile) in %s — `up` runs in a cluster or app directory", abs)
		}}
}

func downVerb() *cli.Verb {
	return &cli.Verb{Name: "down", Group: "general", Args: []cli.Arg{{Name: "dir", Optional: true}},
		Summary: "tear down the dev environment (or --env); data volumes are kept unless --volumes",
		Flags: []cli.Flag{{Name: "env", Kind: cli.String, Usage: "environment (default [deploy].env, else dev)"}, flagDir,
			{Name: "volumes", Kind: cli.Bool, Usage: "also delete data volumes (asks for confirmation)"},
			{Name: "target", Kind: cli.String, Usage: "docker | k8s"}, {Name: "context", Kind: cli.String, Usage: "kube context"},
			flagDryRun, flagForce, {Name: "no-hooks", Kind: cli.Bool, Usage: "skip lifecycle hooks"}},
		Run: func(c *cli.Ctx) error { return undeploy(c, c.String("env"), dirArg(c, 0)) }}
}

type deployOpts = deployOptionsT

func deployLike(c *cli.Ctx, verb, env string, plan bool) error {
	dir := c.String("dir")
	if verb == "up" {
		dir = dirArg(c, 0)
	}
	if dir == "" {
		dir = "."
	}
	m, err := loadCluster(c, dir)
	if err != nil {
		return err
	}
	o, err := deployOptions(c, verb, env)
	if err != nil {
		return err
	}
	if plan {
		o.DryRun = true
	}
	s, err := openSession(m, o)
	if err != nil {
		return err
	}
	if rel := c.Arg(1); rel != "" && verb == "env deploy" {
		if len(o.Modules) > 0 {
			return cli.Usage("env deploy: --module builds the working tree; it cannot be combined with a release (%s)", rel)
		}
		if err := applyRelease(s, rel); err != nil {
			return err
		}
	}
	return s.Up()
}

func runEnvDeploy(c *cli.Ctx) error { return deployLike(c, "env deploy", c.Arg(0), false) }

func undeploy(c *cli.Ctx, env, dir string) error {
	m, err := loadCluster(c, dir)
	if err != nil {
		return err
	}
	o := deployOpts{Verb: c.Verb.Name, Env: env, Target: c.String("target"), Context: c.String("context"),
		DryRun: c.Bool("dry-run"), Force: c.Bool("force"), NoHooks: c.Bool("no-hooks"), Yes: c.G.Yes}
	s, err := openSession(m, o)
	if err != nil {
		return err
	}
	vol := c.Bool("volumes")
	if vol && !o.DryRun && !o.Yes {
		if !ui.Confirm(fmt.Sprintf("Delete ALL data volumes of %s/%s?", m.C.Name, s.Env.Name), false) {
			return fmt.Errorf("--volumes deletes data: confirm in a terminal or pass --yes")
		}
		s.O.Yes = true
	}
	return s.Down(vol)
}

func runEnvUndeploy(c *cli.Ctx) error { return undeploy(c, c.Arg(0), or(c.String("dir"), ".")) }

func or(a, b string) string {
	if a == "" {
		return b
	}
	return a
}

func openEnvSession(c *cli.Ctx, env string) (*sessionT, error) {
	m, err := loadCluster(c, or(c.String("dir"), "."))
	if err != nil {
		return nil, err
	}
	o := deployOpts{Verb: c.Verb.Name, Env: env, Target: c.String("target"), Context: c.String("context")}
	s, err := openSession(m, o)
	if err != nil {
		return nil, err
	}
	if s.Target == "k8s" {
		if err := s.K8sPreflightPublic(); err != nil {
			return nil, err
		}
	}
	if err := s.PreparePublic(); err != nil {
		return nil, err
	}
	return s, nil
}

func runEnvStatus(c *cli.Ctx) error {
	s, err := openEnvSession(c, c.Arg(0))
	if err != nil {
		return err
	}
	for {
		st, err := s.Status()
		if err != nil {
			return err
		}
		if c.Bool("json") {
			return ui.JSON(st)
		}
		if c.Bool("watch") {
			ui.Raw("\x1b[H\x1b[2J")
		}
		printStatus(st)
		if !c.Bool("watch") {
			if strings.HasPrefix(st.Verdict, "degraded") {
				return cli.Failed()
			}
			return nil
		}
		time.Sleep(3 * time.Second)
	}
}

func runEnvLogs(c *cli.Ctx) error {
	s, err := openEnvSession(c, c.Arg(0))
	if err != nil {
		return err
	}
	tail := c.String("tail")
	if tail == "all" {
		tail = "-1"
		if s.Target == "docker" {
			tail = "all"
		}
	}
	return s.Logs(c.Arg(1), c.String("service"), c.Bool("follow"), tail)
}

func runEnvRestart(c *cli.Ctx) error {
	s, err := openEnvSession(c, c.Arg(0))
	if err != nil {
		return err
	}
	return s.Restart(c.Arg(1))
}

func controller(c *cli.Ctx) (*descriptor.Model, error) {
	return loadCluster(c, or(c.String("dir"), "."))
}

func runEnvNew(c *cli.Ctx) error {
	m, err := controller(c)
	if err != nil {
		return err
	}
	name := c.Arg(0)
	if !descriptor.SafeName(name) {
		return cli.Usage("env new: name %q is not safe ([a-z0-9][a-z0-9.-]*)", name)
	}
	p := envcfg.PathFor(m.Dir, name)
	if _, err := os.Stat(p); err == nil {
		return fmt.Errorf("environment %q already exists (%s)", name, p)
	}
	tier := c.String("tier")
	if tier != "dev" && tier != "qa" && tier != "uat" && tier != "prod" {
		return cli.Usage("env new: --tier must be dev, qa, uat or prod")
	}
	rel := c.String("release")
	if rel == "" && tier == "dev" {
		rel = "local"
	}
	e := &envcfg.Env{Name: name, Tier: tier, Namespace: or(c.String("namespace"), m.C.Name+"-"+name), Release: rel,
		Context: c.String("context"), Postgres: c.String("postgres"), HTTPPort: c.Int("http-port"), Target: c.String("target")}
	if c.Bool("check") {
		ui.Out("would write %s", p)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(envcfg.Render(e)), 0o644); err != nil {
		return err
	}
	ui.OK("environment %q → %s", name, rel2(m.Dir, p))
	return nil
}

func rel2(base, p string) string {
	if r, err := filepath.Rel(base, p); err == nil {
		return r
	}
	return p
}

func runEnvList(c *cli.Ctx) error {
	m, err := controller(c)
	if err != nil {
		return err
	}
	names, err := envcfg.List(m.Dir)
	if err != nil {
		return err
	}
	var envs []*envcfg.Env
	for _, n := range names {
		e, err := envcfg.Load(m.Dir, m.C.Name, n)
		if err != nil {
			return err
		}
		envs = append(envs, e)
	}
	if c.Bool("json") {
		if envs == nil {
			envs = []*envcfg.Env{}
		}
		return ui.JSON(envs)
	}
	if len(envs) == 0 {
		ui.Note("no environments yet — `axon env new dev`")
		return nil
	}
	rows := [][]string{{"ENV", "TIER", "TARGET", "NAMESPACE", "RELEASE"}}
	for _, e := range envs {
		rows = append(rows, []string{e.Name, e.Tier, or(e.Target, "auto"), e.Namespace, e.Release})
	}
	ui.Table(ui.Stdout, rows, true)
	return nil
}

func loadEnv(c *cli.Ctx, m *descriptor.Model, name string) (*envcfg.Env, error) {
	e, err := envcfg.Load(m.Dir, m.C.Name, name)
	if err != nil {
		return nil, err
	}
	return e, nil
}

func runEnvShow(c *cli.Ctx) error {
	m, err := controller(c)
	if err != nil {
		return err
	}
	e, err := loadEnv(c, m, c.Arg(0))
	if err != nil {
		return err
	}
	ov, err := envcfg.LoadOverrides(m.Dir, e.Name)
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return ui.JSON(map[string]any{"env": e, "overrides": ov})
	}
	rows := [][]string{
		{"name", e.Name}, {"tier", e.Tier}, {"namespace", e.Namespace}, {"release", e.Release},
		{"target", or(e.Target, "auto (where it runs, else docker)")}, {"context", or(e.Context, "current")},
		{"postgres", or(e.Postgres, "auto")}, {"file", rel2(m.Dir, e.Path)},
	}
	if e.AccountID != "" {
		rows = append(rows, []string{"account_id", "ignored (axon has no accounts)"})
	}
	ui.Table(ui.Stdout, rows, false)
	if len(ov.AppEnv) > 0 {
		ui.Out("\noverrides:")
		var apps []string
		for a := range ov.AppEnv {
			apps = append(apps, a)
		}
		sort.Strings(apps)
		for _, a := range apps {
			for _, k := range dotenv.Keys(ov.AppEnv[a]) {
				ui.Out("  app.%s.%s = %s", a, k, ov.AppEnv[a][k])
			}
		}
	}
	for k, v := range ov.Images {
		ui.Out("  image %s → %s", k, v)
	}
	return nil
}

func runEnvSet(c *cli.Ctx) error {
	m, err := controller(c)
	if err != nil {
		return err
	}
	e, err := loadEnv(c, m, c.Arg(0))
	if err != nil {
		return err
	}
	if len(c.Args) < 2 {
		return cli.Usage("env set: missing required argument(s): key=value")
	}
	for _, kv := range c.Args[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			raw, _ := os.ReadFile(e.Path)
			val := lookupEnvKey(string(raw), k)
			ui.Out("%s", val)
			continue
		}
		if c.Bool("check") {
			ui.Out("would set %s = %s", k, v)
			continue
		}
		f, err := envcfg.SetKey(m.Dir, e, k, v)
		if err != nil {
			return err
		}
		ui.OK("%s: %s = %s", rel2(m.Dir, f), k, v)
	}
	return nil
}

func lookupEnvKey(text, key string) string {
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, key) {
			if k, v, ok := strings.Cut(t, "="); ok && strings.TrimSpace(k) == strings.ReplaceAll(key, "-", "_") || ok && strings.TrimSpace(k) == key {
				v = strings.TrimSpace(v)
				if i := strings.Index(v, " #"); i > 0 {
					v = strings.TrimSpace(v[:i])
				}
				return strings.Trim(v, `"`)
			}
		}
	}
	return ""
}

func runEnvUnset(c *cli.Ctx) error {
	m, err := controller(c)
	if err != nil {
		return err
	}
	e, err := loadEnv(c, m, c.Arg(0))
	if err != nil {
		return err
	}
	if len(c.Args) < 2 {
		return cli.Usage("env unset: missing required argument(s): key")
	}
	for _, k := range c.Args[1:] {
		if c.Bool("check") {
			ui.Out("would unset %s", k)
			continue
		}
		f, err := envcfg.UnsetKey(m.Dir, e, k)
		if err != nil {
			return err
		}
		ui.OK("%s: unset %s", rel2(m.Dir, f), k)
	}
	return nil
}

// envFindings validates an environment offline.
func envFindings(m *descriptor.Model, name string) ([]check.Finding, error) {
	var fs []check.Finding
	add := func(id, sev, subj, cause, fix string) {
		fs = append(fs, check.Finding{ID: id, Concern: "conformance", Severity: sev, Subject: subj, Cause: cause, Fix: fix})
	}
	e, err := envcfg.Load(m.Dir, m.C.Name, name)
	if err != nil {
		if name != "dev" {
			return nil, err
		}
		e = envcfg.Default(m.C.Name, name)
		add("ENV_FILE_MISSING", "note", "environments/dev.toml", "no file; defaults apply", "axon env new dev")
	}
	ov, err := envcfg.LoadOverrides(m.Dir, name)
	if err != nil {
		return nil, err
	}
	fs = append(fs, check.FromProblems(descriptor.ValidateCluster(m))...)
	for app := range ov.AppEnv {
		if m.C.App(app) == nil {
			add("CONFORMANCE_ENV_UNKNOWN_SERVICE", "risk", "overrides", fmt.Sprintf("[app_env.%q] names no [[app]]", app), "fix the app name or remove the block")
		}
	}
	if e.Tier != "dev" && (e.Release == "" || e.Release == "local") {
		add("ENV_RELEASE_REQUIRED", "blocking", "environments/"+name+".toml", "a "+e.Tier+" environment must pin a recorded release", "axon env set "+name+" release=<version>")
	} else if e.Release != "" && e.Release != "local" {
		if _, err := os.Stat(filepath.Join(release.Dir(m.Dir), e.Release+".toml")); err != nil {
			add("ENV_RELEASE_MISSING", "blocking", "release "+e.Release, "releases/"+e.Release+".toml does not exist", "axon release create --version "+e.Release)
		}
	}
	if e.AccountID != "" {
		add("ENV_ACCOUNT_IGNORED", "note", "environments/"+name+".toml", "account_id is ignored: axon has no accounts or tenants", "")
	}
	if m.C.CoreImage == "" {
		for _, a := range m.Apps {
			if a.Decl.Cored() {
				add("CONFORMANCE_CORE_IMAGE_MISSING", "blocking", "core_image", "cored apps are assembled FROM core_image, which is not set", "axon cluster set core_image=<ref>")
				break
			}
		}
	}
	// an official environment keeps its values in a pre-provisioned secret:
	// check the required keys against that secret's inventory instead
	if l := officialEnv(m, e); l != nil {
		return officialEnvFindings(m, l, e, name, fs), nil
	}
	// every ${VAR} the workloads reference must have a value
	vals, _ := dotenv.Read(filepath.Join(m.Infra, ".env"))
	if st := secrets.New(or(e.Context, m.C.Deploy.Context), descriptor.Slug(m.C.Name), name); st.Available() {
		if list, err := st.List(); err == nil {
			for _, x := range list {
				vals[x.Name] = "(stored)"
			}
		}
	}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && v != "" {
			if _, have := vals[k]; !have {
				vals[k] = "(env)"
			}
		}
	}
	derived := map[string]bool{"POSTGRES_PASSWORD": true, "IAM_SIGNING_KEY": true, "OIDC_CLIENT_SECRET": true, "KEYCLOAK_ADMIN_PASSWORD": true}
	in := &gen.Input{M: m, Env: e, Ov: ov, Target: "docker", PG: gen.PG{Host: "postgres", Port: 5432, User: gen.PGUser(m), Password: "${POSTGRES_PASSWORD}"}}
	var missing []string
	for _, v := range in.SecretVars() {
		if _, ok := vals[v]; ok || derived[v] || strings.HasSuffix(v, "_CLICKHOUSE_WRITER_PASSWORD") || strings.HasSuffix(v, "_CLICKHOUSE_READER_PASSWORD") {
			continue
		}
		missing = append(missing, v)
	}
	sort.Strings(missing)
	for _, v := range missing {
		add("ENV_VALUE_MISSING", "blocking", "${"+v+"}", "referenced by the descriptor but not set in axon secrets or "+m.L.Infra+"/.env", "axon secrets set "+v+"  (or add "+v+"=… to "+m.L.Infra+"/.env)")
	}
	return fs, nil
}

func runEnvCheck(c *cli.Ctx) error {
	m, err := controller(c)
	if err != nil {
		return err
	}
	if c.Bool("live") {
		e, err := envcfg.Load(m.Dir, m.C.Name, c.Arg(0))
		if err != nil {
			return err
		}
		if officialEnv(m, e) == nil {
			return cli.Usage("env check --live: %s is not an official environment of a pipeline-released cluster", c.Arg(0))
		}
		if err := inventoryOutsideRepos(m, c.Arg(0)); err != nil {
			return err
		}
		inv, err := refreshInventory(m, e, c.Arg(0), c.String("context"))
		if err != nil {
			return err
		}
		if !c.Bool("json") {
			ui.Sub("key inventory refreshed: %d key name(s) in secret %s", len(inv.Keys), appEnvSecret(m))
		}
	}
	fs, err := envFindings(m, c.Arg(0))
	if err != nil {
		return err
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

func runEnvDiagnose(c *cli.Ctx) error {
	m, err := controller(c)
	if err != nil {
		return err
	}
	env := c.Arg(0)
	want := map[string]bool{}
	for _, v := range c.Strings("concern") {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				want[p] = true
			}
		}
	}
	for k := range want {
		ok := false
		for _, cc := range check.Concerns {
			ok = ok || cc == k
		}
		if !ok {
			return cli.Usage("env diagnose: unknown concern %q (%s)", k, strings.Join(check.Concerns, ", "))
		}
	}
	all := len(want) == 0
	var fs []check.Finding
	var notChecked []string
	s, serr := openSession(m, deployOpts{Verb: "env diagnose", Env: env, Target: c.String("target"), Context: c.String("context")})
	target := "docker"
	if serr == nil {
		target = s.Target
	}
	if all || want["toolchain"] {
		fs = append(fs, check.ToolchainFindings(target)...)
	}
	if all || want["conformance"] {
		ef, err := envFindings(m, env)
		if err != nil {
			return err
		}
		fs = append(fs, ef...)
		if serr == nil {
			if changes, err := s.Generate(true); err == nil {
				var stale []string
				for _, ch := range changes {
					if ch.Changed {
						stale = append(stale, ch.Path)
					}
				}
				if len(stale) > 0 {
					fs = append(fs, check.Finding{ID: "CONFORMANCE_CONFIG_DRIFT", Concern: "conformance", Severity: "risk", Subject: "generated files",
						Cause: fmt.Sprintf("%d generated file(s) are stale (%s…)", len(stale), stale[0]), Fix: "axon cluster generate"})
				}
			}
		}
	}
	if all || want["security"] {
		fs = append(fs, check.Security(m, check.GitIgnored(m.Dir))...)
	}
	if all || want["devx"] {
		fs = append(fs, check.DevX(m)...)
	}
	if all || want["business"] {
		fs = append(fs, check.Business(m)...)
	}
	if all || want["function"] || want["performance"] {
		if serr != nil {
			notChecked = append(notChecked, "function", "performance")
		} else {
			ff, nc := liveFindings(s)
			fs = append(fs, ff...)
			notChecked = append(notChecked, nc...)
		}
	}
	if c.Bool("json") {
		if fs == nil {
			fs = []check.Finding{}
		}
		if notChecked == nil {
			notChecked = []string{}
		}
		ui.JSON(map[string]any{"findings": fs, "notChecked": notChecked})
	} else {
		check.Print(fs)
		if len(notChecked) > 0 {
			ui.Out("\nnot checked: %s", strings.Join(notChecked, ", "))
		}
	}
	if b, _, _ := check.Count(fs); b > 0 {
		return cli.Failed()
	}
	return nil
}

// liveFindings checks a running environment through its front door.
func liveFindings(s *sessionT) ([]check.Finding, []string) {
	var fs []check.Finding
	if s.Target == "k8s" {
		if err := s.K8sPreflightPublic(); err != nil {
			return nil, []string{"function (" + err.Error() + ")", "performance"}
		}
	}
	if err := s.PreparePublic(); err != nil {
		return nil, []string{"function", "performance"}
	}
	st, err := s.Status()
	if err != nil {
		return nil, []string{"function (" + err.Error() + ")", "performance"}
	}
	if len(st.Workloads) == 0 {
		fs = append(fs, check.Finding{ID: "CONFORMANCE_NOT_DEPLOYED", Concern: "conformance", Severity: "blocking", Subject: s.Env.Name, Cause: "nothing is running", Fix: "axon up --build"})
		return fs, []string{"function", "performance"}
	}
	for _, w := range st.Workloads {
		if !w.Healthy {
			fs = append(fs, check.Finding{ID: "FUNCTION_MODULE_NOT_SERVING", Concern: "function", Severity: "blocking", Subject: w.Name, Cause: "workload is " + w.State + " (" + w.Ready + ")", Fix: "axon env logs " + s.Env.Name + " " + w.App})
		}
	}
	r := &journey.Runner{Port: s.M.C.HTTPPort(), Domain: s.M.C.DomainOrDefault()}
	for _, a := range s.M.Apps {
		if !a.Decl.Cored() || a.Problem != "" {
			continue
		}
		p := r.ProbeSystem(a.Decl.Name, a.Decl.Host)
		switch {
		case p.Status == 0:
			fs = append(fs, check.Finding{ID: "FUNCTION_CORE_UNREACHABLE", Concern: "function", Severity: "blocking", Subject: a.Decl.Name, Cause: p.URL + " does not answer", Fix: "axon env status " + s.Env.Name})
		case p.Status >= 500:
			fs = append(fs, check.Finding{ID: "FUNCTION_CORE_UNHEALTHY", Concern: "function", Severity: "blocking", Subject: a.Decl.Name, Cause: fmt.Sprintf("/api/system answered %d", p.Status), Fix: "axon env logs " + s.Env.Name + " " + a.Decl.Name})
		case p.Status == 200 && !p.Pass:
			fs = append(fs, check.Finding{ID: "FUNCTION_MODULE_NOT_SERVING", Concern: "function", Severity: "blocking", Subject: a.Decl.Name, Cause: "a module reports health other than SERVING", Fix: "axon env logs " + s.Env.Name + " " + a.Decl.Name + " --service <token>"})
		case p.Status == 200 && p.Report == nil:
			fs = append(fs, check.Finding{ID: "FUNCTION_CORE_REPORT_UNREADABLE", Concern: "function", Severity: "note", Subject: a.Decl.Name, Cause: "/api/system did not return JSON"})
		}
	}
	return fs, []string{"performance (needs a load profile)"}
}

func runEnvTest(c *cli.Ctx) error {
	m, err := controller(c)
	if err != nil {
		return err
	}
	js, err := journey.Load(filepath.Join(m.Infra, "journeys"))
	if err != nil {
		return err
	}
	if only := c.Strings("journey"); len(only) > 0 {
		want := map[string]bool{}
		for _, o := range only {
			want[strings.TrimSuffix(o, ".toml")] = true
		}
		var keep []*journey.Journey
		for _, j := range js {
			if want[strings.TrimSuffix(filepath.Base(j.Path), ".toml")] {
				keep = append(keep, j)
			}
		}
		js = keep
	}
	hosts := map[string]bool{}
	for _, h := range gen.Hosts(m, m.C.Deploy.Products) {
		hosts[strings.TrimSuffix(h, "."+m.C.DomainOrDefault())] = true
	}
	if c.Bool("list") {
		for _, j := range js {
			ui.Out("%-40s app=%-14s %d step(s)", strings.TrimSuffix(filepath.Base(j.Path), ".toml"), j.Meta.App, len(j.Steps))
		}
		return nil
	}
	if c.Bool("lint") {
		probs := journey.Lint(js, hosts)
		for _, p := range probs {
			ui.Out("%s %s", ui.Red("✗"), p)
		}
		if len(probs) > 0 {
			return cli.Failed()
		}
		ui.OK("%d journey(s) valid", len(js))
		return nil
	}
	vals, _ := dotenv.Read(filepath.Join(m.Infra, ".env"))
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vals[k] = v
		}
	}
	r := &journey.Runner{Port: m.C.HTTPPort(), Domain: m.C.DomainOrDefault(), Vars: vals}
	lr := &journey.LastRun{At: time.Now().UTC().Format(time.RFC3339), Cluster: m.C.Name, Env: c.Arg(0), Pass: true}
	if !c.Bool("skip-probe") {
		ui.Step("probe gate")
		for _, a := range m.Apps {
			if !a.Decl.Cored() || a.Problem != "" {
				continue
			}
			p := r.ProbeSystem(a.Decl.Name, a.Decl.Host)
			for wait := 0; !p.Pass && wait < 10; wait++ { // modules may still be (re)connecting
				time.Sleep(3 * time.Second)
				p = r.ProbeSystem(a.Decl.Name, a.Decl.Host)
			}
			lr.Probes = append(lr.Probes, p)
			if p.Pass {
				ui.OK("%s %s", a.Decl.Name, p.URL)
			} else {
				ui.Fail("%s %s (status %d)", a.Decl.Name, p.URL, p.Status)
				lr.Pass = false
			}
		}
	}
	ui.Step("journeys (%d)", len(js))
	for _, j := range js {
		if !hosts[j.Meta.App] {
			lr.Journeys = append(lr.Journeys, journey.Result{Name: j.Meta.Name, App: j.Meta.App, Pass: true,
				Skipped: "host " + j.Meta.App + " is not deployed in this cluster"})
			ui.Note("skipped %s — host %q is not deployed in this cluster (`axon env test %s --lint` lists such journeys)", j.Meta.Name, j.Meta.App, c.Arg(0))
			continue
		}
		res := r.Run(j)
		lr.Journeys = append(lr.Journeys, res)
		if res.Pass {
			ui.OK("%s (%d step(s))", res.Name, len(res.Steps))
			continue
		}
		lr.Pass = false
		last := res.Steps[len(res.Steps)-1]
		ui.Fail("%s — step %q: %s", res.Name, last.Name, last.Reason)
	}
	if err := journey.Save(filepath.Join(m.Dir, "verify"), lr); err != nil {
		return err
	}
	if c.Bool("json") {
		ui.JSON(lr)
	}
	if !lr.Pass {
		return cli.Failed()
	}
	ui.OK("verified %s/%s → verify/last-run.json", m.C.Name, c.Arg(0))
	return nil
}

func applyRelease(s *sessionT, version string) error {
	r, err := release.Load(filepath.Join(release.Dir(s.M.Dir), version+".toml"))
	if err != nil {
		return err
	}
	if !release.MadeByAxon(r) {
		return fmt.Errorf("release %s was produced by %q; axon deploys releases it recorded (`axon release create`)", version, r.Str("tool_version"))
	}
	if s.Refs == nil {
		s.Refs = map[string]string{}
	}
	for repo, ref := range r.Table("images") {
		switch {
		case strings.Contains(ref, "@sha256:"): // pushed: registry/repo@sha256:…
			s.Refs[repo] = ref
		case strings.HasPrefix(ref, repo+":"): // local: repo:tag
			s.Tags[repo] = strings.TrimPrefix(ref, repo+":")
		default:
			return fmt.Errorf("release %s: unrecognised image reference %q for %s", version, ref, repo)
		}
	}
	ui.Note("deploying release %s (%d images)", version, len(r.Table("images")))
	return nil
}
