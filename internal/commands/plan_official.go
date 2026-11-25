package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"axon/internal/cli"
	"axon/internal/descriptor"
	"axon/internal/envcfg"
	"axon/internal/official"
	"axon/internal/sh"
	"axon/internal/ui"
)

// runOfficialPlan is `env plan <env> [<release>]` for an official environment
// (plan T1.1/T1.3): render the deploy objects axon knows how to render, read
// the live objects' last-applied configuration with the developer's own kube
// context, and show what a deploy would change. It changes nothing.
func runOfficialPlan(c *cli.Ctx, m *descriptor.Model, l *official.Layout, e *envcfg.Env, env string) error {
	ctx := or(or(c.String("context"), e.Context), m.C.Deploy.Context)
	offline := c.Bool("offline")
	if !offline && ctx == "" {
		return cli.Usage("env plan: pass --context <your kube context for the cluster>, or --offline to only render")
	}
	r, err := renderOfficial(m, l, e, env, c.Arg(1), ctx, offline)
	if err != nil {
		return err
	}
	objs, skipped, version := r.objs, r.skipped, r.version
	if out := c.String("out"); out != "" {
		for _, o := range objs {
			k := strings.Split(official.Key(o), "/")
			dir := filepath.Join(out, k[0])
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			b, _ := json.MarshalIndent(official.Normalize(o), "", " ")
			if err := os.WriteFile(filepath.Join(dir, k[1]+".json"), append(b, '\n'), 0o644); err != nil {
				return err
			}
		}
		ui.OK("wrote %d rendered object(s) to %s", len(objs), out)
	}
	if offline {
		ui.OK("rendered %d object(s) for release %s (offline: not compared with the cluster)", len(objs), version)
		for _, s := range skipped {
			ui.Sub("not rendered yet: %s", s)
		}
		return nil
	}

	kinds := map[string]bool{}
	for _, o := range objs {
		kinds[strings.ToLower(fmt.Sprint(o["kind"]))] = true
	}
	var kl []string
	for k := range kinds {
		kl = append(kl, k)
	}
	sort.Strings(kl)
	live, _, err := official.FetchLive(ctx, e.Namespace, kl)
	if err != nil {
		return err
	}
	plan := official.Plan(objs, live)
	if c.Bool("json") {
		return ui.JSON(map[string]any{"release": version, "plan": plan, "notRendered": skipped})
	}
	n := map[string]int{}
	for _, p := range plan {
		n[p.Action]++
		switch p.Action {
		case "create":
			ui.Out("  %s %s", ui.Green("+"), p.Key)
		case "change":
			ui.Out("  %s %s", ui.Yellow("~"), p.Key)
			max := 6
			if c.Bool("verbose") {
				max = len(p.Diffs)
			}
			for i, d := range p.Diffs {
				if i == max {
					ui.Out("      … %d more (--verbose)", len(p.Diffs)-max)
					break
				}
				ui.Out("      %s", d)
			}
		}
	}
	ui.OK("release %s on %s: %d unchanged, %d to change, %d to create (of %d rendered objects)", version, env, n["unchanged"], n["change"], n["create"], len(objs))
	ui.Note("not rendered yet: the IAM schema ConfigMap (shipped inside the IAM image) and the secrets fingerprint (needs secret values)")
	for _, s := range skipped {
		ui.Sub("not rendered yet: %s", s)
	}
	return nil
}

// liveIAMInputs reads the IAM Deployment's runtime inputs from the cluster:
// the first administrator's subject (from the live Deployment) and the IAM
// configuration's hash (from the live ConfigMap).
func liveIAMInputs(ctx, ns string, desc *official.Descriptor) (subject, hash string) {
	for _, a := range desc.App {
		if a.Kind != "iam" {
			continue
		}
		la := `jsonpath={.metadata.annotations.kubectl\.kubernetes\.io/last-applied-configuration}`
		if out, err := sh.Output("kubectl", "--context", ctx, "-n", ns, "get", "deployment", official.K8sName(a.Name), "-o", la); err == nil && out != "" {
			var d official.Object
			if json.Unmarshal([]byte(out), &d) == nil {
				t, _ := d["spec"].(map[string]any)["template"].(map[string]any)
				if md, ok := t["metadata"].(map[string]any); ok {
					if an, ok := md["annotations"].(map[string]any); ok {
						subject, _ = an["mycel.dev/first-admin-subject"].(string)
					}
				}
			}
		}
		if out, err := sh.Output("kubectl", "--context", ctx, "-n", ns, "get", "configmap", "mycel-iam-configuration", "-o",
			`jsonpath={.metadata.annotations.mycel\.dev/iam-configuration}`); err == nil {
			hash = out
		}
	}
	return subject, hash
}

