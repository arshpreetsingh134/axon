package commands

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"axon/internal/cli"
	"axon/internal/deploy"
	"axon/internal/descriptor"
	"axon/internal/envcfg"
	"axon/internal/official"
	"axon/internal/sh"
	"axon/internal/ui"
)

// runPipelineDevDeploy is `env deploy dev` / `up` for the dev environment of a
// pipeline-released cluster whose members are image-backed. It deploys exactly
// what `env plan dev` shows: the official renderer's objects for the pinned
// images, with the dev profile (SPEC.md Appendix C.11). It applies only the
// objects the plan creates or changes (kubectl apply, so each keeps the
// last-applied configuration the plan compares with), waits for every rolled
// Deployment, retires the old-layout worker a newly created worker replaces,
// and runs the cluster's post_deploy hooks. The images are the pipeline's pins;
// with --build, the members a [[local_build]] recipe covers are built from the
// developer's checkouts with the pipeline's Dockerfile when the checkout moved
// past its pin or has uncommitted changes (--app names members, --rebuild takes
// every one), and run instead of their pins (SPEC.md Appendix C.12).
func runPipelineDevDeploy(c *cli.Ctx, m *descriptor.Model, l *official.Layout, e *envcfg.Env) error {
	ctx := or(or(c.String("context"), e.Context), m.C.Deploy.Context)
	if ctx == "" {
		return cli.Usage("%s: pass --context <your kube context for the dev cluster>", c.Verb.Name)
	}
	if len(c.Strings("module")) > 0 {
		return cli.Usage("%s: the dev environment of this cluster runs one image per member; --module does not apply (use --build, with --app <member> to build one)", c.Verb.Name)
	}
	if !c.Bool("build") && (len(c.Strings("app")) > 0 || c.Bool("rebuild")) {
		return cli.Usage("%s: --app and --rebuild choose what --build builds; add --build", c.Verb.Name)
	}
	dry := c.Bool("dry-run")
	timeout := 10 * time.Minute
	if t := c.String("timeout"); t != "" {
		d, err := time.ParseDuration(t)
		if err != nil {
			return cli.Usage("%s: --timeout wants a duration like 10m, got %q", c.Verb.Name, t)
		}
		timeout = d
	}

	var local map[string]string
	if c.Bool("build") {
		lock, err := l.Lock()
		if err != nil {
			return err
		}
		_, tokens, err := l.MemberMeta()
		if err != nil {
			return err
		}
		recipes := localRecipes(m.Overlay.LocalBuild, m.Dir, m.Root, l)
		if len(recipes) == 0 {
			return cli.Usage("%s --build: no build recipe: neither a GitHub build workflow nor a [[local_build]] recipe builds a member (SPEC.md Appendix C.12)", c.Verb.Name)
		}
		builds, notes, err := planLocalBuilds(recipes, l.Workflow.Materializations, lock, tokens, m.Root, m.Dir,
			c.Strings("app"), c.Bool("rebuild"), realGit{})
		if err != nil {
			return err
		}
		for _, n := range notes {
			ui.Sub("%s", n)
		}
		if len(builds) == 0 {
			ui.Note("--build: every checkout is at its pin; nothing to build")
		}
		if dry {
			for _, b := range builds {
				ui.Sub("would build %s (%s) with %s", b.Target, b.Why, l.Rel(b.Dockerfile))
			}
		} else if len(builds) > 0 {
			if local, err = buildLocal(builds, c.Bool("rebuild")); err != nil {
				return err
			}
		}
	}

	// Workflow clients (SPEC.md Appendix C.13): their private key Secrets exist before the render mounts them.
	desc, err := official.LoadDescriptor(m.Found.Path)
	if err != nil {
		return err
	}
	var overrides map[string]map[string]string
	if ov, err := envcfg.LoadOverrides(m.Dir, e.Name); err == nil {
		overrides = ov.AppEnv
	}
	ids := devIdentities(desc, overrides)
	if _, err := ensureKeySecrets(ids, ctx, e.Namespace, dry); err != nil {
		return err
	}
	keys := map[string]official.ServiceKey{}
	for _, id := range ids {
		keys[id.Member] = official.ServiceKey{Secret: id.Secret, Path: id.KeyFile, Extra: id.extraKeys()}
	}

	r, err := renderOfficial(m, l, e, e.Name, "", ctx, false, local, keys)
	if err != nil {
		return err
	}
	live, _, err := official.FetchLive(ctx, e.Namespace, kindsOf(r.objs))
	if err != nil {
		return err
	}
	plan := official.Plan(r.objs, live)
	todo := map[string]string{}
	for _, p := range plan {
		if p.Action != "unchanged" {
			todo[p.Key] = p.Action
		}
	}
	var apply []official.Object
	for _, o := range r.objs {
		if todo[official.Key(official.Normalize(o))] != "" {
			apply = append(apply, official.Normalize(o))
		}
	}
	sortForApply(apply)
	ui.Note("%s on %s (%s): %d unchanged, %d to apply (of %d rendered objects)", c.Verb.Name, e.Name, ctx, len(plan)-len(apply), len(apply), len(r.objs))
	for _, s := range r.skipped {
		ui.Sub("left as it is: %s", s)
	}

	if err := pullMissing(apply, dry); err != nil {
		return err
	}
	if len(apply) > 0 {
		for _, o := range apply {
			k := official.Key(o)
			mark := ui.Yellow("~")
			if todo[k] == "create" {
				mark = ui.Green("+")
			}
			ui.Out("  %s %s", mark, k)
		}
		list, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": apply})
		args := []string{"--context", ctx, "-n", e.Namespace, "apply", "-f", "-"}
		if dry {
			args = append(args, "--dry-run=server")
		}
		res, err := sh.Run(sh.Cmd{Name: "kubectl", Args: args, Stdin: string(list), Timeout: 5 * time.Minute})
		if err != nil {
			return fmt.Errorf("kubectl apply: %w\n%s", err, strings.TrimSpace(res.Stderr))
		}
		for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
			ui.Sub("%s", line)
		}
	}
	var olds []string
	if !c.Bool("keep-superseded") {
		olds = superseded(r.objs, ctx, e.Namespace)
	}
	if dry {
		for _, old := range olds {
			ui.Sub("would scale deployment/%s to 0 once deployment/%s-worker is ready (it polls the same task queue)", old, old)
		}
		for _, id := range ids {
			ui.Sub("would register workflow client %s in %s and grant it workflow.call on %d site(s)", id.ClientID, r.iamOrigin, len(id.Sites))
		}
		ui.OK("dry run: the API server accepted %d object(s); nothing was changed", len(apply))
		return deploy.RunStage(m, deploy.HookVars{Env: e.Name, Target: "k8s", Namespace: e.Namespace, KubeContext: ctx}, "post_deploy", true)
	}

	var rolled []string
	for _, o := range apply {
		if o["kind"] == "Deployment" {
			rolled = append(rolled, name(o))
		}
	}
	for _, d := range rolled {
		ui.Step("waiting for deployment/%s", d)
		if res, err := sh.Run(sh.Cmd{Name: "kubectl", Args: []string{"--context", ctx, "-n", e.Namespace, "rollout", "status", "deployment/" + d,
			"--timeout", timeout.String()}, Timeout: timeout + time.Minute}); err != nil {
			return fmt.Errorf("deployment/%s did not become ready: %w\n%s", d, err, strings.TrimSpace(res.Stderr+res.Stdout))
		}
	}

	for _, old := range olds {
		if res, err := sh.Run(sh.Cmd{Name: "kubectl", Args: []string{"--context", ctx, "-n", e.Namespace, "rollout", "status", "deployment/" + old + "-worker",
			"--timeout", timeout.String()}, Timeout: timeout + time.Minute}); err != nil {
			return fmt.Errorf("deployment/%s-worker is not ready, so deployment/%s keeps running: %w\n%s", old, old, err, strings.TrimSpace(res.Stderr+res.Stdout))
		}
		ui.Step("deployment/%s is replaced by deployment/%s-worker: scaling it to 0 (the Deployment is kept; --keep-superseded leaves it running)", old, old)
		if res, err := sh.Run(sh.Cmd{Name: "kubectl", Args: []string{"--context", ctx, "-n", e.Namespace, "scale", "deployment/" + old, "--replicas=0"}}); err != nil {
			return fmt.Errorf("scale deployment/%s: %w\n%s", old, err, strings.TrimSpace(res.Stderr))
		}
	}

	// The IAM reads its configuration at start: when this deploy changed it (a catalog read from a new image, a new
	// application), restart the IAM so the registrations below see it.
	if todo[official.Key(official.Normalize(official.Object{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "mycel-iam-configuration", "namespace": e.Namespace}}))] != "" {
		for _, a := range desc.App {
			if a.Kind != "iam" {
				continue
			}
			n := official.K8sName(a.Name)
			ui.Step("the IAM configuration changed: restarting deployment/%s", n)
			if res, err := sh.Run(sh.Cmd{Name: "kubectl", Args: []string{"--context", ctx, "-n", e.Namespace, "rollout", "restart", "deployment/" + n}}); err != nil {
				return fmt.Errorf("restart deployment/%s: %w\n%s", n, err, strings.TrimSpace(res.Stderr))
			}
			if res, err := sh.Run(sh.Cmd{Name: "kubectl", Args: []string{"--context", ctx, "-n", e.Namespace, "rollout", "status", "deployment/" + n,
				"--timeout", timeout.String()}, Timeout: timeout + time.Minute}); err != nil {
				return fmt.Errorf("deployment/%s did not become ready: %w\n%s", n, err, strings.TrimSpace(res.Stderr+res.Stdout))
			}
		}
	}

	if len(ids) > 0 {
		actor, password := "", ""
		if con := m.C.Fabric.Console; con != nil {
			actor, password = con.PushActor, con.PushPassword
		}
		if err := provisionIdentities(ids, r.iamOrigin, actor, password, ctx, e.Namespace); err != nil {
			return fmt.Errorf("workflow clients: %w", err)
		}
	}

	if !c.Bool("no-hooks") {
		if err := deploy.RunStage(m, deploy.HookVars{Env: e.Name, Target: "k8s", Namespace: e.Namespace, KubeContext: ctx}, "post_deploy", false); err != nil {
			return err
		}
	}
	ui.OK("%s is deployed: release %s, %d object(s) applied, %d rolled out", e.Name, r.version, len(apply), len(rolled))
	return nil
}

