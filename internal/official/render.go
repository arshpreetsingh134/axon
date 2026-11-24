package official

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Object is one rendered Kubernetes object, shaped like a kubectl
// last-applied configuration.
type Object = map[string]any

// RenderInput is everything the official renderer reads (plan T1.1).
type RenderInput struct {
	Namespace    string
	Domain       string
	IngressClass string
	PG           PG                // managed Postgres (platforms/<env>.outputs.json)
	Images       map[string]string // release record [modules]: member → image@digest
	TraefikIP    string            // ClusterIP of the traefik-internal Service (hostAliases); live
	Desc         *Descriptor
	Titles       map[string]string   // member → materialization [app] title
	Tokens       map[string][]string // member → materialization [[compose]] tokens
}

// PG is the managed Postgres an official environment uses.
type PG struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
	TLS  string `json:"tls"`
}

// Descriptor is the part of the cluster descriptor the renderer reads.
type Descriptor struct {
	App []struct {
		Name     string            `toml:"name"`
		Kind     string            `toml:"kind"`
		Host     string            `toml:"host"`
		DB       string            `toml:"db"`
		Temporal string            `toml:"temporal"`
		Port     int               `toml:"port"`
		Env      map[string]string `toml:"env"`
	} `toml:"app"`
	Switcher struct {
		App []SwitcherApp `toml:"app"`
	} `toml:"switcher"`
	Product map[string]struct {
		Host string `toml:"host"`
		App  string `toml:"app"`
	} `toml:"product"`
	Fabric map[string]any `toml:"fabric"`
}

// HasFabric reports whether the descriptor declares [fabric.<name>].
func (d *Descriptor) HasFabric(name string) bool { _, ok := d.Fabric[name]; return ok }

// SwitcherApp is one [[switcher.app]] entry.
type SwitcherApp struct {
	App   string `toml:"app"`
	Title string `toml:"title"`
	Icon  string `toml:"icon"`
	Order int    `toml:"order"`
}

// LoadDescriptor reads the renderer's view of a descriptor file.
func LoadDescriptor(path string) (*Descriptor, error) {
	var d Descriptor
	if _, err := toml.DecodeFile(path, &d); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	sort.SliceStable(d.Switcher.App, func(i, j int) bool { return d.Switcher.App[i].Order < d.Switcher.App[j].Order })
	return &d, nil
}

// LoadPG reads the managed Postgres from platforms/<env>.outputs.json.
func LoadPG(root, env string) (PG, error) {
	var out struct {
		Postgres PG `json:"postgres"`
	}
	b, err := os.ReadFile(filepath.Join(root, "platforms", env+".outputs.json"))
	if err != nil {
		return PG{}, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return PG{}, err
	}
	return out.Postgres, nil
}

// MemberMeta reads each materialization's [app] title and [[compose]] tokens.
func (l *Layout) MemberMeta() (titles map[string]string, tokens map[string][]string, err error) {
	titles, tokens = map[string]string{}, map[string][]string{}
	dir := filepath.Join(l.Root, MaterializationsDir)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return titles, tokens, nil
	}
	for _, e := range ents {
		var doc struct {
			App struct {
				Name  string `toml:"name"`
				Title string `toml:"title"`
			} `toml:"app"`
			Compose []struct {
				Token string `toml:"token"`
			} `toml:"compose"`
		}
		p := filepath.Join(dir, e.Name(), "mycelfile")
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if _, err := toml.DecodeFile(p, &doc); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", l.Rel(p), err)
		}
		name := or(doc.App.Name, e.Name())
		if doc.App.Title != "" {
			titles[name] = doc.App.Title
		}
		for _, c := range doc.Compose {
			if c.Token != "" {
				tokens[name] = append(tokens[name], c.Token)
			}
		}
	}
	return titles, tokens, nil
}

// K8sName is a member's object name: "app-" + the name with dots as dashes.
func K8sName(member string) string { return "app-" + strings.ReplaceAll(member, ".", "-") }

func short16(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:16]
}

// platformTokens are the module tokens of the platform apps, which have no
// materialization (SPEC.md Appendix C, observed).
var platformTokens = map[string]string{"iam": "mycel_iam", "wfm": "workflow_manager", "kpi": "kpi"}

// appEntry finds a descriptor [[app]] by name.
func (d *Descriptor) appEntry(name string) (host, db, kind string, env map[string]string, ok bool) {
	for _, a := range d.App {
		if a.Name == name {
			return a.Host, a.DB, a.Kind, a.Env, true
		}
	}
	for key, p := range d.Product {
		if p.App == name {
			db := ""
			if key == "clc" {
				db = "console" // the cluster console keeps its state in its own database
			}
			return p.Host, db, "product", nil, true
		}
	}
	return "", "", "", nil, false
}

