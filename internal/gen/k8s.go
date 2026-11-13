package gen

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"axon/internal/descriptor"
	y "axon/internal/yamlx"
)

// K8s holds Kubernetes-specific generation inputs.
type K8s struct {
	Namespace string
	// ServiceIPs maps Service name → ClusterIP (filled after Services exist).
	ServiceIPs map[string]string
	// Secrets are the resolved values of the app-env Secret (used only for
	// change-detection hashes; never rendered).
	Secrets map[string]string
	// InClusterPostgres renders a Postgres StatefulSet.
	InClusterPostgres bool
	Replicas          int
}

// Labels shared by every object axon manages.
func (in *Input) labels(name, component string) *y.M {
	return y.New(
		"app", name,
		"app.kubernetes.io/managed-by", "axon",
		"axon.dev/cluster", descriptor.Slug(in.M.C.Name),
		"axon.dev/component", component,
	)
}

func meta(name string, labels *y.M, annotations *y.M) *y.M {
	m := y.New("name", name, "labels", labels)
	if annotations != nil && annotations.Len() > 0 {
		m.Set("annotations", annotations)
	}
	return m
}

// AppEnvSecret is the Secret holding every ${VAR} value.
func (in *Input) AppEnvSecret() string { return in.M.L.K8s("app-env") }

// k8sEnv converts a compose-style env map to a Kubernetes env list. "${X}" values
// become secretKeyRefs; embedded references become $(X) dependent variables.
func (in *Input) k8sEnv(env map[string]string, k *K8s) ([]any, string) {
	secret := in.AppEnvSecret()
	var head, body []any
	defined := map[string]bool{}
	hash := sha256.New()
	ref := func(name string) {
		if defined[name] {
			return
		}
		defined[name] = true
		head = append(head, y.New("name", name, "valueFrom", y.New("secretKeyRef", y.New("name", secret, "key", name, "optional", true))))
		fmt.Fprintf(hash, "%s=%s\n", name, k.Secrets[name])
	}
	keys := make([]string, 0, len(env))
	for kk := range env {
		keys = append(keys, kk)
	}
	sort.Strings(keys)
	// pure refs read their Secret key directly (KEY ← secret[VAR]), so tools that
	// inspect the Deployment see a secretKeyRef, not a "$(VAR)" indirection
	for _, kk := range keys {
		v := env[kk]
		if name, ok := pureRef(v); ok {
			if name == kk {
				ref(kk)
				continue
			}
			head = append(head, y.New("name", kk, "valueFrom", y.New("secretKeyRef", y.New("name", secret, "key", name, "optional", true))))
			fmt.Fprintf(hash, "%s<-%s=%s\n", kk, name, k.Secrets[name])
		}
	}
	for _, kk := range keys {
		v := env[kk]
		if _, ok := pureRef(v); ok {
			continue
		}
		rs := refs(v)
		for _, r := range rs {
			ref(r)
		}
		val := strings.ReplaceAll(v, "$(", "$$(")
		for _, r := range rs {
			val = strings.ReplaceAll(val, "${"+r+"}", "$("+r+")")
		}
		body = append(body, y.New("name", kk, "value", val))
		fmt.Fprintf(hash, "%s=%s\n", kk, val)
	}
	return append(head, body...), hex.EncodeToString(hash.Sum(nil))[:16]
}

func pureRef(v string) (string, bool) {
	if strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}") && strings.Count(v, "${") == 1 {
		return v[2 : len(v)-1], true
	}
	return "", false
}

var appSecurity = y.New(
	"allowPrivilegeEscalation", false,
	"capabilities", y.New("drop", y.L("ALL")),
	"readOnlyRootFilesystem", true,
	"runAsNonRoot", true,
	"runAsUser", 65532,
	"seccompProfile", y.New("type", "RuntimeDefault"),
)

func appResources() *y.M {
	return y.New("limits", y.New("memory", "512Mi"), "requests", y.New("cpu", "10m", "memory", "32Mi"))
}

// hostAliases maps every front-door host to Traefik so in-cluster callers reach
// <host>.<domain>:<port> (only valid DNS names may appear in hostAliases).
func (in *Input) hostAliases(k *K8s) []any {
	ip := k.ServiceIPs["traefik"]
	if ip == "" || ip == "None" {
		return nil
	}
	var hs []any
	for _, h := range Hosts(in.M, in.M.C.Deploy.Products) {
		hs = append(hs, h)
	}
	if len(hs) == 0 {
		return nil
	}
	return []any{y.New("ip", ip, "hostnames", hs)}
}