// superseded lists the running Deployments a rendered worker replaces: an
// old-layout Deployment named like the worker without its "-worker" suffix that
// the renderer does not render, so both would poll the same task queue.
func superseded(objs []official.Object, ctx, ns string) []string {
	rendered := map[string]bool{}
	for _, o := range objs {
		if o["kind"] == "Deployment" {
			rendered[name(o)] = true
		}
	}
	var out []string
	for _, o := range objs {
		n := name(o)
		if o["kind"] != "Deployment" || !strings.HasSuffix(n, "-worker") {
			continue
		}
		old := strings.TrimSuffix(n, "-worker")
		if rendered[old] {
			continue
		}
		res, err := sh.Run(sh.Cmd{Name: "kubectl", Args: []string{"--context", ctx, "-n", ns, "get", "deployment", old, "-o", "jsonpath={.spec.replicas}"}})
		if err != nil || strings.TrimSpace(res.Stdout) == "0" || strings.TrimSpace(res.Stdout) == "" {
			continue
		}
		out = append(out, old)
	}
	return out
}

// kindsOf lists the lower-cased kinds of objs, sorted.
func kindsOf(objs []official.Object) []string {
	seen := map[string]bool{}
	var out []string
	for _, o := range objs {
		k := strings.ToLower(fmt.Sprint(o["kind"]))
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// applyOrder puts what a workload refers to before the workload.
var applyOrder = map[string]int{"ServiceAccount": 0, "Role": 1, "RoleBinding": 2, "ConfigMap": 3, "Service": 4, "Deployment": 5, "Ingress": 6}

func sortForApply(objs []official.Object) {
	rank := func(o official.Object) int {
		if r, ok := applyOrder[fmt.Sprint(o["kind"])]; ok {
			return r
		}
		return 9
	}
	sort.SliceStable(objs, func(i, j int) bool {
		if ri, rj := rank(objs[i]), rank(objs[j]); ri != rj {
			return ri < rj
		}
		return official.Key(objs[i]) < official.Key(objs[j])
	})
}

func name(o official.Object) string {
	m, _ := o["metadata"].(map[string]any)
	return fmt.Sprint(m["name"])
}

// pullMissing pulls, with the developer's own Docker login, each image the
// objects run that the local image store lacks. The dev cluster has no image
// pull secret: its kubelet uses the local store (OrbStack, Docker Desktop).
func pullMissing(objs []official.Object, dry bool) error {
	for _, img := range imagesOf(objs) {
		if _, err := sh.Run(sh.Cmd{Name: "docker", Args: []string{"image", "inspect", "--format", "{{.Id}}", img}}); err == nil {
			continue
		}
		if dry {
			ui.Sub("would pull %s", img)
			continue
		}
		ui.Step("pulling %s", img)
		if res, err := sh.Run(sh.Cmd{Name: "docker", Args: []string{"pull", "-q", img}, Timeout: 15 * time.Minute}); err != nil {
			return fmt.Errorf("docker pull %s: %w\n%s (sign in to the registry with your own account: docker login <registry>)", img, err, strings.TrimSpace(res.Stderr))
		}
	}
	return nil
}

// imagesOf lists the distinct container images of the Deployments in objs.
func imagesOf(objs []official.Object) []string {
	seen := map[string]bool{}
	var out []string
	for _, o := range objs {
		if o["kind"] != "Deployment" {
			continue
		}
		spec, _ := o["spec"].(map[string]any)
		tmpl, _ := spec["template"].(map[string]any)
		pod, _ := tmpl["spec"].(map[string]any)
		for _, list := range []string{"initContainers", "containers"} {
			cs, _ := pod[list].([]any)
			for _, c := range cs {
				m, _ := c.(map[string]any)
				if img, _ := m["image"].(string); img != "" && !seen[img] {
					seen[img] = true
					out = append(out, img)
				}
			}
		}
	}
	return out
}