// hosts lists keycloak, then every [[app]] host in descriptor order, then
// product hosts: the names pods resolve to the front door (hostAliases).
func (in *RenderInput) hosts() []string {
	out := []string{"keycloak." + in.Domain}
	for _, a := range in.Desc.App {
		if a.Host != "" {
			out = append(out, a.Host+"."+in.Domain)
		}
	}
	inSwitcher := map[string]bool{}
	for _, a := range in.Desc.Switcher.App {
		inSwitcher[a.App] = true
	}
	var ps []string
	for _, p := range in.Desc.Product {
		if p.Host != "" && inSwitcher[p.App] { // a storage engine has a host but is not behind the front door
			ps = append(ps, p.Host+"."+in.Domain)
		}
	}
	sort.Strings(ps)
	return append(out, ps...)
}

func titleWords(name string) string {
	var w []string
	for _, p := range strings.Split(name, ".") {
		if p != "" {
			w = append(w, strings.ToUpper(p[:1])+p[1:])
		}
	}
	return strings.Join(w, " ")
}

var exactRef = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

// RenderAppCore renders the per-member objects of every app-core member (the
// apps of the switcher): ServiceAccount, Service, Ingress, delegation-targets
// ConfigMap and Deployment. Members it cannot render are reported in skipped.
//
// shadow holds Deployments rendered only as inputs to the NetworkPolicies (their
// environment); they are not emitted until they render completely.
func RenderAppCore(in *RenderInput) (objs, shadow []Object, skipped []string) {
	apps := in.Desc.Switcher.App
	// the switcher list (MYCEL_HOST_APPS), shared by every member
	type appJSON struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Icon  string `json:"icon"`
		URL   string `json:"url"`
	}
	var list []appJSON
	for _, a := range apps {
		host, _, _, _, _ := in.Desc.appEntry(a.App)
		list = append(list, appJSON{ID: a.App, Title: a.Title, Icon: a.Icon, URL: "https://" + host + "." + in.Domain})
	}
	hostApps, _ := json.Marshal(map[string]any{"apps": list})
	meta := func(name string) map[string]any {
		return map[string]any{"annotations": map[string]any{}, "name": name, "namespace": in.Namespace}
	}

	for _, a := range apps {
		name := a.App
		host, db, kind, denv, ok := in.Desc.appEntry(name)
		image := in.Images[name]
		if !ok || host == "" || image == "" {
			skipped = append(skipped, fmt.Sprintf("%s (host %q, image %q)", name, host, image))
			continue
		}
		N := K8sName(name)
		dashed := strings.TrimPrefix(N, "app-")

		// delegation targets: every other switcher app
		type peer struct {
			App      string `json:"app"`
			ClientID string `json:"client_id"`
			Origin   string `json:"origin"`
		}
		var peers []peer
		for _, p := range apps {
			if p.App != name {
				peers = append(peers, peer{App: p.App, ClientID: p.App, Origin: fmt.Sprintf("http://%s.%s.svc:9080", K8sName(p.App), in.Namespace)})
			}
		}
		targets, _ := json.Marshal(map[string]any{"peers": peers})

		console := kind == "product" // the cluster console: its own service account, serves before it is ready
		sa := Object{"apiVersion": "v1", "kind": "ServiceAccount", "automountServiceAccountToken": false, "metadata": meta("mycel-" + N)}
		svcSpec := map[string]any{"ports": []any{map[string]any{"name": "http", "port": 9080, "targetPort": "http"}}, "selector": map[string]any{"app": N}}
		if console {
			sa = Object{"apiVersion": "v1", "kind": "ServiceAccount", "automountServiceAccountToken": true, "metadata": meta("mycel-app-cluster-console")}
			svcSpec["publishNotReadyAddresses"] = true
		}
		objs = append(objs, sa, Object{"apiVersion": "v1", "kind": "Service", "metadata": meta(N), "spec": svcSpec},
			ingress(in, N, host, platformTokens[host], in.Titles[name] == "" && (host == "kpi" || host == "wfm")),
		)
		_ = []any{
			Object{"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
				"metadata": map[string]any{"annotations": map[string]any{"traefik.ingress.kubernetes.io/router.entrypoints": "web,websecure"},
					"name": N + "-ingress", "namespace": in.Namespace},
				"spec": map[string]any{"ingressClassName": in.IngressClass, "rules": []any{map[string]any{
					"host": host + "." + in.Domain,
					"http": map[string]any{"paths": []any{map[string]any{
						"backend":  map[string]any{"service": map[string]any{"name": N, "port": map[string]any{"number": 9080}}},
						"path":     "/",
						"pathType": "Prefix"}}}}}}},
		}
		objs = append(objs,
			Object{"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"annotations": map[string]any{"mycel.dev/host-delegation-targets": short16(string(targets))},
					"labels": map[string]any{"app": N, "app.kubernetes.io/managed-by": "mycel"},
					"name":   N + "-delegation-targets", "namespace": in.Namespace},
				"data": map[string]any{"targets.json": string(targets) + "\n"}},
		)

		// environment
		title := in.Titles[name]
		if title == "" {
			title = titleWords(name)
		}
		token := ""
		if ts := in.Tokens[name]; len(ts) > 0 {
			token = ts[0]
		} else {
			token = platformTokens[host]
		}
		iam := "https://iam." + in.Domain
		head := []any{
			envVal("PORT", "9080"),
			envVal("MYCEL_HOST_CLIENT_ID", name),
			envSecret("MYCEL_HOST_CLIENT_PROOF_KEY", "proof-key", "mycel-host-"+dashed, false),
			envVal("MYCEL_HOST_IAM_URL", iam),
			envVal("MYCEL_HOST_CALLBACK_URL", "https://"+host+"."+in.Domain+"/auth/callback"),
			envVal("MYCEL_HOST_OPERATOR_ROLES", "admin,operator,mycel_operator,platform"),
			envVal("MYCEL_HOST_TITLE", title),
			envVal("MYCEL_HOST_APPS", string(hostApps)),
			envVal("MYCEL_HOST_DELEGATION_TARGETS", "/etc/mycel/delegation/targets.json"),
		}
		platform := in.Titles[name] == "" && platformTokens[host] != ""
		if db != "" {
			head = append(head, envSecret("POSTGRES_PASSWORD_URI", "uri-password", "postgres-credentials", false))
			if platform && host == "kpi" {
				head = append(head,
					envSecret("MYCEL_CLICKHOUSE_WRITER_PASSWORD", "password", "clickhouse-kpi-writer", false),
					envSecret("MYCEL_CLICKHOUSE_READER_PASSWORD", "password", "clickhouse-kpi-reader", false),
					envVal("CLICKHOUSE_DATABASE", "mycel_kpi"),
					envVal("CLICKHOUSE_DSN", "clickhouse://kpi_writer:$(MYCEL_CLICKHOUSE_WRITER_PASSWORD)@clickhouse:9000/mycel_kpi?dial_timeout=5s"),
					envVal("CLICKHOUSE_READ_DSN", "clickhouse://kpi_reader:$(MYCEL_CLICKHOUSE_READER_PASSWORD)@clickhouse:9000/mycel_kpi?dial_timeout=5s"))
			}
			head = append(head,
				envVal("DATABASE_URL", fmt.Sprintf("postgres://%s:$(POSTGRES_PASSWORD_URI)@%s:%d/%s?sslmode=%s&sslrootcert=/etc/ssl/certs/ca-certificates.crt",
					in.PG.User, in.PG.Host, in.PG.Port, db, or(in.PG.TLS, "verify-full"))))
		}
		rest := map[string]any{
			"MYCEL_ANONYMOUS_ROUTES":      envVal("MYCEL_ANONYMOUS_ROUTES", "/api/system"),
			"MYCEL_IAM_ISSUER":            envVal("MYCEL_IAM_ISSUER", iam),
			"OTEL_EXPORTER_OTLP_ENDPOINT": envVal("OTEL_EXPORTER_OTLP_ENDPOINT", "http://tempo:4317"),
			"OTEL_SERVICE_NAME":           envVal("OTEL_SERVICE_NAME", host+"-"+token),
			"OTEL_TRACES_SAMPLER":         envVal("OTEL_TRACES_SAMPLER", "parentbased_always_on"),
		}
		if platform {
			switch host {
			case "kpi":
				rest["MYCEL_ANONYMOUS_ROUTES"] = envVal("MYCEL_ANONYMOUS_ROUTES", "/api/system,/m/kpi/events")
			case "wfm":
				rest["MYCEL_ANONYMOUS_ROUTES"] = envVal("MYCEL_ANONYMOUS_ROUTES", "/api/system,/m/workflow_manager/workflows,/m/workflow_manager/workflow")
				rest["TEMPORAL_HOSTPORT"] = envVal("TEMPORAL_HOSTPORT", "temporal:7233")
				rest["TEMPORAL_NAMESPACE"] = envVal("TEMPORAL_NAMESPACE", "default")
			}
		}
		for k, v := range denv {
			if m := exactRef.FindStringSubmatch(v); m != nil {
				rest[k] = envSecret(k, m[1], "mycel-app-env", false)
			} else {
				rest[k] = envVal(k, v)
			}
		}
		var keys []string
		for k := range rest {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		env := head
		for _, k := range keys {
			env = append(env, rest[k])
		}

		container := map[string]any{
			"env":             env,
			"image":           image,
			"imagePullPolicy": "IfNotPresent",
			"lifecycle":       map[string]any{"preStop": map[string]any{"exec": map[string]any{"command": []any{"sleep", "5"}}}},
			"livenessProbe":   probe("/livez", 6, 30, 5),
			"name":            N,
			"ports":           []any{map[string]any{"containerPort": 9080, "name": "http"}},
			"readinessProbe":  probe("/healthz", 2, 5, 3),
			"resources": map[string]any{"limits": map[string]any{"memory": "512Mi"},
				"requests": map[string]any{"cpu": "10m", "memory": "32Mi"}},
			"securityContext": map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}},
				"readOnlyRootFilesystem": true, "runAsNonRoot": true, "runAsUser": 65532, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
			"startupProbe": probe("/livez", 40, 3, 3),
			"volumeMounts": []any{map[string]any{"mountPath": "/tmp", "name": "tmp"},
				map[string]any{"mountPath": "/etc/mycel/delegation", "name": "mycel-delegation-targets", "readOnly": true}},
		}
		var hostnames []any
		for _, h := range in.hosts() {
			hostnames = append(hostnames, h)
		}
		pod := map[string]any{
			"automountServiceAccountToken":  false,
			"containers":                    []any{container},
			"imagePullSecrets":              []any{map[string]any{"name": "mycel-ghcr-pull"}},
			"priorityClassName":             "mycel-app",
			"serviceAccountName":            "mycel-" + N,
			"terminationGracePeriodSeconds": 90,
			"volumes": []any{map[string]any{"emptyDir": map[string]any{}, "name": "tmp"},
				map[string]any{"configMap": map[string]any{"name": N + "-delegation-targets"}, "name": "mycel-delegation-targets"}},
		}
		if in.TraefikIP != "" {
			pod["hostAliases"] = []any{map[string]any{"hostnames": hostnames, "ip": in.TraefikIP}}
		}
		dep := Object{"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"annotations": map[string]any{}, "labels": map[string]any{"app": N, "mycel.io/app-core-series": "0.2"},
				"name": N, "namespace": in.Namespace},
			"spec": map[string]any{
				"replicas": 1, "revisionHistoryLimit": 3,
				"selector": map[string]any{"matchLabels": map[string]any{"app": N}},
				"template": map[string]any{
					"metadata": map[string]any{
						"annotations": map[string]any{"mycel.dev/host-apps": short16(string(hostApps)), "mycel.dev/host-delegation-targets": short16(string(targets))},
						"labels":      map[string]any{"app": N}},
					"spec": pod}}}
		if kind == "iam" || console {
			skipped = append(skipped, "Deployment/"+N+" (needs runtime inputs: the IAM configuration and first admin, or the console topology)")
			shadow = append(shadow, dep)
			continue
		}
		objs = append(objs, dep)
	}
	return objs, shadow, skipped
}