// EtcHosts renders the /etc/hosts file mounted into app cores: module tokens are
// not always DNS names (mycel_iam), so cores resolve each token to its Service IP
// here, plus every front-door host to Traefik.
func (in *Input) EtcHosts(k *K8s) string {
	var b strings.Builder
	b.WriteString("127.0.0.1 localhost\n::1 localhost\n")
	for _, tok := range in.M.Tokens() {
		if ip := k.ServiceIPs[descriptor.DNS(tok)]; ip != "" && ip != "None" {
			fmt.Fprintf(&b, "%s %s\n", ip, tok)
		}
	}
	if ip := k.ServiceIPs["traefik"]; ip != "" && ip != "None" {
		hosts := Hosts(in.M, in.M.C.Deploy.Products)
		if len(hosts) > 0 {
			fmt.Fprintf(&b, "%s %s\n", ip, strings.Join(hosts, " "))
		}
	}
	return b.String()
}

func deployment(in *Input, name, component string, replicas int, podSpec *y.M, annotations *y.M) *y.M {
	tmeta := y.New("labels", in.labels(name, component))
	if annotations != nil && annotations.Len() > 0 {
		tmeta.Set("annotations", annotations)
	}
	return y.New(
		"apiVersion", "apps/v1", "kind", "Deployment",
		"metadata", meta(name, in.labels(name, component), nil),
		"spec", y.New(
			"replicas", replicas,
			"revisionHistoryLimit", 3,
			"selector", y.New("matchLabels", y.New("app", name)),
			"strategy", y.New("type", "RollingUpdate", "rollingUpdate", y.New("maxSurge", 1, "maxUnavailable", 0)),
			"template", y.New("metadata", tmeta, "spec", podSpec),
		),
	)
}

// Service renders a Service; headless when clusterIP is "None".
func (in *Input) service(name, component string, headless bool, ports ...[2]any) *y.M {
	var ps []any
	for _, p := range ports {
		ps = append(ps, y.New("name", p[0], "port", p[1], "targetPort", p[1]))
	}
	spec := y.New("selector", y.New("app", name), "ports", ps)
	if headless {
		spec.Set("clusterIP", "None")
	}
	return y.New("apiVersion", "v1", "kind", "Service", "metadata", meta(name, in.labels(name, component), nil), "spec", spec)
}

// ServiceSpec is a Service axon wants, used to pre-create Services (phase 1) and
// to detect clusterIP changes that need delete-and-recreate.
type ServiceSpec struct {
	Name     string
	Headless bool
	Doc      *y.M
}

// K8sServices returns every Service of the cluster (phase 1).
func K8sServices(in *Input, k *K8s) []ServiceSpec {
	c := in.M.C
	var out []ServiceSpec
	add := func(name, comp string, headless bool, ports ...[2]any) {
		out = append(out, ServiceSpec{Name: name, Headless: headless, Doc: in.service(name, comp, headless, ports...)})
	}
	http := c.HTTPPort()
	tr := in.service("traefik", "fabric", false, [2]any{"web", http})
	tr.Get("spec").(*y.M).Set("type", "LoadBalancer")
	tr.Get("spec").(*y.M).Set("ports", y.L(y.New("name", "web", "port", http, "targetPort", 8080)))
	out = append(out, ServiceSpec{Name: "traefik", Doc: tr})
	if k.InClusterPostgres {
		add("postgres", "fabric", false, [2]any{"pg", 5432})
	}
	if c.Fabric.Temporal != nil {
		add("temporal", "fabric", false, [2]any{"grpc", 7233})
		add("temporal-ui", "fabric", true, [2]any{"http", 8080})
	}
	if c.Fabric.Keycloak != nil {
		add("keycloak", "fabric", true, [2]any{"http", 8080})
	}
	if c.Fabric.Tracing != nil {
		add("tempo", "fabric", false, [2]any{"otlp", 4317}, [2]any{"http", 3200})
		add("grafana", "fabric", true, [2]any{"http", 3000})
	}
	if c.Fabric.Clickhouse != nil {
		add("clickhouse", "fabric", true, [2]any{"native", 9000}, [2]any{"http", 8123})
	}
	for _, a := range in.M.Apps {
		if a.Problem != "" && a.Decl.Cored() {
			continue
		}
		d := a.Decl
		if d.Cored() {
			for _, mod := range a.Modules {
				ports := [][2]any{{"grpc", 7373}}
				if mod.Token == "workflow_manager" {
					ports = append(ports, [2]any{"register", 7380})
				}
				add(descriptor.DNS(mod.Token), "module", false, ports...)
			}
			add(descriptor.CoreName(d.Name), "core", false, [2]any{"http", 9080})
		} else {
			port := d.Port
			if port == 0 {
				port = 8090
			}
			add(descriptor.CoreName(d.Name), "project", true, [2]any{"http", port})
		}
	}
	if c.Deploy.Products {
		for _, n := range sortedProducts(c) {
			p := c.Product[n]
			if p.Image != "" {
				add(descriptor.CoreName(productApp(n, p)), "product", false, [2]any{"http", 9080})
			}
		}
	}
	return out
}

