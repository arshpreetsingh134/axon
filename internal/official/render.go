package official

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
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
	ClusterName  string            // the descriptor's name (the default first IAM administrator is admin@<name>.com)

	// The developer profile (a dev environment of a pipeline-released cluster): plain HTTP on the
	// front door's port, the local Docker Postgres, the image id on each pod template, and the
	// environment's per-app overrides. Zero values are the official profile.
	Scheme    string                       // "http" for dev; "" means https
	FrontPort int                          // the front door's port in URLs (dev), 0 for none
	PGDocker  bool                         // POSTGRES_PASSWORD + sslmode=disable instead of the managed server's form
	ImageIDs  bool                         // annotate pod templates with mycel.build/image-id
	AppEnv    map[string]map[string]string // environments/<env>.overrides.toml [app_env.<app>]
	// Entrypoints are the Ingress entrypoints ("web" for dev); "" means "web,websecure".
	Entrypoints string
	// NoPullSecret drops the image pull secret from pods (a developer's nodes pull with their own credentials).
	NoPullSecret bool
	// Exclude lists members left as they are, with the reason.
	Exclude map[string]string
	// ConsoleModules keeps the live console topology's module list of members left as they are.
	ConsoleModules map[string][]string

	// The IAM Deployment's runtime inputs, read from the cluster: the first
	// administrator's subject and the IAM configuration's hash. Without both
	// the IAM Deployment is not rendered.
	IAMFirstAdminSubject string
	IAMConfigHash        string
	Desc                 *Descriptor
	// Catalogs are access catalogs read from the members' images (label io.mycel.module.access, dev profile); a member
	// without one keeps the observed default (catalogFor).
	Catalogs map[string]Catalog
	// ServiceKeys mounts a project member's service-principal private key (dev profile, SPEC.md Appendix C.13).
	ServiceKeys map[string]ServiceKey
	Titles      map[string]string   // member → materialization [app] title
	Tokens      map[string][]string // member → materialization [[compose]] tokens
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

	// FabricOrder lists the [fabric.*] tables in file order.
	FabricOrder []string `toml:"-"`
}

var fabricTable = regexp.MustCompile(`(?m)^\[fabric\.([A-Za-z0-9_-]+)\]`)

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
	if b, err := os.ReadFile(path); err == nil {
		for _, m := range fabricTable.FindAllStringSubmatch(string(b), -1) {
			d.FabricOrder = append(d.FabricOrder, m[1])
		}
	}
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

// url is a front-door URL: scheme://<host>.<domain>[:port].
// URL is the front-door URL of a host ("iam" → http://iam.localhost:8095 on a dev profile).
func (in *RenderInput) URL(host string) string { return in.url(host) }