// ingress is a member's Ingress: one "/" prefix rule, or for a platform app
// only its public paths.
func ingress(in *RenderInput, N, host, token string, platform bool) Object {
	backend := map[string]any{"service": map[string]any{"name": N, "port": map[string]any{"number": 9080}}}
	path := func(p, t string) map[string]any { return map[string]any{"backend": backend, "path": p, "pathType": t} }
	paths := []any{path("/", "Prefix")}
	if platform {
		paths = []any{path("/", "Exact"), path("/_mycel/", "Prefix"), path("/auth/", "Prefix"), path("/healthz", "Exact"), path("/livez", "Exact"),
			path("/api/system", "Exact"), path("/m/"+token+"/", "Exact"), path("/m/"+token+"/api/", "Prefix"), path("/m/"+token+"/assets/", "Prefix")}
	}
	return Object{"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
		"metadata": map[string]any{"annotations": map[string]any{"traefik.ingress.kubernetes.io/router.entrypoints": "web,websecure"},
			"name": N + "-ingress", "namespace": in.Namespace},
		"spec": map[string]any{"ingressClassName": in.IngressClass, "rules": []any{map[string]any{
			"host": host + "." + in.Domain, "http": map[string]any{"paths": paths}}}}}
}

func envVal(k, v string) map[string]any { return map[string]any{"name": k, "value": v} }