func podSpec(sa string, automount bool, containers []any, extra *y.M) *y.M {
	s := y.New("serviceAccountName", sa, "automountServiceAccountToken", automount,
		"terminationGracePeriodSeconds", 30, "containers", containers)
	if extra != nil {
		for _, k := range []string{"hostAliases", "volumes", "securityContext", "initContainers"} {
			if v := extra.Get(k); v != nil {
				s.Set(k, v)
			}
		}
	}
	return s
}

// K8sWorkloads renders RBAC, fabric and app workloads (phase 2). Services come
// from K8sServices.
func K8sWorkloads(in *Input, k *K8s) []any {
	m := in.M
	c := m.C
	var docs []any
	reader := m.L.K8s("namespace-reader")
	workloadSA := m.L.K8s("workload")
	docs = append(docs,
		y.New("apiVersion", "v1", "kind", "ServiceAccount", "metadata", meta(workloadSA, in.labels(workloadSA, "rbac"), nil), "automountServiceAccountToken", false),
		y.New("apiVersion", "v1", "kind", "ServiceAccount", "metadata", meta(reader, in.labels(reader, "rbac"), nil)),
		y.New("apiVersion", "rbac.authorization.k8s.io/v1", "kind", "Role", "metadata", meta(reader, in.labels(reader, "rbac"), nil),
			"rules", y.L(y.New("apiGroups", y.L("", "apps"), "resources", y.L("pods", "services", "endpoints", "deployments", "configmaps"), "verbs", y.L("get", "list", "watch")))),
		y.New("apiVersion", "rbac.authorization.k8s.io/v1", "kind", "RoleBinding", "metadata", meta(reader, in.labels(reader, "rbac"), nil),
			"roleRef", y.New("apiGroup", "rbac.authorization.k8s.io", "kind", "Role", "name", reader),
			"subjects", y.L(y.New("kind", "ServiceAccount", "name", reader, "namespace", k.Namespace))),
	)
	docs = append(docs, in.k8sFabric(k)...)

	aliases := in.hostAliases(k)
	extra := y.New("volumes", y.L(y.New("name", "tmp", "emptyDir", y.New())))
	if len(aliases) > 0 {
		extra.Set("hostAliases", aliases)
	}
	tmpMount := y.L(y.New("name", "tmp", "mountPath", "/tmp"))
	hostsName := m.L.K8s("etc-hosts")
	hosts := in.EtcHosts(k)
	docs = append(docs, y.New("apiVersion", "v1", "kind", "ConfigMap",
		"metadata", meta(hostsName, in.labels(hostsName, "app-config"), nil), "data", y.New("hosts", hosts)))
	coreExtra := y.New("volumes", y.L(y.New("name", "tmp", "emptyDir", y.New()),
		y.New("name", "etc-hosts", "configMap", y.New("name", hostsName))))
	coreMounts := y.L(y.New("name", "tmp", "mountPath", "/tmp"), y.New("name", "etc-hosts", "mountPath", "/etc/hosts", "subPath", "hosts"))
	replicas := k.Replicas
	if replicas == 0 {
		replicas = 1
	}
	pull := "IfNotPresent"
	for _, a := range m.Apps {
		d := a.Decl
		if a.Problem != "" && d.Cored() {
			continue
		}
		rep := replicas
		if d.Replicas > 0 {
			rep = d.Replicas
		}
		if d.Cored() {
			for _, mod := range a.Modules {
				name := descriptor.DNS(mod.Token)
				img := mod.Image
				if img == "" {
					img = in.Image(descriptor.ServiceImage(c.Name, d.Name, mod.Token))
				} else {
					img = in.subst(img)
				}
				env, h := in.k8sEnv(in.ModuleEnv(a, mod), k)
				ports := y.L(y.New("containerPort", 7373, "name", "grpc"))
				if mod.Token == "workflow_manager" {
					ports = append(ports, y.New("containerPort", 7380, "name", "register"))
				}
				ctr := y.New("name", name, "image", img, "imagePullPolicy", pull, "env", env, "ports", ports,
					"resources", appResources(), "securityContext", appSecurity, "volumeMounts", tmpMount,
					"startupProbe", y.New("tcpSocket", y.New("port", 7373), "periodSeconds", 3, "failureThreshold", 40),
					"readinessProbe", y.New("tcpSocket", y.New("port", 7373), "periodSeconds", 5, "failureThreshold", 2),
					"livenessProbe", y.New("tcpSocket", y.New("port", 7373), "periodSeconds", 30, "failureThreshold", 6),
					"lifecycle", y.New("preStop", y.New("exec", y.New("command", y.L("sleep", "3")))),
				)
				docs = append(docs, deployment(in, name, "module", rep, podSpec(reader, true, y.L(ctr), extra),
					y.New("axon.dev/config-hash", h, "axon.dev/app", d.Name)))
			}
			name := descriptor.CoreName(d.Name)
			env, h := in.k8sEnv(in.CoreEnv(a), k)
			ctr := y.New("name", "core", "image", in.Image(descriptor.ServiceImage(c.Name, d.Name, "core")), "imagePullPolicy", pull,
				"env", env, "ports", y.L(y.New("containerPort", 9080, "name", "http")),
				"resources", appResources(), "securityContext", appSecurity, "volumeMounts", coreMounts,
				"startupProbe", y.New("httpGet", y.New("path", "/healthz", "port", 9080), "periodSeconds", 3, "failureThreshold", 40),
				"readinessProbe", y.New("httpGet", y.New("path", "/healthz", "port", 9080), "periodSeconds", 5, "failureThreshold", 2),
				"lifecycle", y.New("preStop", y.New("exec", y.New("command", y.L("sleep", "3")))),
			)
			docs = append(docs, deployment(in, name, "core", rep, podSpec(workloadSA, false, y.L(ctr), coreExtra),
				y.New("axon.dev/config-hash", h, "axon.dev/etc-hosts-hash", short(hosts), "axon.dev/app", d.Name)))
			continue
		}
		name := descriptor.CoreName(d.Name)
		img := d.Image
		if img == "" {
			img = in.Image(descriptor.WorkerImage(c.Name, d.Name))
		} else {
			img = in.ResolveRef(img)
		}
		env, h := in.k8sEnv(in.WorkerEnv(a), k)
		ctr := y.New("name", name, "image", img, "imagePullPolicy", pull, "env", env,
			"resources", appResources(), "securityContext", appSecurity, "volumeMounts", tmpMount)
		if d.Image != "" || RouteHost(c, d) != "" {
			port := d.Port
			if port == 0 {
				port = 8090
			}
			ctr.Set("ports", y.L(y.New("containerPort", port, "name", "http")))
			ctr.Set("readinessProbe", y.New("tcpSocket", y.New("port", port), "periodSeconds", 5))
		}
		if d.Command != "" {
			ctr.Set("command", y.L("sh", "-c", d.Command))
		}
		docs = append(docs, deployment(in, name, "project", rep, podSpec(workloadSA, false, y.L(ctr), extra),
			y.New("axon.dev/config-hash", h, "axon.dev/app", d.Name)))
	}
	if c.Deploy.Products {
		for _, n := range sortedProducts(c) {
			p := c.Product[n]
			if p.Image == "" {
				continue
			}
			name := descriptor.CoreName(productApp(n, p))
			ctr := y.New("name", "core", "image", in.subst(p.Image), "imagePullPolicy", pull,
				"ports", y.L(y.New("containerPort", 9080)), "resources", appResources())
			docs = append(docs, deployment(in, name, "product", 1, podSpec(workloadSA, false, y.L(ctr), extra), nil))
		}
	}
	return docs
}

