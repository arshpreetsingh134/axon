package deploy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"axon/internal/descriptor"
	"axon/internal/gen"
	"axon/internal/sh"
	"axon/internal/ui"
	y "axon/internal/yamlx"
)

func (s *Session) kargs(args ...string) []string {
	base := []string{}
	if s.KubeContext != "" {
		base = append(base, "--context", s.KubeContext)
	}
	base = append(base, "-n", s.Namespace)
	return append(base, args...)
}

func (s *Session) kubectl(stdin string, args ...string) (*sh.Result, error) {
	return sh.Run(sh.Cmd{Name: "kubectl", Args: s.kargs(args...), Stdin: stdin, Timeout: s.timeout() + time.Minute})
}

func (s *Session) kubeReachable() bool {
	if !sh.Has("kubectl") {
		return false
	}
	args := []string{"get", "ns", "--request-timeout=4s", "-o", "name"}
	if s.KubeContext != "" {
		args = append([]string{"--context", s.KubeContext}, args...)
	}
	_, err := sh.Run(sh.Cmd{Name: "kubectl", Args: args, Timeout: 8 * time.Second})
	return err == nil
}

func (s *Session) k8sHasWorkloads() bool {
	r, err := sh.Run(sh.Cmd{Name: "kubectl", Args: s.kargs("get", "deploy", "-o", "name", "--request-timeout=5s"), Timeout: 10 * time.Second})
	return err == nil && strings.TrimSpace(r.Stdout) != ""
}

func (s *Session) k8sPreflight() error {
	if !sh.Has("kubectl") {
		return fmt.Errorf("kubectl is not installed (TOOLCHAIN_KUBECTL_MISSING)")
	}
	if s.KubeContext == "" {
		cur, err := sh.Output("kubectl", "config", "current-context")
		if err != nil || cur == "" {
			return fmt.Errorf("no kube context (TOOLCHAIN_KUBE_CONTEXT_UNSET): set `context` in environments/%s.toml or pass --context", s.Env.Name)
		}
		s.KubeContext = cur
	}
	if !s.kubeReachable() {
		return fmt.Errorf("kube context %q is unreachable (TOOLCHAIN_KUBE_UNREACHABLE)", s.KubeContext)
	}
	return nil
}

// readClusterSecret reads one key back from the live app-env Secret (or, for a
// compat namespace deployed by mycel, its signing-key Secret).
func (s *Session) readClusterSecret(key string) string {
	if s.Target != "k8s" {
		return ""
	}
	try := func(secret, k string) string {
		r, err := s.kubectl("", "get", "secret", secret, "-o", "jsonpath={.data."+strings.ReplaceAll(k, ".", `\.`)+"}")
		if err != nil || strings.TrimSpace(r.Stdout) == "" {
			return ""
		}
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(r.Stdout))
		if err != nil {
			return ""
		}
		return string(b)
	}
	if v := try(s.M.L.K8s("app-env"), key); v != "" {
		return v
	}
	if key == "IAM_SIGNING_KEY" {
		return try(s.M.L.K8s("iam-signing-keys"), "key.pem")
	}
	return ""
}

func (s *Session) k8sInput(ips map[string]string) (*gen.Input, *gen.K8s) {
	in := s.Input()
	in.Target = "k8s"
	k := &gen.K8s{Namespace: s.Namespace, ServiceIPs: ips, Secrets: s.Secrets, InClusterPostgres: s.PGMode == "in-cluster", Replicas: s.Env.Replicas}
	return in, k
}

func docName(d any) (kind, name string) {
	m := d.(*y.M)
	kind, _ = m.Get("kind").(string)
	name, _ = m.Get("metadata").(*y.M).Get("name").(string)
	return
}

func component(d any) string {
	m := d.(*y.M)
	if l, ok := m.Get("metadata").(*y.M).Get("labels").(*y.M); ok {
		if c, ok := l.Get("axon.dev/component").(string); ok {
			return c
		}
	}
	return ""
}

func (s *Session) apply(docs []any, what string) error {
	if len(docs) == 0 {
		return nil
	}
	text, err := y.MarshalAll(docs)
	if err != nil {
		return err
	}
	if _, err := s.kubectl(text, "apply", "-f", "-"); err != nil {
		return fmt.Errorf("apply %s: %w", what, err)
	}
	return nil
}

type liveService struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		ClusterIP string `json:"clusterIP"`
	} `json:"spec"`
}