// rendered is what renderOfficial produced.
type rendered struct {
	objs    []official.Object
	skipped []string
	version string
	dev     bool
}

// renderOfficial renders the deploy objects of an environment of a
// pipeline-released cluster. An official environment renders from a release
// record; a dev environment (the developer profile) renders the members'
// pinned images (github/materializations) with plain HTTP on the front door,
// the local Docker Postgres and the environment's overrides, and leaves the
// running fabric, databases and NetworkPolicies to the cluster.
func renderOfficial(m *descriptor.Model, l *official.Layout, e *envcfg.Env, env, release, ctx string, offline bool) (*rendered, error) {
	dev := e.Tier == "dev"
	desc, err := official.LoadDescriptor(m.Found.Path)
	if err != nil {
		return nil, err
	}
	titles, tokens, err := l.MemberMeta()
	if err != nil {
		return nil, err
	}
	in := &official.RenderInput{Namespace: e.Namespace, Domain: e.Domain, IngressClass: or(e.IngressClass, "traefik"), Desc: desc, Titles: titles, Tokens: tokens}
	version := or(release, e.Release)
	tables := map[string]map[string]string{}
	traefikSvc := "traefik-internal"
	if dev {
		version = "local"
		in.Images = l.PinnedImages()
		for k, v := range l.WorkerImages() {
			in.Images[k] = v
		}
		in.Scheme, in.PGDocker, in.ImageIDs = "http", true, true
		in.FrontPort = fabricInt(desc, "traefik", "http")
		in.PG = official.PG{Host: "host.docker.internal", Port: fabricInt(desc, "postgres", "port"), User: "mycel"}
		if ov, err := envcfg.LoadOverrides(m.Dir, env); err == nil {
			in.AppEnv = ov.AppEnv
		}
		traefikSvc = "traefik"
		in.Entrypoints, in.NoPullSecret = "web", true
		if !offline {
			devRuntime(ctx, e.Namespace, in, desc)
		}
	} else {
		rec, err := loadRelease(m, version)
		if err != nil {
			return nil, err
		}
		pg, err := official.LoadPG(m.Dir, env)
		if err != nil {
			return nil, fmt.Errorf("platforms/%s.outputs.json: %w", env, err)
		}
		in.PG, in.Images = pg, rec.Table("modules")
		for _, t := range []string{"fabric", "products", "knowledge_base", "modules"} {
			tables[t] = rec.Table(t)
		}
	}
	if !offline {
		ip, err := sh.Output("kubectl", "--context", ctx, "-n", e.Namespace, "get", "service", traefikSvc, "-o", "jsonpath={.spec.clusterIP}")
		if err != nil {
			return nil, fmt.Errorf("read the %s service IP: %w", traefikSvc, err)
		}
		in.TraefikIP = ip
	}
	in.ClusterName = m.C.Name
	if !offline {
		in.IAMFirstAdminSubject, in.IAMConfigHash = liveIAMInputs(ctx, e.Namespace, desc)
	}
	objs, shadow, skipped := official.RenderAppCore(in)
	workers, wskipped := official.RenderWorkers(in)
	objs, skipped = append(objs, workers...), append(skipped, wskipped...)
	if !dev {
		fabric, fskipped := official.RenderFabric(in, official.LoadFabricVars(m.Dir, env, tables))
		objs, skipped = append(objs, fabric...), append(skipped, fskipped...)
		objs = append(objs, official.RenderDatabases(in))
	}
	if kc, ok := desc.Fabric["keycloak"].(map[string]any); ok {
		var liveIAM official.Object
		if !offline {
			if out, err := sh.Output("kubectl", "--context", ctx, "-n", e.Namespace, "get", "configmap", "mycel-iam-configuration",
				"-o", `jsonpath={.metadata.annotations.kubectl\.kubernetes\.io/last-applied-configuration}`); err == nil && out != "" {
				_ = json.Unmarshal([]byte(out), &liveIAM)
			}
		}
		realm, _ := kc["realm"].(string)
		client, _ := kc["client"].(string)
		iam, unknown := official.RenderIAMConfig(in, official.LoadDemoPeople(m.Dir), realm, client, official.LiveIAMRuntime(liveIAM))
		objs = append(objs, iam)
		for _, u := range unknown {
			skipped = append(skipped, "IAM configuration: "+u)
		}
	}
	if !dev { // a developer's namespace carries no NetworkPolicies
		objs = append(objs, official.RenderNetworkPolicies(in, append(append([]official.Object{}, objs...), shadow...), official.LoadFrontendIP(m.Dir, env))...)
		objs = append(objs, official.RenderSingletons(in, official.LoadHelixVaultURL(m.Dir, env))...)
	}
	return &rendered{objs: objs, skipped: skipped, version: version, dev: dev}, nil
}