func configMap(in *Input, name string, data *y.M) *y.M {
	return y.New("apiVersion", "v1", "kind", "ConfigMap", "metadata", meta(name, in.labels(name, "fabric"), nil), "data", data)
}

func short(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:16]
}

func secretEnv(secret, key string) *y.M {
	return y.New("valueFrom", y.New("secretKeyRef", y.New("name", secret, "key", key)))
}

func envVar(name string, v any) *y.M {
	if m, ok := v.(*y.M); ok {
		out := y.New("name", name)
		out.Set("valueFrom", m.Get("valueFrom"))
		return out
	}
	return y.New("name", name, "value", fmt.Sprint(v))
}

func (in *Input) k8sFabric(k *K8s) []any {
	c := in.M.C
	secret := in.AppEnvSecret()
	user := PGUser(in.M)
	var docs []any
	fabricSA := in.M.L.K8s("workload")
	ps := func(containers []any, extra *y.M) *y.M { return podSpec(fabricSA, false, containers, extra) }

	// front door: Traefik with the file provider (no cluster-scoped RBAC needed)
	routes := RoutesYAML(Routes(in, c.Deploy.Products), "")
	docs = append(docs, configMap(in, "traefik-routes", y.New("routes.yml", routes)))
	trCtr := y.New("name", "traefik", "image", in.subst(or(traefikImage(c), ImgTraefik)),
		"args", y.L("--entrypoints.web.address=:8080", "--providers.file.directory=/etc/traefik/dynamic",
			"--providers.file.watch=true", "--ping=true", "--ping.entrypoint=web", "--api.dashboard=false", "--log.level=WARN"),
		"ports", y.L(y.New("containerPort", 8080, "name", "web")),
		"readinessProbe", y.New("httpGet", y.New("path", "/ping", "port", 8080), "periodSeconds", 5),
		"volumeMounts", y.L(y.New("name", "routes", "mountPath", "/etc/traefik/dynamic")),
		"resources", y.New("limits", y.New("memory", "128Mi"), "requests", y.New("cpu", "25m", "memory", "64Mi")),
	)
	docs = append(docs, deployment(in, "traefik", "fabric", 1,
		ps(y.L(trCtr), y.New("volumes", y.L(y.New("name", "routes", "configMap", y.New("name", "traefik-routes"))))),
		y.New("axon.dev/config-hash", short(routes))))

	if k.InClusterPostgres {
		pg := c.Fabric.Postgres
		if pg == nil {
			pg = &descriptor.Postgres{}
		}
		maxConn := pg.MaxConnections
		if maxConn == 0 {
			maxConn = 300
		}
		ctr := y.New("name", "postgres", "image", in.subst(or(pg.Image, ImgPostgres)),
			"args", y.L("-c", fmt.Sprintf("max_connections=%d", maxConn)),
			"env", y.L(envVar("POSTGRES_USER", user), envVar("POSTGRES_DB", "postgres"),
				envVar("POSTGRES_PASSWORD", secretEnv(secret, "POSTGRES_PASSWORD")),
				envVar("PGDATA", "/var/lib/postgresql/data/pgdata")),
			"ports", y.L(y.New("containerPort", 5432)),
			"readinessProbe", y.New("exec", y.New("command", y.L("pg_isready", "-U", user, "-d", "postgres")), "periodSeconds", 5),
			"volumeMounts", y.L(y.New("name", "data", "mountPath", "/var/lib/postgresql/data")),
		)
		docs = append(docs, y.New("apiVersion", "apps/v1", "kind", "StatefulSet",
			"metadata", meta("postgres", in.labels("postgres", "fabric"), nil),
			"spec", y.New("serviceName", "postgres", "replicas", 1,
				"selector", y.New("matchLabels", y.New("app", "postgres")),
				"template", y.New("metadata", y.New("labels", in.labels("postgres", "fabric")), "spec", ps(y.L(ctr), nil)),
				"volumeClaimTemplates", y.L(y.New("metadata", y.New("name", "data"),
					"spec", y.New("accessModes", y.L("ReadWriteOnce"), "resources", y.New("requests", y.New("storage", storageSize(in)))))),
			)))
	}

	if t := c.Fabric.Temporal; t != nil {
		shards := t.Shards
		if shards == 0 {
			shards = 4
		}
		dyn := "limit.maxIDLength:\n  - value: 255\n    constraints: {}\n"
		docs = append(docs, configMap(in, "temporal-dynamicconfig", y.New("axon.yaml", dyn)))
		ctr := y.New("name", "temporal", "image", in.subst(or(t.Image, ImgTemporal)),
			"env", y.L(envVar("DB", "postgres12"), envVar("DB_PORT", fmt.Sprint(in.PG.Port)), envVar("POSTGRES_SEEDS", in.PG.Host),
				envVar("POSTGRES_USER", in.PG.User), envVar("POSTGRES_PWD", secretEnv(secret, "POSTGRES_PASSWORD")),
				envVar("NUM_HISTORY_SHARDS", fmt.Sprint(shards)), envVar("BIND_ON_IP", "0.0.0.0"),
				envVar("TEMPORAL_BROADCAST_ADDRESS", "$(POD_IP)"),
				envVar("DYNAMIC_CONFIG_FILE_PATH", "config/dynamicconfig/axon.yaml")),
			"ports", y.L(y.New("containerPort", 7233)),
			"readinessProbe", y.New("tcpSocket", y.New("port", 7233), "periodSeconds", 5, "initialDelaySeconds", 10),
			"volumeMounts", y.L(y.New("name", "dyn", "mountPath", "/etc/temporal/config/dynamicconfig")),
			"resources", y.New("limits", y.New("memory", "1Gi"), "requests", y.New("cpu", "100m", "memory", "256Mi")),
		)
		// POD_IP must precede its use
		envs := ctr.Get("env").([]any)
		ctr.Set("env", append([]any{y.New("name", "POD_IP", "valueFrom", y.New("fieldRef", y.New("fieldPath", "status.podIP")))}, envs...))
		docs = append(docs, deployment(in, "temporal", "fabric", 1,
			ps(y.L(ctr), y.New("volumes", y.L(y.New("name", "dyn", "configMap", y.New("name", "temporal-dynamicconfig"))))), nil))
		ui := y.New("name", "temporal-ui", "image", in.subst(or(t.UIImage, ImgTemporalUI)),
			"env", y.L(envVar("TEMPORAL_ADDRESS", "temporal:7233"), envVar("TEMPORAL_CORS_ORIGINS", c.FrontDoor("temporal"))),
			"ports", y.L(y.New("containerPort", 8080)))
		docs = append(docs, deployment(in, "temporal-ui", "fabric", 1, ps(y.L(ui), nil), nil))
	}

	if kc := c.Fabric.Keycloak; kc != nil {
		ctr := y.New("name", "keycloak", "image", in.subst(or(kc.Image, ImgKeycloak)),
			"args", y.L("start-dev", "--http-port=8080"),
			"env", y.L(envVar("KEYCLOAK_ADMIN", "admin"), envVar("KEYCLOAK_ADMIN_PASSWORD", secretEnv(secret, "KEYCLOAK_ADMIN_PASSWORD")),
				envVar("KC_BOOTSTRAP_ADMIN_USERNAME", "admin"), envVar("KC_BOOTSTRAP_ADMIN_PASSWORD", secretEnv(secret, "KEYCLOAK_ADMIN_PASSWORD")),
				envVar("KC_DB", "postgres"), envVar("KC_DB_URL", fmt.Sprintf("jdbc:postgresql://%s:%d/keycloak", in.PG.Host, in.PG.Port)),
				envVar("KC_DB_USERNAME", in.PG.User), envVar("KC_DB_PASSWORD", secretEnv(secret, "POSTGRES_PASSWORD")),
				envVar("KC_HOSTNAME", c.FrontDoor("keycloak")), envVar("KC_HTTP_ENABLED", "true"),
				envVar("KC_HEALTH_ENABLED", "true"), envVar("KC_PROXY_HEADERS", "xforwarded")),
			"ports", y.L(y.New("containerPort", 8080)),
			"readinessProbe", y.New("tcpSocket", y.New("port", 8080), "periodSeconds", 5, "initialDelaySeconds", 10),
			"resources", y.New("limits", y.New("memory", "1Gi"), "requests", y.New("cpu", "100m", "memory", "384Mi")),
		)
		extra := y.New()
		if al := in.hostAliases(k); len(al) > 0 {
			extra.Set("hostAliases", al)
		}
		docs = append(docs, deployment(in, "keycloak", "fabric", 1, ps(y.L(ctr), extra), nil))
	}

	if tr := c.Fabric.Tracing; tr != nil {
		docs = append(docs, configMap(in, "tempo-config", y.New("tempo.yaml", TempoConfig)))
		tempo := y.New("name", "tempo", "image", in.subst(or(tr.TempoImage, ImgTempo)),
			"args", y.L("-config.file=/etc/tempo/tempo.yaml"),
			"ports", y.L(y.New("containerPort", 4317), y.New("containerPort", 3200)),
			"volumeMounts", y.L(y.New("name", "config", "mountPath", "/etc/tempo"), y.New("name", "data", "mountPath", "/var/tempo")),
			"resources", y.New("limits", y.New("memory", "512Mi"), "requests", y.New("cpu", "25m", "memory", "64Mi")),
		)
		docs = append(docs, deployment(in, "tempo", "fabric", 1, ps(y.L(tempo), y.New("volumes", y.L(
			y.New("name", "config", "configMap", y.New("name", "tempo-config")),
			y.New("name", "data", "emptyDir", y.New()),
		))), nil))
		docs = append(docs, configMap(in, "grafana-datasources", y.New("tempo.yaml", GrafanaDatasources)))
		graf := y.New("name", "grafana", "image", in.subst(or(tr.GrafanaImage, ImgGrafana)),
			"env", y.L(envVar("GF_AUTH_ANONYMOUS_ENABLED", "true"), envVar("GF_AUTH_ANONYMOUS_ORG_ROLE", "Admin"),
				envVar("GF_AUTH_DISABLE_LOGIN_FORM", "true"), envVar("GF_SERVER_ROOT_URL", c.FrontDoor("grafana"))),
			"ports", y.L(y.New("containerPort", 3000)),
			"volumeMounts", y.L(y.New("name", "ds", "mountPath", "/etc/grafana/provisioning/datasources")),
		)
		docs = append(docs, deployment(in, "grafana", "fabric", 1, ps(y.L(graf), y.New("volumes", y.L(
			y.New("name", "ds", "configMap", y.New("name", "grafana-datasources")),
		))), nil))
	}

	if ch := c.Fabric.Clickhouse; ch != nil {
		db := or(ch.Database, in.M.L.Runtime()+"_kpi")
		docs = append(docs, configMap(in, "clickhouse-init", y.New("10-reader.sh", ClickhouseReaderInit)))
		ctr := y.New("name", "clickhouse", "image", in.subst(or(ch.Image, ImgClickhouse)),
			"env", y.L(envVar("CLICKHOUSE_DB", db), envVar("CLICKHOUSE_USER", in.chUser("writer")),
				envVar("CLICKHOUSE_PASSWORD", secretEnv(secret, in.chPassVar("WRITER"))),
				envVar("CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT", "1"),
				envVar("READER_USER", in.chUser("reader")), envVar("READER_PASSWORD", secretEnv(secret, in.chPassVar("READER")))),
			"ports", y.L(y.New("containerPort", 9000), y.New("containerPort", 8123)),
			"volumeMounts", y.L(y.New("name", "data", "mountPath", "/var/lib/clickhouse"), y.New("name", "init", "mountPath", "/docker-entrypoint-initdb.d")),
		)
		docs = append(docs, y.New("apiVersion", "apps/v1", "kind", "StatefulSet",
			"metadata", meta("clickhouse", in.labels("clickhouse", "fabric"), nil),
			"spec", y.New("serviceName", "clickhouse", "replicas", 1,
				"selector", y.New("matchLabels", y.New("app", "clickhouse")),
				"template", y.New("metadata", y.New("labels", in.labels("clickhouse", "fabric")),
					"spec", ps(y.L(ctr), y.New("volumes", y.L(y.New("name", "init", "configMap", y.New("name", "clickhouse-init")))))),
				"volumeClaimTemplates", y.L(y.New("metadata", y.New("name", "data"),
					"spec", y.New("accessModes", y.L("ReadWriteOnce"), "resources", y.New("requests", y.New("storage", storageSize(in)))))),
			)))
	}
	return docs
}

func storageSize(in *Input) string {
	if in.Env != nil && in.Env.Storage.Durable.Size != "" {
		return in.Env.Storage.Durable.Size
	}
	return "10Gi"
}

// WorkloadNames lists every Deployment/StatefulSet axon renders (for rollout waits and pruning).
func WorkloadNames(docs []any) (deploys, statefulsets []string) {
	for _, d := range docs {
		m, ok := d.(*y.M)
		if !ok {
			continue
		}
		name := m.Get("metadata").(*y.M).Get("name").(string)
		switch m.Get("kind") {
		case "Deployment":
			deploys = append(deploys, name)
		case "StatefulSet":
			statefulsets = append(statefulsets, name)
		}
	}
	return
}
