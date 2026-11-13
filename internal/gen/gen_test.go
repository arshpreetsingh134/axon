package gen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"axon/internal/descriptor"
	"axon/internal/layout"
)

const cluster = `build_type = "cluster"
name = "demo"
domain = "lvh.me"
core_image = "core:1"
[fabric.postgres]
port = 5545
[fabric.temporal]
shards = 8
[fabric.keycloak]
realm = "demo"
client = "demo-iam"
secret = "${OIDC_CLIENT_SECRET}"
[fabric.traefik]
http = 8090
[fabric.tracing]
grafana_port = 3000
[fabric.iam]
issuer = "demo"
audience = "demo-app"
backend = "mycel_iam:7373"

[[app]]
name = "demo.iam"
kind = "iam"
host = "iam"
db = "iam"

[[app]]
name = "demo.web"
host = "web"
db = "web"
[app.env]
API_KEY = "${API_KEY}"
BASE = "http://${HOST}:80"

[[app]]
name = "demo.wfc"
kind = "project"
temporal = "temporal:7233"
`

func fixture(t *testing.T) *descriptor.Model {
	t.Helper()
	root := t.TempDir()
	ctrl := filepath.Join(root, "demo.axon")
	write := func(p, s string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(s), 0o644)
	}
	write(filepath.Join(ctrl, ".axon.infra", "axonfile"), cluster)
	write(filepath.Join(root, "demo.iam", "axonfile"), "build_type = \"app\"\n[app]\nname = \"demo.iam\"\n[[compose]]\nsource = \"modules/mycel_iam/module\"\nimage = \"iam:1\"\ntoken = \"mycel_iam\"\ndb = true\n")
	write(filepath.Join(root, "demo.web", "axonfile"), "build_type = \"app\"\n[app]\nname = \"demo.web\"\n[[compose]]\nsource = \"modules/web_ui/module\"\n")
	write(filepath.Join(root, "demo.web", "modules", "web_ui", "module", "axonfile"), "build_type = \"module\"\n[module]\ntoken = \"web_ui\"\nlanguage = \"go\"\n")
	write(filepath.Join(root, "demo.wfc", "axonfile"), "build_type = \"project\"\n[app]\nname = \"demo.wfc\"\n[[project.build]]\nname = \"x\"\ncommand = \"true\"\n")
	f, err := layout.FindCluster(ctrl, layout.ModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	m, err := descriptor.Load(f)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func input(m *descriptor.Model) *Input {
	return &Input{M: m, Target: "docker", PG: PG{Host: "postgres", Port: 5432, User: "axon", Password: "${POSTGRES_PASSWORD}"}, Tags: map[string]string{}}
}

func TestInjectedEnv(t *testing.T) {
	m := fixture(t)
	in := input(m)
	web := m.AppByName("demo.web")
	core := in.CoreEnv(web)
	if core["MYCEL_GATE"] != "1" || core["MYCEL_LOGIN_URL"] != "http://iam.lvh.me:8090/m/mycel_iam/oidc/login" ||
		core["OTEL_SERVICE_NAME"] != "web-core" || core["API_KEY"] != "${API_KEY}" {
		t.Fatalf("core env %v", core)
	}
	mod := in.ModuleEnv(web, web.Modules[0])
	if mod["DATABASE_URL"] != "postgres://axon:${POSTGRES_PASSWORD}@postgres:5432/web?sslmode=disable" ||
		mod["TEMPORAL_HOSTPORT"] != "temporal:7233" || mod["MYCEL_KPI_URL"] != "kpi:7373" || mod["OTEL_SERVICE_NAME"] != "web-web_ui" {
		t.Fatalf("module env %v", mod)
	}
	iam := m.AppByName("demo.iam")
	ie := in.ModuleEnv(iam, iam.Modules[0])
	if ie["MYCEL_IAM_SIGNING_KEY"] != "${IAM_SIGNING_KEY}" || ie["OIDC_CLIENT_SECRET"] != "${OIDC_CLIENT_SECRET}" ||
		ie["OIDC_ISSUER"] != "http://keycloak.lvh.me:8090/realms/demo" || ie["MYCEL_SESSION_COOKIE_DOMAIN"] != "lvh.me" {
		t.Fatalf("iam env %v", ie)
	}
	if _, gated := in.CoreEnv(iam)["MYCEL_GATE"]; gated {
		t.Fatal("the iam core must not be gated")
	}
	w := in.WorkerEnv(m.AppByName("demo.wfc"))
	if w["TEMPORAL_HOSTPORT"] != "temporal:7233" || w["PORT"] != "8090" || w["OTEL_SERVICE_NAME"] != "demo.wfc" {
		t.Fatalf("worker env %v", w)
	}
}

func TestComposeFilesParseAndHoldNoSecrets(t *testing.T) {
	m := fixture(t)
	in := input(m)
	files := FabricFiles(in)
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(files["fabric/docker-compose.yml"]), &doc); err != nil {
		t.Fatal(err)
	}
	svcs := doc["services"].(map[string]any)
	for _, s := range []string{"postgres", "temporal", "temporal-ui", "keycloak", "tempo", "grafana", "traefik"} {
		if svcs[s] == nil {
			t.Errorf("fabric lacks %s", s)
		}
	}
	tr := svcs["traefik"].(map[string]any)["networks"].(map[string]any)["shared"].(map[string]any)["aliases"].([]any)
	if !contains(tr, "web.lvh.me") || !contains(tr, "keycloak.lvh.me") {
		t.Fatalf("traefik aliases %v", tr)
	}
	sql := files["fabric/postgres/init/01-databases.sql"]
	for _, db := range []string{"iam", "web", "keycloak", "temporal"} {
		if !strings.Contains(sql, "datname = '"+db+"'") {
			t.Errorf("init SQL lacks %s", db)
		}
	}
	if !strings.Contains(files["fabric/traefik/dynamic/routes.yml"], "Host(`web.lvh.me`)") {
		t.Fatal("no route for web")
	}
	app := AppCompose(in, m.AppByName("demo.web"))
	if err := yaml.Unmarshal([]byte(app), &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(app, "BASE: http://${HOST}:80") || strings.Contains(app, "BEGIN") {
		t.Fatalf("app compose:\n%s", app)
	}
	core := doc["services"].(map[string]any)["core"].(map[string]any)
	aliases := core["networks"].(map[string]any)["shared"].(map[string]any)["aliases"].([]any)
	if !contains(aliases, "web-core") || !contains(aliases, "app-demo-web") {
		t.Fatalf("core aliases %v", aliases)
	}
}

func contains(xs []any, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestK8sEnvSecretRefs(t *testing.T) {
	m := fixture(t)
	in := input(m)
	in.Target = "k8s"
	k := &K8s{Namespace: "demo-dev", Secrets: map[string]string{"API_KEY": "s3cret", "HOST": "h"}}
	env, hash := in.k8sEnv(map[string]string{"API_KEY": "${API_KEY}", "BASE": "http://${HOST}:80", "PLAIN": "a$(b)", "SVC_EMAIL": "${FORMS_EMAIL}"}, k)
	out, _ := yaml.Marshal(env)
	s := string(out)
	if !strings.Contains(s, "secretKeyRef") || !strings.Contains(s, "value: http://$(HOST):80") || !strings.Contains(s, "value: a$$(b)") {
		t.Fatalf("env:\n%s", s)
	}
	if strings.Contains(s, "$(FORMS_EMAIL)") || !strings.Contains(s, "key: FORMS_EMAIL") {
		t.Fatalf("a renamed pure ref must be a direct secretKeyRef:\n%s", s)
	}
	if strings.Contains(s, "s3cret") {
		t.Fatal("secret value leaked into the manifest")
	}
	k.Secrets["API_KEY"] = "rotated"
	if _, h2 := in.k8sEnv(map[string]string{"API_KEY": "${API_KEY}"}, k); h2 == hash {
		t.Fatal("config hash must change when a referenced secret changes")
	}
	docs := K8sWorkloads(in, &K8s{Namespace: "demo-dev", ServiceIPs: map[string]string{"traefik": "10.0.0.1", "mycel-iam": "10.0.0.2"}, Secrets: map[string]string{}})
	deps, _ := WorkloadNames(docs)
	all, _ := yaml.Marshal(docs)
	if strings.Contains(string(all), "- mycel_iam") {
		t.Fatal("hostAliases must not carry non-DNS names like mycel_iam")
	}
	if !strings.Contains(string(all), "10.0.0.2 mycel_iam") || !strings.Contains(string(all), "subPath: hosts") {
		t.Fatal("cores must resolve tokens through the mounted /etc/hosts")
	}
	want := map[string]bool{"traefik": true, "temporal": true, "keycloak": true, "mycel-iam": true, "web-ui": true, "app-demo-web": true, "app-demo-iam": true, "app-demo-wfc": true}
	for _, d := range deps {
		delete(want, d)
	}
	if len(want) > 0 {
		t.Fatalf("missing deployments %v (have %v)", want, deps)
	}
}

func TestRouteHostFromExternalURL(t *testing.T) {
	c := &descriptor.Cluster{Domain: "lvh.me"}
	cases := map[string]string{"http://app.lvh.me:8095": "app", "https://x.example.com": "", "": ""}
	for u, want := range cases {
		if got := RouteHost(c, &descriptor.App{Kind: "project", ExternalURL: u}); got != want {
			t.Errorf("RouteHost(%q) = %q, want %q", u, got, want)
		}
	}
}
