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
	objs, shadow, skipped := official.RenderAppCore(in)
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
	ui.Note("rendered: every switcher app's ServiceAccount, Service, Ingress, delegation ConfigMap and Deployment (except the IAM and console Deployments), their NetworkPolicies, and the namespace-wide policies")
	ui.Note("not rendered yet: the workers, the fabric (traefik, temporal, keycloak, grafana, tempo, helix, clickhouse) with their jobs and policies, and the IAM and console Deployments")
	for _, s := range skipped {
		ui.Sub("not rendered yet: %s", s)
	}
	return nil
}
