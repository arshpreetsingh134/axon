package gen

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"axon/internal/descriptor"
	y "axon/internal/yamlx"
)

// Default fabric images (pinned).
const (
	ImgPostgres   = "postgres:16-alpine"
	ImgTemporal   = "temporalio/auto-setup:1.25.2"
	ImgTemporalUI = "temporalio/ui:2.31.2"
	ImgKeycloak   = "quay.io/keycloak/keycloak:25.0"
	ImgTraefik    = "traefik:v3.1"
	ImgTempo      = "grafana/tempo:2.6.1"
	ImgGrafana    = "grafana/grafana:11.3.0"
	ImgClickhouse = "clickhouse/clickhouse-server:24.8"
)

func or(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Databases returns every logical database the cluster needs, sorted.
func Databases(m *descriptor.Model) []string {
	set := map[string]bool{}
	for _, a := range m.C.Apps {
		if a.DB != "" && !a.Disabled {
			set[a.DB] = true
		}
	}
	if m.C.Fabric.Keycloak != nil {
		set["keycloak"] = true
	}
	if m.C.Fabric.Temporal != nil {
		set["temporal"] = true
		set["temporal_visibility"] = true
	}
	var out []string
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// EnsureDatabasesSQL is idempotent CREATE DATABASE SQL for psql (\gexec).
func EnsureDatabasesSQL(dbs []string) string {
	var b strings.Builder
	for _, d := range dbs {
		fmt.Fprintf(&b, "SELECT 'CREATE DATABASE %q' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '%s')\\gexec\n", d, strings.ReplaceAll(d, "'", "''"))
	}
	return b.String()
}

// PGUser is the Postgres role axon uses for a layout.
func PGUser(m *descriptor.Model) string {
	if p := m.C.Fabric.Postgres; p != nil && p.User != "" {
		return p.User
	}
	return m.L.Runtime() // "axon" (compat or migrated: "mycel", matching existing data volumes)
}

// Hosts returns every <host>.<domain> name the front door serves.
func Hosts(m *descriptor.Model, products bool) []string {
	c := m.C
	d := c.DomainOrDefault()
	set := map[string]bool{}
	for _, a := range m.Apps {
		if h := RouteHost(c, a.Decl); h != "" {
			set[h+"."+d] = true
		}
	}
	if c.Fabric.Keycloak != nil {
		set["keycloak."+d] = true
	}
	if c.Fabric.Tracing != nil {
		set["grafana."+d] = true
	}
	if c.Fabric.Temporal != nil {
		set["temporal."+d] = true
	}
	if products {
		for _, p := range c.Product {
			if p.Host != "" {
				set[p.Host+"."+d] = true
			}
		}
	}
	var out []string
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// Route is one front-door route.
type Route struct {
	Name     string
	Host     string // full host name
	Upstream string // http://svc:port
}

// Routes returns the front-door routing table for a target.
func Routes(in *Input, products bool) []Route {
	c := in.M.C
	d := c.DomainOrDefault()
	var rs []Route
	for _, a := range in.M.Apps {
		h := RouteHost(c, a.Decl)
		if h == "" || a.Problem != "" && a.Decl.Cored() {
			continue
		}
		port := 9080
		if !a.Decl.Cored() {
			port = a.Decl.Port
			if port == 0 {
				port = 8090
			}
		}
		rs = append(rs, Route{Name: h, Host: h + "." + d, Upstream: fmt.Sprintf("http://%s:%d", descriptor.CoreName(a.Decl.Name), port)})
	}
	if c.Fabric.Keycloak != nil {
		rs = append(rs, Route{Name: "keycloak", Host: "keycloak." + d, Upstream: "http://keycloak:8080"})
	}
	if c.Fabric.Tracing != nil {
		rs = append(rs, Route{Name: "grafana", Host: "grafana." + d, Upstream: "http://grafana:3000"})
	}
	if c.Fabric.Temporal != nil {
		rs = append(rs, Route{Name: "temporal", Host: "temporal." + d, Upstream: "http://temporal-ui:8080"})
	}
	if products {
		for _, name := range sortedProducts(c) {
			p := c.Product[name]
			if p.Host != "" && p.Image != "" {
				rs = append(rs, Route{Name: "product-" + name, Host: p.Host + "." + d, Upstream: fmt.Sprintf("http://%s:9080", descriptor.CoreName(productApp(name, p)))})
			}
		}
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Name < rs[j].Name })
	return rs
}

func sortedProducts(c *descriptor.Cluster) []string {
	var ns []string
	for n := range c.Product {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	return ns
}

func productApp(name string, p descriptor.Product) string {
	if p.App != "" {
		return p.App
	}
	return name
}

// RoutesYAML renders Traefik's dynamic file-provider config.
func RoutesYAML(rs []Route, header string) string {
	routers, services := y.New(), y.New()
	for _, r := range rs {
		routers.Set(r.Name, y.New("rule", fmt.Sprintf("Host(`%s`)", r.Host), "entryPoints", y.L("web"), "service", r.Name))
		services.Set(r.Name, y.New("loadBalancer", y.New("passHostHeader", true, "servers", y.L(y.New("url", r.Upstream)))))
	}
	if routers.Len() == 0 {
		return header + "http: {}\n"
	}
	s, _ := y.Marshal(y.New("http", y.New("routers", routers, "services", services)))
	return header + s
}

func volumeName(v *descriptor.Volume, def string) string {
	if v != nil && v.Name != "" {
		return v.Name
	}
	return def
}

func volumeMount(v *descriptor.Volume, def string) string {
	if v != nil && v.Mount != "" {
		return v.Mount
	}
	return def
}

func shared(aliases ...string) *y.M {
	if len(aliases) == 0 {
		return y.New("shared", y.New())
	}
	var l []any
	for _, a := range aliases {
		l = append(l, a)
	}
	return y.New("shared", y.New("aliases", l))
}

// FabricFiles renders the docker-target fabric under fabric/ (paths relative to the infra dir).
func FabricFiles(in *Input) map[string]string {
	m := in.M
	c := m.C
	src := in.Source()
	files := map[string]string{}
	hdrY := Header("#", src)
	pg := c.Fabric.Postgres
	if pg == nil {
		pg = &descriptor.Postgres{}
	}
	user := PGUser(m)
	services := y.New()
	volumes := y.New()

	pgVol := volumeName(pg.Volume, "pgdata")
	volumes.Set(pgVol, y.New())
	maxConn := pg.MaxConnections
	if maxConn == 0 {
		maxConn = 300
	}
	pgPorts := y.L()
	if pg.Port > 0 {
		pgPorts = y.L(fmt.Sprintf("127.0.0.1:%d:5432", pg.Port))
	}
	services.Set("postgres", y.New(
		"image", in.subst(or(pg.Image, ImgPostgres)),
		"restart", "unless-stopped",
		"command", y.L("postgres", "-c", fmt.Sprintf("max_connections=%d", maxConn)),
		"environment", y.New("POSTGRES_USER", user, "POSTGRES_PASSWORD", "${POSTGRES_PASSWORD}", "POSTGRES_DB", "postgres"),
		"ports", pgPorts,
		"volumes", y.L(pgVol+":/var/lib/postgresql/data", "./postgres/init:/docker-entrypoint-initdb.d:ro"),
		"healthcheck", y.New("test", y.L("CMD-SHELL", "pg_isready -U "+user+" -d postgres"), "interval", "3s", "timeout", "3s", "retries", 30),
		"networks", shared("postgres"),
	))
	files["fabric/postgres/init/01-databases.sql"] = Header("--", src) + EnsureDatabasesSQL(Databases(m))

	if t := c.Fabric.Temporal; t != nil {
		shards := t.Shards
		if shards == 0 {
			shards = 4
		}
		ports := y.L()
		if t.Port > 0 {
			ports = y.L(fmt.Sprintf("127.0.0.1:%d:7233", t.Port))
		}
		services.Set("temporal", y.New(
			"image", in.subst(or(t.Image, ImgTemporal)),
			"restart", "unless-stopped",
			"environment", y.New(
				"DB", "postgres12", "DB_PORT", "5432", "POSTGRES_SEEDS", "postgres",
				"POSTGRES_USER", user, "POSTGRES_PWD", "${POSTGRES_PASSWORD}",
				"NUM_HISTORY_SHARDS", fmt.Sprint(shards),
				"DYNAMIC_CONFIG_FILE_PATH", "config/dynamicconfig/axon.yaml",
			),
			"ports", ports,
			"volumes", y.L("./temporal/dynamicconfig.yaml:/etc/temporal/config/dynamicconfig/axon.yaml:ro"),
			"depends_on", y.New("postgres", y.New("condition", "service_healthy")),
			"healthcheck", y.New("test", y.L("CMD-SHELL", "temporal operator cluster health --address temporal:7233 | grep -q SERVING || tctl --address temporal:7233 cluster health | grep -q SERVING"),
				"interval", "5s", "timeout", "5s", "retries", 60, "start_period", "20s"),
			"networks", shared("temporal"),
		))
		files["fabric/temporal/dynamicconfig.yaml"] = hdrY + "limit.maxIDLength:\n  - value: 255\n    constraints: {}\nsystem.forceSearchAttributesCacheRefreshOnRead:\n  - value: true\n    constraints: {}\n"
		uiPorts := y.L()
		if t.UIPort > 0 {
			uiPorts = y.L(fmt.Sprintf("127.0.0.1:%d:8080", t.UIPort))
		}
		services.Set("temporal-ui", y.New(
			"image", in.subst(or(t.UIImage, ImgTemporalUI)),
			"restart", "unless-stopped",
			"environment", y.New("TEMPORAL_ADDRESS", "temporal:7233", "TEMPORAL_CORS_ORIGINS", c.FrontDoor("temporal")),
			"ports", uiPorts,
			"depends_on", y.New("temporal", y.New("condition", "service_healthy")),
			"networks", shared("temporal-ui"),
		))
	}

	if kc := c.Fabric.Keycloak; kc != nil {
		services.Set("keycloak", y.New(
			"image", in.subst(or(kc.Image, ImgKeycloak)),
			"restart", "unless-stopped",
			"command", y.L("start-dev", "--http-port=8080"),
			"environment", y.New(
				"KEYCLOAK_ADMIN", "admin", "KEYCLOAK_ADMIN_PASSWORD", "${KEYCLOAK_ADMIN_PASSWORD}",
				"KC_BOOTSTRAP_ADMIN_USERNAME", "admin", "KC_BOOTSTRAP_ADMIN_PASSWORD", "${KEYCLOAK_ADMIN_PASSWORD}",
				"KC_DB", "postgres", "KC_DB_URL", "jdbc:postgresql://postgres:5432/keycloak",
				"KC_DB_USERNAME", user, "KC_DB_PASSWORD", "${POSTGRES_PASSWORD}",
				"KC_HOSTNAME", c.FrontDoor("keycloak"), "KC_HTTP_ENABLED", "true", "KC_HEALTH_ENABLED", "true",
				"KC_PROXY_HEADERS", "xforwarded",
			),
			"depends_on", y.New("postgres", y.New("condition", "service_healthy")),
			"networks", shared("keycloak"),
		))
	}

	if tr := c.Fabric.Tracing; tr != nil {
		tv := volumeName(tr.TempoVolume, "tempo-data")
		gv := volumeName(tr.GrafanaVolume, "grafana-data")
		volumes.Set(tv, y.New())
		volumes.Set(gv, y.New())
		services.Set("tempo", y.New(
			"image", in.subst(or(tr.TempoImage, ImgTempo)),
			"restart", "unless-stopped",
			"user", "0",
			"command", y.L("-config.file=/etc/tempo.yaml"),
			"volumes", y.L("./tempo/tempo.yaml:/etc/tempo.yaml:ro", tv+":"+volumeMount(tr.TempoVolume, "/var/tempo")),
			"networks", shared("tempo"),
		))
		gPorts := y.L()
		if tr.GrafanaPort > 0 {
			gPorts = y.L(fmt.Sprintf("127.0.0.1:%d:3000", tr.GrafanaPort))
		}
		services.Set("grafana", y.New(
			"image", in.subst(or(tr.GrafanaImage, ImgGrafana)),
			"restart", "unless-stopped",
			"environment", y.New("GF_AUTH_ANONYMOUS_ENABLED", "true", "GF_AUTH_ANONYMOUS_ORG_ROLE", "Admin",
				"GF_AUTH_DISABLE_LOGIN_FORM", "true", "GF_SERVER_ROOT_URL", c.FrontDoor("grafana")),
			"ports", gPorts,
			"volumes", y.L("./grafana/provisioning:/etc/grafana/provisioning:ro", gv+":"+volumeMount(tr.GrafanaVolume, "/var/lib/grafana")),
			"depends_on", y.L("tempo"),
			"networks", shared("grafana"),
		))
		files["fabric/tempo/tempo.yaml"] = hdrY + TempoConfig
		files["fabric/grafana/provisioning/datasources/tempo.yaml"] = hdrY + GrafanaDatasources
	}

	if ch := c.Fabric.Clickhouse; ch != nil {
		cv := volumeName(ch.Volume, "clickhouse-data")
		volumes.Set(cv, y.New())
		ports := y.L()
		if ch.HTTPPort > 0 {
			ports = y.L(fmt.Sprintf("127.0.0.1:%d:8123", ch.HTTPPort))
		}
		db := or(ch.Database, m.L.Runtime()+"_kpi")
		services.Set("clickhouse", y.New(
			"image", in.subst(or(ch.Image, ImgClickhouse)),
			"restart", "unless-stopped",
			"environment", y.New("CLICKHOUSE_DB", db, "CLICKHOUSE_USER", in.chUser("writer"),
				"CLICKHOUSE_PASSWORD", in.Var(in.chPassVar("WRITER")), "CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT", "1",
				"READER_USER", in.chUser("reader"), "READER_PASSWORD", in.Var(in.chPassVar("READER"))),
			"ports", ports,
			"volumes", y.L(cv+":"+volumeMount(ch.Volume, "/var/lib/clickhouse"), "./clickhouse/init:/docker-entrypoint-initdb.d:ro"),
			"ulimits", y.New("nofile", y.New("soft", 262144, "hard", 262144)),
			"networks", shared("clickhouse"),
		))
		files["fabric/clickhouse/init/10-reader.sh"] = Header("#", src) + ClickhouseReaderInit
	}

	// Front door: listens on the same port inside and out, and answers to every
	// <host>.<domain> on the shared network, so front-door URLs work from the
	// browser and from containers alike.
	http := c.HTTPPort()
	var aliases []string
	aliases = append(aliases, "traefik")
	aliases = append(aliases, Hosts(m, c.Deploy.Products)...)
	services.Set("traefik", y.New(
		"image", in.subst(or(traefikImage(c), ImgTraefik)),
		"restart", "unless-stopped",
		"command", y.L(
			fmt.Sprintf("--entrypoints.web.address=:%d", http),
			"--providers.file.directory=/etc/traefik/dynamic", "--providers.file.watch=true",
			"--ping=true", "--ping.entrypoint=web", "--api.dashboard=false", "--log.level=WARN",
		),
		"ports", y.L(fmt.Sprintf("127.0.0.1:%d:%d", http, http)),
		"volumes", y.L("./traefik/dynamic:/etc/traefik/dynamic:ro"),
		"healthcheck", y.New("test", y.L("CMD", "traefik", "healthcheck", "--ping", fmt.Sprintf("--entrypoints.web.address=:%d", http), "--ping.entrypoint=web"),
			"interval", "3s", "timeout", "3s", "retries", 20),
		"networks", shared(aliases...),
	))
	files["fabric/traefik/dynamic/routes.yml"] = RoutesYAML(Routes(in, c.Deploy.Products), hdrY)

	doc := y.New(
		"name", descriptor.NormProject(c.Name),
		"networks", y.New("shared", y.New("name", c.NetworkOrDefault(), "external", true)),
		"volumes", volumes,
		"services", services,
	)
	s, _ := y.Marshal(doc)
	files["fabric/docker-compose.yml"] = hdrY + s
	return files
}

func traefikImage(c *descriptor.Cluster) string {
	if c.Fabric.Traefik != nil {
		return c.Fabric.Traefik.Image
	}
	return ""
}

// TempoConfig is a minimal single-binary Tempo config with OTLP receivers.
const TempoConfig = `server:
  http_listen_port: 3200
distributor:
  receivers:
    otlp:
      protocols:
        grpc:
          endpoint: 0.0.0.0:4317
        http:
          endpoint: 0.0.0.0:4318
storage:
  trace:
    backend: local
    wal:
      path: /var/tempo/wal
    local:
      path: /var/tempo/blocks
compactor:
  compaction:
    block_retention: 72h
`

// GrafanaDatasources provisions Tempo as the default datasource.
const GrafanaDatasources = `apiVersion: 1
datasources:
  - name: Tempo
    type: tempo
    uid: tempo
    access: proxy
    url: http://tempo:3200
    isDefault: true
    editable: false
`

// ClickhouseReaderInit creates a read-only user from env (no secret on disk).
const ClickhouseReaderInit = `set -eu
clickhouse client -n <<SQL
CREATE USER IF NOT EXISTS ${READER_USER} IDENTIFIED WITH sha256_password BY '${READER_PASSWORD}';
GRANT SELECT ON ${CLICKHOUSE_DB}.* TO ${READER_USER};
SQL
`

// AppComposeFile returns the per-app compose path relative to the controller infra dir.
func AppComposeFile(m *descriptor.Model, app string) string {
	return path.Join(m.L.State, "apps", app, "docker-compose.yml")
}

func envM(env map[string]string) *y.M {
	out := map[string]string{}
	for k, v := range env {
		out[k] = escapeDollar(v)
	}
	return y.StrMap(out)
}

// escapeDollar keeps ${VAR} for interpolation and escapes any other '$'.
func escapeDollar(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '$' && !(i+1 < len(s) && s[i+1] == '{') {
			b.WriteString("$$")
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// AppCompose renders one member app's compose stack (docker target).
func AppCompose(in *Input, a *descriptor.AppModel) string {
	m := in.M
	c := m.C
	d := a.Decl
	services := y.New()
	net := y.New("default", y.New(), "shared", y.New("name", c.NetworkOrDefault(), "external", true))
	core := descriptor.CoreName(d.Name)
	if d.Cored() {
		deps := y.New()
		for _, mod := range a.Modules {
			deps.Set(mod.Token, y.New("condition", "service_healthy"))
		}
		services.Set("core", y.New(
			"image", in.Image(descriptor.ServiceImage(c.Name, d.Name, "core")),
			"restart", "unless-stopped",
			"expose", y.L("9080"),
			"environment", envM(in.CoreEnv(a)),
			"healthcheck", y.New("test", y.L("CMD-SHELL", "wget -q -O /dev/null http://127.0.0.1:9080/healthz || exit 1"),
				"interval", "3s", "timeout", "3s", "retries", 40, "start_period", "5s"),
			"networks", y.New("default", y.New(), "shared", y.New("aliases", y.L(d.Host+"-core", core))),
			"depends_on", deps,
		))
		for _, mod := range a.Modules {
			img := mod.Image
			if img == "" {
				img = in.Image(descriptor.ServiceImage(c.Name, d.Name, mod.Token))
			} else {
				img = in.subst(img)
			}
			aliases := y.L(mod.Token)
			if dns := descriptor.DNS(mod.Token); dns != mod.Token {
				aliases = append(aliases, dns)
			}
			services.Set(mod.Token, y.New(
				"image", img,
				"restart", "unless-stopped",
				"expose", y.L("7373"),
				"environment", envM(in.ModuleEnv(a, mod)),
				"healthcheck", y.New("test", y.L("CMD", "nc", "-z", "127.0.0.1", "7373"),
					"interval", "3s", "timeout", "3s", "retries", 40, "start_period", "5s"),
				"networks", y.New("default", y.New(), "shared", y.New("aliases", aliases)),
			))
		}
	} else {
		img := d.Image
		if img == "" {
			img = in.Image(descriptor.WorkerImage(c.Name, d.Name))
		} else {
			img = in.ResolveRef(img)
		}
		svc := y.New(
			"image", img,
			"restart", "unless-stopped",
			"environment", envM(in.WorkerEnv(a)),
			"networks", y.New("shared", y.New("aliases", y.L(descriptor.Slug(d.Name), core))),
		)
		if d.Command != "" {
			svc.Set("command", d.Command)
		}
		if d.Mount != "" && a.Dir != "" {
			svc.Set("volumes", y.L(path.Join(a.Dir, d.Mount)+":/workspace"))
		}
		services.Set(descriptor.Slug(d.Name), svc)
		net = y.New("shared", y.New("name", c.NetworkOrDefault(), "external", true))
	}
	doc := y.New(
		"name", descriptor.NormProject(descriptor.ComposeProject(c.Name, d.Name)),
		"networks", net,
		"services", services,
	)
	s, _ := y.Marshal(doc)
	return Header("#", in.Source()) + s
}

// ProductCompose renders a licensed product as a single-container app.
func ProductCompose(in *Input, name string, p descriptor.Product) string {
	c := in.M.C
	app := productApp(name, p)
	core := descriptor.CoreName(app)
	env := map[string]string{"PORT": "9080"}
	if iam := c.Fabric.IAM; iam != nil {
		env["MYCEL_IAM_ISSUER"], env["MYCEL_IAM_AUDIENCE"], env["MYCEL_IAM_URL"] = iam.Issuer, iam.Audience, iam.Backend
	}
	if lic := c.Fabric.Console; lic != nil && lic.License != "" {
		env["MYCEL_LICENSE_TOKEN"] = "${PRODUCT_LICENSE_TOKEN}"
	}
	doc := y.New(
		"name", descriptor.NormProject(descriptor.ComposeProject(c.Name, app)),
		"networks", y.New("shared", y.New("name", c.NetworkOrDefault(), "external", true)),
		"services", y.New("core", y.New(
			"image", in.subst(p.Image),
			"restart", "unless-stopped",
			"expose", y.L("9080"),
			"environment", envM(env),
			"networks", y.New("shared", y.New("aliases", y.L(p.Host+"-core", core))),
		)),
	)
	s, _ := y.Marshal(doc)
	return Header("#", in.Source()) + s
}