func (in *RenderInput) url(host string) string {
	u := or(in.Scheme, "https") + "://" + host + "." + in.Domain
	if in.FrontPort != 0 {
		u += fmt.Sprintf(":%d", in.FrontPort)
	}
	return u
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
		list = append(list, appJSON{ID: a.App, Title: a.Title, Icon: a.Icon, URL: in.url(host)})
	}
	hostApps, _ := json.Marshal(map[string]any{"apps": list})
	meta := func(name string) map[string]any {
		return map[string]any{"annotations": map[string]any{}, "name": name, "namespace": in.Namespace}
	}

	for _, a := range apps {
		name := a.App
		host, db, kind, denv, ok := in.Desc.appEntry(name)
		if why, ex := in.Exclude[name]; ex {
			skipped = append(skipped, name+" (left as it is: "+why+")")
			continue
		}
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
		token := rawToken(in, name)
		iam := in.url("iam")
		head := []any{
			envVal("PORT", "9080"),
			envVal("MYCEL_HOST_CLIENT_ID", name),
			envSecret("MYCEL_HOST_CLIENT_PROOF_KEY", "proof-key", "mycel-host-"+dashed, false),
			envVal("MYCEL_HOST_IAM_URL", iam),
			envVal("MYCEL_HOST_CALLBACK_URL", in.url(host)+"/auth/callback"),
			envVal("MYCEL_HOST_OPERATOR_ROLES", "admin,operator,mycel_operator,platform"),
			envVal("MYCEL_HOST_TITLE", title),
			envVal("MYCEL_HOST_APPS", string(hostApps)),
			envVal("MYCEL_HOST_DELEGATION_TARGETS", "/etc/mycel/delegation/targets.json"),
		}
		platform := in.Titles[name] == "" && platformTokens[host] != ""
		if db != "" {
			if in.PGDocker {
				head = append(head, envSecret("POSTGRES_PASSWORD", "password", "postgres-credentials", false))
			} else {
				head = append(head, envSecret("POSTGRES_PASSWORD_URI", "uri-password", "postgres-credentials", false))
			}
			if platform && host == "kpi" {
				head = append(head,
					envSecret("MYCEL_CLICKHOUSE_WRITER_PASSWORD", "password", "clickhouse-kpi-writer", false),
					envSecret("MYCEL_CLICKHOUSE_READER_PASSWORD", "password", "clickhouse-kpi-reader", false),
					envVal("CLICKHOUSE_DATABASE", "mycel_kpi"),
					envVal("CLICKHOUSE_DSN", "clickhouse://kpi_writer:$(MYCEL_CLICKHOUSE_WRITER_PASSWORD)@clickhouse:9000/mycel_kpi?dial_timeout=5s"),
					envVal("CLICKHOUSE_READ_DSN", "clickhouse://kpi_reader:$(MYCEL_CLICKHOUSE_READER_PASSWORD)@clickhouse:9000/mycel_kpi?dial_timeout=5s"))
			}
			if in.PGDocker {
				head = append(head, envVal("DATABASE_URL", fmt.Sprintf("postgres://%s:$(POSTGRES_PASSWORD)@%s:%d/%s?sslmode=disable", in.PG.User, in.PG.Host, in.PG.Port, db)))
			} else {
				head = append(head,
					envVal("DATABASE_URL", fmt.Sprintf("postgres://%s:$(POSTGRES_PASSWORD_URI)@%s:%d/%s?sslmode=%s&sslrootcert=/etc/ssl/certs/ca-certificates.crt",
						in.PG.User, in.PG.Host, in.PG.Port, db, or(in.PG.TLS, "verify-full"))))
			}
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
		merged := map[string]string{}
		for k, v := range denv {
			merged[k] = v
		}
		for k, v := range merged {
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
		if console { // the cluster console: what it watches and how to reach it
			realm := ""
			if kc, ok := in.Desc.Fabric["keycloak"].(map[string]any); ok {
				realm, _ = kc["realm"].(string)
			}
			for k, v := range map[string]string{
				"MYCEL_CONSOLE_REALM":    realm,
				"MYCEL_CONSOLE_TARGETS":  in.consoleTargets(),
				"MYCEL_CONSOLE_TOPOLOGY": in.consoleTopology(),
				"MYCEL_KPI_URL":          "kpi:7373",
			} {
				rest[k] = envVal(k, v)
				if !contains(keys, k) {
					keys = append(keys, k)
				}
			}
			if in.Desc.HasFabric("tracing") {
				rest["MYCEL_CONSOLE_GRAFANA_URL"] = envVal("MYCEL_CONSOLE_GRAFANA_URL", in.url("grafana"))
				if !contains(keys, "MYCEL_CONSOLE_GRAFANA_URL") {
					keys = append(keys, "MYCEL_CONSOLE_GRAFANA_URL")
				}
			}
			sort.Strings(keys)
		}
		isIAM := kind == "iam"
		if isIAM { // the IAM module runs in cluster mode from its mounted configuration
			for k, v := range map[string]string{
				"MYCEL_DEFAULT_RETURN_TO":       in.url(host) + "/",
				"MYCEL_IAM_APPLICATIONS_FILE":   "/etc/mycel/iam/applications.json",
				"MYCEL_IAM_FIRST_ADMIN_EMAIL":   "admin@" + in.ClusterName + ".com",
				"MYCEL_IAM_FIRST_ADMIN_SUBJECT": in.IAMFirstAdminSubject,
				"MYCEL_IAM_MODE":                "cluster",
				"MYCEL_IAM_OIDC_PROVIDERS_FILE": "/etc/mycel/iam/providers.json",
				"MYCEL_IAM_REALM":               in.Namespace,
				"MYCEL_IAM_SEED_FILE":           "/etc/mycel/iam/seed.json",
			} {
				rest[k] = envVal(k, v)
				if !contains(keys, k) {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
		}
		for k, v := range in.AppEnv[name] { // the environment's overrides win
			if m := exactRef.FindStringSubmatch(v); m != nil {
				rest[k] = envSecret(k, m[1], "mycel-app-env", false)
			} else {
				rest[k] = envVal(k, v)
			}
			if !contains(keys, k) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		env := head
		for _, k := range keys {
			env = append(env, rest[k])
		}
		if isIAM {
			env = append(env,
				envSecret("MYCEL_IAM_SIGNING_KEY", "key.pem", "mycel-iam-signing-keys", false),
				envSecret("MYCEL_IAM_SIGNING_KEY_PREVIOUS", "previous.pem", "mycel-iam-signing-keys", true),
				envSecret("MYCEL_IAM_PROVIDER_SECRETS", "providers", "mycel-iam-provider-secrets", false))
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
		if in.NoPullSecret {
			delete(pod, "imagePullSecrets")
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
		if in.ImageIDs {
			if i := strings.Index(image, "@"); i >= 0 {
				dep["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["annotations"].(map[string]any)["mycel.build/image-id"] = image[i+1:]
			}
		}
		if isIAM {
			container["volumeMounts"] = append(container["volumeMounts"].([]any), map[string]any{"mountPath": "/etc/mycel/iam", "name": "mycel-iam-configuration", "readOnly": true})
			pod["volumes"] = append(pod["volumes"].([]any), map[string]any{"configMap": map[string]any{"name": "mycel-iam-configuration"}, "name": "mycel-iam-configuration"})
			ann := dep["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["annotations"].(map[string]any)
			ann["mycel.dev/first-admin-subject"] = in.IAMFirstAdminSubject
			ann["mycel.dev/iam-configuration"] = in.IAMConfigHash
			dep["spec"].(map[string]any)["strategy"] = map[string]any{"type": "Recreate"}
		}
		if console {
			pod["automountServiceAccountToken"] = true
			pod["serviceAccountName"] = "mycel-app-cluster-console"
			labels := dep["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]any)
			labels["mycel.io/license-journal-clc"] = "true"
			dep["spec"].(map[string]any)["strategy"] = map[string]any{"type": "Recreate"}
		}
		if isIAM && (in.IAMFirstAdminSubject == "" || in.IAMConfigHash == "") {
			skipped = append(skipped, "Deployment/"+N+" (needs runtime inputs from the cluster: the first IAM administrator and the IAM configuration hash)")
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
		"metadata": map[string]any{"annotations": map[string]any{"traefik.ingress.kubernetes.io/router.entrypoints": or(in.Entrypoints, "web,websecure")},
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
		if why, ex := in.Exclude[a.Name]; ex {
			skipped = append(skipped, a.Name+" (left as it is: "+why+")")
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
		for k, v := range in.AppEnv[a.Name] { // the environment's overrides win, as for apps
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
		pod := workerPod(in, N, container)
		if k, ok := in.ServiceKeys[a.Name]; ok && k.Secret != "" && k.Path != "" {
			dir, file := path.Split(k.Path)
			items := []any{map[string]any{"key": "key.pem", "path": file}}
			for _, x := range k.Extra {
				if x != file {
					items = append(items, map[string]any{"key": x, "path": x})
				}
			}
			container["volumeMounts"] = []any{map[string]any{"mountPath": strings.TrimRight(dir, "/"), "name": "service-key", "readOnly": true}}
			pod["volumes"] = []any{map[string]any{"name": "service-key", "secret": map[string]any{"secretName": k.Secret, "defaultMode": 0o440,
				"items": items}}}
			pod["securityContext"] = map[string]any{"fsGroup": 65532}
		}
		objs = append(objs,
			Object{"apiVersion": "v1", "kind": "ServiceAccount", "automountServiceAccountToken": false, "metadata": meta(N)},
			Object{"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"annotations": map[string]any{}, "labels": map[string]any{"app": N}, "name": N, "namespace": in.Namespace},
				"spec": map[string]any{"replicas": 1, "revisionHistoryLimit": 3,
					"selector": map[string]any{"matchLabels": map[string]any{"app": N}},
					"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": N}},
						"spec": pod}}})
		if a.Port != 0 {
			objs = append(objs, Object{"apiVersion": "v1", "kind": "Service", "metadata": meta(N),
				"spec": map[string]any{"ports": []any{map[string]any{"port": a.Port, "targetPort": a.Port}}, "selector": map[string]any{"app": N}}})
		}
	}
	return objs, skipped
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// consoleTargets lists every app the console watches: each [[app]] with a
// host, in descriptor order, then the products behind the front door.
func (in *RenderInput) consoleTargets() string {
	var t []string
	for _, a := range in.Desc.App {
		if a.Host != "" {
			t = append(t, a.Name+"=http://"+K8sName(a.Name)+":9080")
		}
	}
	for _, s := range in.Desc.Switcher.App {
		if _, ok := productKey(in, s.App); ok {
			t = append(t, s.App+"=http://"+K8sName(s.App)+":9080")
		}
	}
	return strings.Join(t, ",")
}

// consoleTopology is the cluster's shape as the console shows it: the fabric
// services, and every app with its kind, host and module tokens.
func (in *RenderInput) consoleTopology() string {
	type app struct {
		Name    string   `json:"name"`
		Kind    string   `json:"kind"`
		Host    string   `json:"host,omitempty"`
		Modules []string `json:"modules"`
	}
	fabric := []string{}
	for _, f := range in.Desc.FabricOrder {
		if f != "iam" && f != "console" {
			fabric = append(fabric, f)
		}
	}
	apps := []app{}
	for _, a := range in.Desc.App {
		switch a.Kind {
		case "project":
			apps = append(apps, app{Name: a.Name, Kind: "project", Modules: []string{}})
		case "iam":
			apps = append(apps, app{Name: a.Name, Kind: "iam", Host: a.Host, Modules: []string{rawToken(in, a.Name)}})
		default:
			mods := []string{rawToken(in, a.Name)}
			if live, ok := in.ConsoleModules[a.Name]; ok && in.Exclude[a.Name] != "" {
				mods = live
			}
			apps = append(apps, app{Name: a.Name, Kind: "module", Host: a.Host, Modules: mods})
		}
	}
	for _, s := range in.Desc.Switcher.App {
		if key, ok := productKey(in, s.App); ok {
			host, _, _, _, _ := in.Desc.appEntry(s.App)
			apps = append(apps, app{Name: s.App, Kind: "module", Host: host, Modules: []string{key}})
		}
	}
	b, _ := json.Marshal(struct {
		Fabric []string `json:"fabric"`
		Apps   []app    `json:"apps"`
	}{fabric, apps})
	return string(b)
}

// PinnedImages maps each image-backed member to the image its member file pins
// (its first pinned image): what a developer's deploy runs when there is no
// release record.
func (l *Layout) PinnedImages() map[string]string {
	out := map[string]string{}
	ms, _ := l.Materializations()
	for _, m := range ms {
		for _, img := range m.Images {
			if _, _, ok := ParsePin(img); ok {
				out[m.Name] = img
				break
			}
		}
	}
	return out
}

// ServiceKey is a Secret holding a worker's private key (data key "key.pem") and the file path the worker reads it at.
// Extra names further data keys of the Secret (one key per service principal) mounted beside it under their own names.
type ServiceKey struct {
	Secret string
	Path   string
	Extra  []string
}

func workerPod(in *RenderInput, N string, container map[string]any) map[string]any {
	pod := map[string]any{"automountServiceAccountToken": false, "containers": []any{container},
		"imagePullSecrets": []any{map[string]any{"name": "mycel-ghcr-pull"}}, "priorityClassName": "mycel-app",
		"serviceAccountName": N, "terminationGracePeriodSeconds": 90}
	if in.NoPullSecret {
		delete(pod, "imagePullSecrets")
	}
	return pod
}

// WorkerImages maps each project member to the image its member Dockerfile
// starts FROM, when that line pins a digest.
func (l *Layout) WorkerImages() map[string]string {
	out := map[string]string{}
	ms, _ := l.Materializations()
	for _, m := range ms {
		if len(m.Images) == 1 {
			if _, _, ok := ParsePin(m.Images[0]); ok {
				out[m.Name] = m.Images[0]
			}
		}
	}
	return out
}