func (s *Session) liveServices() (map[string]string, error) {
	r, err := s.kubectl("", "get", "svc", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []liveService `json:"items"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &list); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, it := range list.Items {
		out[it.Metadata.Name] = it.Spec.ClusterIP
	}
	return out, nil
}

func (s *Session) upK8s() error {
	ns := y.New("apiVersion", "v1", "kind", "Namespace", "metadata", y.New("name", s.Namespace,
		"labels", y.New("app.kubernetes.io/managed-by", "axon", "axon.dev/cluster", descriptor.Slug(s.M.C.Name))))
	in, k := s.k8sInput(map[string]string{})
	svcs := gen.K8sServices(in, k)
	if s.DryRun {
		return s.planK8s(svcs)
	}
	if _, err := sh.Run(sh.Cmd{Name: "kubectl", Args: append(contextArgs(s.KubeContext), "apply", "-f", "-"), Stdin: mustYAML(ns)}); err != nil {
		return fmt.Errorf("namespace: %w", err)
	}
	// secrets: applied from memory, never written to disk
	labels := map[string]string{"app.kubernetes.io/managed-by": "axon", "axon.dev/cluster": descriptor.Slug(s.M.C.Name)}
	if _, err := s.kubectl(secretManifest(s.M.L.K8s("app-env"), s.Namespace, s.Secrets, labels), "apply", "-f", "-"); err != nil {
		return fmt.Errorf("secret: %w", err)
	}
	// registry credentials for private images (CI passes AXON_PULL_*)
	pullSecret, err := s.applyPullSecret(labels)
	if err != nil {
		return err
	}
	// the conventional Postgres credentials Secret, for tools that read it
	// (e.g. data-migration scripts: kubectl get secret postgres-credentials)
	pgCreds := map[string]string{"username": s.PG.User, "password": s.Secrets["POSTGRES_PASSWORD"]}
	if _, err := s.kubectl(secretManifest("postgres-credentials", s.Namespace, pgCreds, labels), "apply", "-f", "-"); err != nil {
		return fmt.Errorf("secret postgres-credentials: %w", err)
	}
	// phase 1: Services (recreate any whose clusterIP mode flips)
	live, err := s.liveServices()
	if err != nil {
		return err
	}
	var svcDocs []any
	for _, sv := range svcs {
		if ip, ok := live[sv.Name]; ok && (ip == "None") != sv.Headless {
			ui.Sub("recreating service %s (clusterIP mode changed)", sv.Name)
			if _, err := s.kubectl("", "delete", "svc", sv.Name, "--wait=true"); err != nil {
				return err
			}
		}
		svcDocs = append(svcDocs, sv.Doc)
	}
	ui.Step("services (%d)", len(svcDocs))
	if err := s.apply(svcDocs, "services"); err != nil {
		return err
	}
	ips, err := s.liveServices()
	if err != nil {
		return err
	}
	in, k = s.k8sInput(ips)
	docs := gen.K8sWorkloads(in, k)
	gen.AddPullSecret(docs, pullSecret)
	_ = s.writeManifest("services", svcDocs)
	_ = s.writeManifest("workloads", docs)
	var fabric, apps []any
	for _, d := range docs {
		switch component(d) {
		case "fabric", "rbac":
			fabric = append(fabric, d)
		default:
			apps = append(apps, d)
		}
	}
	// Postgres and the databases come first: Keycloak and Temporal need theirs at boot.
	var pgDocs, rest []any
	for _, d := range fabric {
		if _, name := docName(d); name == "postgres" {
			pgDocs = append(pgDocs, d)
		} else {
			rest = append(rest, d)
		}
	}
	if len(pgDocs) > 0 {
		ui.Step("postgres (in-cluster)")
		if err := s.apply(pgDocs, "postgres"); err != nil {
			return err
		}
	}
	if err := s.ensureDatabases(); err != nil {
		return err
	}
	ui.Step("fabric")
	if err := s.apply(rest, "fabric"); err != nil {
		return err
	}
	fdeps, fsts := gen.WorkloadNames(rest)
	if err := s.waitRollouts(fdeps, fsts, false); err != nil {
		return err
	}
	if err := s.provisionKeycloak(); err != nil {
		return err
	}
	if err := s.runHooks("post_fabric"); err != nil {
		return err
	}
	ui.Step("apps")
	if len(s.O.Apps) > 0 {
		apps = filterApps(apps, s.O.Apps)
	}
	if err := s.apply(apps, "apps"); err != nil {
		return err
	}
	adeps, _ := gen.WorkloadNames(apps)
	if err := s.waitRollouts(adeps, nil, !s.O.KeepOnFailure); err != nil {
		return err
	}
	return s.seedIAM()
}

func filterApps(docs []any, names []string) []any {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var out []any
	for _, d := range docs {
		m := d.(*y.M)
		tm := m.Get("spec")
		app := ""
		if sp, ok := tm.(*y.M); ok {
			if t, ok := sp.Get("template").(*y.M); ok {
				if md, ok := t.Get("metadata").(*y.M); ok {
					if an, ok := md.Get("annotations").(*y.M); ok {
						app, _ = an.Get("axon.dev/app").(string)
					}
				}
			}
		}
		if app == "" || want[app] {
			out = append(out, d)
		}
	}
	return out
}

func contextArgs(ctx string) []string {
	if ctx == "" {
		return nil
	}
	return []string{"--context", ctx}
}

func mustYAML(v any) string {
	s, _ := y.Marshal(v)
	return s
}

// waitRollouts waits for every workload in parallel; failed ones are reported with
// pod status and recent logs, and (when undo) rolled back to the previous revision.
func (s *Session) waitRollouts(deploys, statefulsets []string, undo bool) error {
	type item struct{ kind, name string }
	var items []item
	for _, d := range deploys {
		items = append(items, item{"deployment", d})
	}
	for _, st := range statefulsets {
		items = append(items, item{"statefulset", st})
	}
	if len(items) == 0 {
		return nil
	}
	s.restartStuck(deploys)
	timeout := fmt.Sprintf("--timeout=%ds", int(s.timeout().Seconds()))
	var mu sync.Mutex
	var failed []string
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, it := range items {
		it := it
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if _, err := s.kubectl("", "rollout", "status", it.kind+"/"+it.name, timeout); err != nil {
				mu.Lock()
				failed = append(failed, it.kind+"/"+it.name)
				mu.Unlock()
				return
			}
			ui.Debug("%s/%s ready", it.kind, it.name)
		}()
	}
	wg.Wait()
	if len(failed) == 0 {
		ui.OK("%d workload(s) ready", len(items))
		return nil
	}
	sort.Strings(failed)
	for _, f := range failed {
		name := strings.SplitN(f, "/", 2)[1]
		ui.Fail("%s did not become ready", f)
		if r, err := s.kubectl("", "get", "pods", "-l", "app="+name, "--no-headers"); err == nil {
			ui.Sub("%s", strings.TrimSpace(r.Stdout))
		}
		if r, err := s.kubectl("", "logs", f, "--tail=12", "--all-containers"); err == nil {
			ui.Sub("last logs:\n%s", indentLines(r.Stdout, "    "))
		}
		if undo && strings.HasPrefix(f, "deployment/") {
			if _, err := s.kubectl("", "rollout", "undo", f); err == nil {
				ui.Note("rolled %s back to its previous revision (pass --keep-on-failure to keep the new one)", f)
			}
		}
	}
	return fmt.Errorf("%d workload(s) failed to roll out: %s", len(failed), strings.Join(failed, ", "))
}

// restartStuck restarts deployments left broken by an earlier run — pods in
// CrashLoopBackOff/Error, or past their progress deadline — so they retry now that
// their prerequisites (databases, secrets, config) are in place.
func (s *Session) restartStuck(deploys []string) {
	r, err := s.kubectl("", "get", "deploy,pods", "-o", "json")
	if err != nil {
		return
	}
	var list struct {
		Items []struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Reason string `json:"reason"`
				} `json:"conditions"`
				ContainerStatuses []struct {
					State struct {
						Waiting *struct {
							Reason string `json:"reason"`
						} `json:"waiting"`
					} `json:"state"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(r.Stdout), &list) != nil {
		return
	}
	want := map[string]bool{}
	for _, d := range deploys {
		want[d] = true
	}
	stuck := map[string]bool{}
	for _, it := range list.Items {
		switch it.Kind {
		case "Deployment":
			for _, c := range it.Status.Conditions {
				if c.Type == "Progressing" && c.Reason == "ProgressDeadlineExceeded" {
					stuck[it.Metadata.Name] = true
				}
			}
		case "Pod":
			for _, cs := range it.Status.ContainerStatuses {
				if w := cs.State.Waiting; w != nil && (w.Reason == "CrashLoopBackOff" || w.Reason == "Error" || w.Reason == "CreateContainerConfigError") {
					stuck[it.Metadata.Labels["app"]] = true
				}
			}
		}
	}
	var names []string
	for n := range stuck {
		if want[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if _, err := s.kubectl("", "rollout", "restart", "deployment/"+n); err == nil {
			ui.Sub("restarted %s (left failing by an earlier run)", n)
		}
	}
}

func (s *Session) downK8s(volumes bool) error {
	sel := "app.kubernetes.io/managed-by=axon,axon.dev/cluster=" + descriptor.Slug(s.M.C.Name)
	kinds := "deployment,statefulset,service,configmap,serviceaccount,role,rolebinding"
	if volumes {
		kinds += ",secret,persistentvolumeclaim"
	}
	if s.DryRun {
		ui.Sub("would delete %s labelled %s in namespace %s", kinds, sel, s.Namespace)
		if !volumes {
			ui.Sub("would keep secrets and persistent volume claims (pass --volumes to delete them)")
		}
		return nil
	}
	if _, err := s.kubectl("", "delete", kinds, "-l", sel, "--ignore-not-found", "--wait=false"); err != nil {
		return err
	}
	ui.Sub("deleted axon-managed objects in %s", s.Namespace)
	if volumes {
		pgc := descriptor.Slug(s.M.C.Name) + "-postgres"
		if containerExists(pgc) {
			sh.Run(sh.Cmd{Name: "docker", Args: []string{"rm", "-f", pgc}})
			sh.Run(sh.Cmd{Name: "docker", Args: []string{"volume", "rm", pgc + "-data"}})
			ui.Sub("removed postgres container %s and its volume", pgc)
		}
	}
	return nil
}

// planK8s renders everything against the live Service IPs and has the API server
// validate it (server-side dry run): schema, immutable fields, selectors. Nothing
// is changed; the rendered manifests (no secrets) are written for inspection.
func (s *Session) planK8s(svcs []gen.ServiceSpec) error {
	live, _ := s.liveServices()
	if live == nil {
		live = map[string]string{}
	}
	ips := map[string]string{}
	var svcDocs []any
	recreate := 0
	for _, sv := range svcs {
		ip, ok := live[sv.Name]
		switch {
		case ok && (ip == "None") != sv.Headless:
			recreate++
			ui.Sub("would recreate service %s (clusterIP mode changes)", sv.Name)
		default:
			svcDocs = append(svcDocs, sv.Doc)
		}
		if ok {
			ips[sv.Name] = ip
		} else if !sv.Headless {
			ips[sv.Name] = "10.0.0.1" // placeholder until the Service exists
		}
	}
	in, k := s.k8sInput(ips)
	docs := gen.K8sWorkloads(in, k)
	deps, sts := gen.WorkloadNames(docs)
	ui.Sub("would apply namespace %s, secret %s (%d keys), %d services, %d deployments, %d statefulsets",
		s.Namespace, s.M.L.K8s("app-env"), len(s.Secrets), len(svcs), len(deps), len(sts))
	ui.Sub("postgres: %s (%s:%d)", s.PGMode, s.PG.Host, s.PG.Port)
	_ = s.writeManifest("services", svcDocs)
	_ = s.writeManifest("workloads", docs)
	if _, err := s.kubectl("", "get", "ns", s.Namespace, "-o", "name"); err != nil {
		ui.Note("namespace %s does not exist yet; skipping the server-side dry run", s.Namespace)
		return nil
	}
	all := append(append([]any{}, svcDocs...), docs...)
	text, err := y.MarshalAll(all)
	if err != nil {
		return err
	}
	if _, err := s.kubectl(text, "apply", "--dry-run=server", "-f", "-"); err != nil {
		return fmt.Errorf("the API server rejected the plan (nothing was changed): %w", err)
	}
	ui.OK("API server accepted all %d objects (server-side dry run; manifests in %s/k8s/axon/)", len(all), s.M.L.Infra)
	return nil
}

// applyPullSecret creates the image pull Secret from AXON_PULL_REGISTRY,
// AXON_PULL_USERNAME and AXON_PULL_PASSWORD; it returns "" when they are unset.
func (s *Session) applyPullSecret(labels map[string]string) (string, error) {
	reg, user, pass := os.Getenv("AXON_PULL_REGISTRY"), os.Getenv("AXON_PULL_USERNAME"), os.Getenv("AXON_PULL_PASSWORD")
	if reg == "" || user == "" || pass == "" {
		if reg != "" || user != "" || pass != "" {
			ui.Warn("AXON_PULL_REGISTRY, AXON_PULL_USERNAME and AXON_PULL_PASSWORD must all be set; not creating an image pull secret")
		}
		return "", nil
	}
	name := s.M.L.K8s("registry")
	cfg, err := json.Marshal(map[string]any{"auths": map[string]any{reg: map[string]string{
		"username": user, "password": pass, "auth": base64.StdEncoding.EncodeToString([]byte(user + ":" + pass)),
	}}})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\n  namespace: %s\n  labels:\n", name, s.Namespace)
	for _, k := range sortedKeys(labels) {
		fmt.Fprintf(&b, "    %s: %q\n", k, labels[k])
	}
	fmt.Fprintf(&b, "type: kubernetes.io/dockerconfigjson\ndata:\n  .dockerconfigjson: %s\n", base64.StdEncoding.EncodeToString(cfg))
	if _, err := s.kubectl(b.String(), "apply", "-f", "-"); err != nil {
		return "", fmt.Errorf("image pull secret: %w", err)
	}
	ui.Sub("image pull secret %s (%s)", name, reg)
	return name, nil
}