func envSecret(k, key, secret string, optional bool) map[string]any {
	ref := map[string]any{"key": key, "name": secret}
	if optional {
		ref["optional"] = true
	}
	return map[string]any{"name": k, "valueFrom": map[string]any{"secretKeyRef": ref}}
}

func probe(path string, failure, period, timeout int) map[string]any {
	return map[string]any{"failureThreshold": failure, "httpGet": map[string]any{"path": path, "port": "http"},
		"periodSeconds": period, "timeoutSeconds": timeout}
}

// RenderWorkers renders the project members (kind = "project", e.g. Temporal
// workers): a ServiceAccount named after the app, a Deployment without probes
// or volumes, and a Service when the member declares a port.
func RenderWorkers(in *RenderInput) (objs []Object, skipped []string) {
	kpiN := ""
	for _, a := range in.Desc.App {
		if a.Host == "kpi" {
			kpiN = K8sName(a.Name)
		}
	}
	meta := func(name string) map[string]any {
		return map[string]any{"annotations": map[string]any{}, "name": name, "namespace": in.Namespace}
	}
	for _, a := range in.Desc.App {
		if a.Kind != "project" {
			continue
		}
		image := in.Images[a.Name]
		if image == "" {
			skipped = append(skipped, a.Name+" (no image in the release)")
			continue
		}
		N := K8sName(a.Name)
		vals := map[string]any{
			"OTEL_EXPORTER_OTLP_ENDPOINT": envVal("OTEL_EXPORTER_OTLP_ENDPOINT", "http://tempo:4317"),
			"OTEL_SERVICE_NAME":           envVal("OTEL_SERVICE_NAME", a.Name),
			"PORT":                        envVal("PORT", "8090"),
		}
		if kpiN != "" {
			vals["MYCEL_KPI_URL"] = envVal("MYCEL_KPI_URL", "http://"+kpiN+":9080")
		}
		if a.Temporal != "" {
			vals["TEMPORAL_HOSTPORT"] = envVal("TEMPORAL_HOSTPORT", a.Temporal)
			vals["TEMPORAL_NAMESPACE"] = envVal("TEMPORAL_NAMESPACE", "default")
		}
		for k, v := range a.Env {
			if m := exactRef.FindStringSubmatch(v); m != nil {
				vals[k] = envSecret(k, m[1], "mycel-app-env", false)
			} else {
				vals[k] = envVal(k, v)
			}
		}
		var keys []string
		for k := range vals {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var env []any
		for _, k := range keys {
			env = append(env, vals[k])
		}
		container := map[string]any{
			"env": env, "image": image, "imagePullPolicy": "IfNotPresent", "name": N,
			"resources": map[string]any{"limits": map[string]any{"memory": "256Mi"}, "requests": map[string]any{"cpu": "10m", "memory": "32Mi"}},
			"securityContext": map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}},
				"readOnlyRootFilesystem": true, "runAsNonRoot": true, "runAsUser": 65532, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		}
		if a.Port != 0 {
			container["ports"] = []any{map[string]any{"containerPort": a.Port}}
		}
		objs = append(objs,
			Object{"apiVersion": "v1", "kind": "ServiceAccount", "automountServiceAccountToken": false, "metadata": meta(N)},
			Object{"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"annotations": map[string]any{}, "labels": map[string]any{"app": N}, "name": N, "namespace": in.Namespace},
				"spec": map[string]any{"replicas": 1, "revisionHistoryLimit": 3,
					"selector": map[string]any{"matchLabels": map[string]any{"app": N}},
					"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": N}},
						"spec": map[string]any{"automountServiceAccountToken": false, "containers": []any{container},
							"imagePullSecrets": []any{map[string]any{"name": "mycel-ghcr-pull"}}, "priorityClassName": "mycel-app",
							"serviceAccountName": N, "terminationGracePeriodSeconds": 90}}}})
		if a.Port != 0 {
			objs = append(objs, Object{"apiVersion": "v1", "kind": "Service", "metadata": meta(N),
				"spec": map[string]any{"ports": []any{map[string]any{"port": a.Port, "targetPort": a.Port}}, "selector": map[string]any{"app": N}}})
		}
	}
	return objs, skipped
}
