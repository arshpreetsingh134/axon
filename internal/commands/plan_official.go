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
	version := or(c.Arg(1), e.Release)
	rec, err := loadRelease(m, version)
	if err != nil {
		return err
	}
	desc, err := official.LoadDescriptor(m.Found.Path)
	if err != nil {
		return err
	}
	pg, err := official.LoadPG(m.Dir, env)
	if err != nil {
		return fmt.Errorf("platforms/%s.outputs.json: %w", env, err)
	}
	titles, tokens, err := l.MemberMeta()
	if err != nil {
		return err
	}
	in := &official.RenderInput{Namespace: e.Namespace, Domain: e.Domain, IngressClass: or(e.IngressClass, "traefik"), PG: pg,
		Images: rec.Table("modules"), Desc: desc, Titles: titles, Tokens: tokens}

	ctx := or(c.String("context"), e.Context)
	offline := c.Bool("offline")
	if !offline && ctx == "" {
		return cli.Usage("env plan: pass --context <your kube context for the cluster>, or --offline to only render")
	}
	if !offline {
		ip, err := sh.Output("kubectl", "--context", ctx, "-n", e.Namespace, "get", "service", "traefik-internal", "-o", "jsonpath={.spec.clusterIP}")
		if err != nil {
			return fmt.Errorf("read the traefik-internal service IP: %w", err)
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
	tables := map[string]map[string]string{}
	for _, t := range []string{"fabric", "products", "knowledge_base", "modules"} {
		tables[t] = rec.Table(t)
	}
	fabric, fskipped := official.RenderFabric(in, official.LoadFabricVars(m.Dir, env, tables))
	objs, skipped = append(objs, fabric...), append(skipped, fskipped...)
	objs = append(objs, official.RenderDatabases(in))
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
	objs = append(objs, official.RenderNetworkPolicies(in, append(append([]official.Object{}, objs...), shadow...), official.LoadFrontendIP(m.Dir, env))...)
	objs = append(objs, official.RenderSingletons(in, official.LoadHelixVaultURL(m.Dir, env))...)

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