// fabricInt reads [fabric.<name>] <key> as an int (0 when absent).
func fabricInt(d *official.Descriptor, name, key string) int {
	if t, ok := d.Fabric[name].(map[string]any); ok {
		switch v := t[key].(type) {
		case int64:
			return int(v)
		case float64:
			return int(v)
		}
	}
	return 0
}

// devRuntime fills a developer deploy's runtime inputs from the cluster: the
// platform apps' images (they are pinned by the reference tool, not the
// repository), and the members left as they are because their proof-key
// secret does not exist yet (only its existence is checked, never its value).
func devRuntime(ctx, ns string, in *official.RenderInput, desc *official.Descriptor) {
	in.Exclude = map[string]string{}
	apps := map[string]bool{}
	for _, a := range desc.App {
		apps[a.Name] = true
	}
	for _, s := range desc.Switcher.App {
		apps[s.App] = true
	}
	left := map[string]bool{} // projects with a member left as it is
	for name := range apps {
		N := official.K8sName(name)
		if in.Images[name] == "" {
			if img, err := sh.Output("kubectl", "--context", ctx, "-n", ns, "get", "deployment", N, "-o", "jsonpath={.spec.template.spec.containers[0].image}"); err == nil && img != "" {
				in.Images[name] = img
			}
		}
		isProject := false
		for _, a := range desc.App {
			if a.Name == name && a.Kind == "project" {
				isProject = true
			}
		}
		if isProject {
			continue
		}
		if _, err := sh.Output("kubectl", "--context", ctx, "-n", ns, "get", "secret", "mycel-host-"+strings.TrimPrefix(N, "app-"), "-o", "name"); err != nil {
			in.Exclude[name] = "no proof-key secret yet; the reference tool's deploy creates it"
			left[projectOf(name)] = true
		}
	}
	in.ConsoleModules = map[string][]string{}
	console := desc.Product["clc"].App
	if top, err := sh.Output("kubectl", "--context", ctx, "-n", ns, "get", "deployment", official.K8sName(console), "-o",
		`jsonpath={.spec.template.spec.containers[0].env[?(@.name=="MYCEL_CONSOLE_TOPOLOGY")].value}`); console != "" && err == nil && top != "" {
		var t struct {
			Apps []struct {
				Name    string   `json:"name"`
				Modules []string `json:"modules"`
			} `json:"apps"`
		}
		if json.Unmarshal([]byte(top), &t) == nil {
			for _, a := range t.Apps {
				in.ConsoleModules[a.Name] = a.Modules
			}
		}
	}
	for _, a := range desc.App {
		if a.Kind == "project" && left[projectOf(a.Name)] {
			in.Exclude[a.Name] = "its project's apps stay on their current layout"
		}
	}
}

// projectOf is the project a member belongs to: its name without the last
// dotted part (<project>.app, <project>.wfc, <project>.<module>).
func projectOf(name string) string {
	if i := strings.LastIndex(name, "."); i > 0 {
		return name[:i]
	}
	return name
}
