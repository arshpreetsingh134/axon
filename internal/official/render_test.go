package official

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const renderDescriptor = `name = "example"

[[app]]
name = "example.kpi"
host = "kpi"
db = "kpi"
[app.env]
MYCEL_KPI_URL = "http://app-example-kpi:9080"

[[app]]
name = "example.shop.app"
host = "shop"
db = "shop"
[app.env]
API_KEY = "${SHOP_API_KEY}"
PEER_URL = "http://app-example-orders-app:9080"

[[app]]
name = "example.orders.app"
host = "orders"
[app.env]
MODE = "fast"

[[switcher.app]]
app = "example.orders.app"
title = "Orders"
icon = "box"
order = 20

[[switcher.app]]
app = "example.shop.app"
title = "Shop"
icon = "cart"
order = 10

[[switcher.app]]
app = "example.kpi"
title = "KPI"
icon = "chart"
order = 30
`

func renderFixture(t *testing.T) *RenderInput {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mycelfile")
	if err := os.WriteFile(p, []byte(renderDescriptor), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := LoadDescriptor(p)
	if err != nil {
		t.Fatal(err)
	}
	return &RenderInput{Namespace: "ns", Domain: "example.test", IngressClass: "traefik", TraefikIP: "10.0.0.9",
		PG:     PG{Host: "pg.example.test", Port: 5432, User: "admin", TLS: "verify-full"},
		Images: map[string]string{"example.shop.app": "reg/shop@sha256:1", "example.orders.app": "reg/orders@sha256:2", "example.kpi": "reg/kpi@sha256:3"},
		Desc:   d, Titles: map[string]string{"example.shop.app": "Example Shop", "example.orders.app": "Example Orders"},
		Tokens: map[string][]string{"example.shop.app": {"shop"}, "example.orders.app": {"orders_core"}}}
}

func byKey(objs []Object) map[string]Object {
	m := map[string]Object{}
	for _, o := range objs {
		m[Key(Normalize(o))] = Normalize(o)
	}
	return m
}

func TestRenderAppCore(t *testing.T) {
	in := renderFixture(t)
	objs, _, skipped := RenderAppCore(in)
	if len(skipped) != 0 || len(objs) != 15 {
		t.Fatalf("%d objects, skipped %v", len(objs), skipped)
	}
	m := byKey(objs)
	d := m["Deployment/app-example-shop-app"]
	pod := d["spec"].(map[string]any)["template"].(map[string]any)
	ann := pod["metadata"].(map[string]any)["annotations"].(map[string]any)
	env := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["env"].([]any)
	var names []string
	vals := map[string]map[string]any{}
	for _, e := range env {
		em := e.(map[string]any)
		names = append(names, em["name"].(string))
		vals[em["name"].(string)] = em
	}
	wantHead := "PORT,MYCEL_HOST_CLIENT_ID,MYCEL_HOST_CLIENT_PROOF_KEY,MYCEL_HOST_IAM_URL,MYCEL_HOST_CALLBACK_URL,MYCEL_HOST_OPERATOR_ROLES,MYCEL_HOST_TITLE,MYCEL_HOST_APPS,MYCEL_HOST_DELEGATION_TARGETS,POSTGRES_PASSWORD_URI,DATABASE_URL,API_KEY,MYCEL_ANONYMOUS_ROUTES"
	if got := strings.Join(names[:13], ","); got != wantHead {
		t.Fatalf("env order:\n got %s\nwant %s", got, wantHead)
	}
	if ref := vals["API_KEY"]["valueFrom"].(map[string]any)["secretKeyRef"].(map[string]any); ref["key"] != "SHOP_API_KEY" || ref["name"] != "mycel-app-env" {
		t.Errorf("secret ref %v", ref)
	}
	apps := vals["MYCEL_HOST_APPS"]["value"].(string)
	if !strings.HasPrefix(apps, `{"apps":[{"id":"example.shop.app","title":"Shop"`) {
		t.Errorf("switcher order: %s", apps)
	}
	if ann["mycel.dev/host-apps"] != short16(apps) {
		t.Errorf("host-apps hash")
	}
	if vals["OTEL_SERVICE_NAME"]["value"] != "shop-shop" || vals["MYCEL_HOST_TITLE"]["value"] != "Example Shop" {
		t.Errorf("otel %v title %v", vals["OTEL_SERVICE_NAME"], vals["MYCEL_HOST_TITLE"])
	}
	// orders has no db: no DATABASE_URL
	o := m["Deployment/app-example-orders-app"]
	oenv := o["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["env"].([]any)
	for _, e := range oenv {
		if e.(map[string]any)["name"] == "DATABASE_URL" {
			t.Error("a member without db got DATABASE_URL")
		}
	}
	// delegation targets: the other switcher apps, in switcher order
	cm := m["ConfigMap/app-example-shop-app-delegation-targets"]
	var tj struct {
		Peers []struct{ App string } `json:"peers"`
	}
	_ = json.Unmarshal([]byte(strings.TrimSuffix(cm["data"].(map[string]any)["targets.json"].(string), "\n")), &tj)
	if len(tj.Peers) != 2 || tj.Peers[0].App != "example.orders.app" || tj.Peers[1].App != "example.kpi" {
		t.Errorf("peers %+v", tj.Peers)
	}
	// the kpi platform app gets the restricted ingress
	ing := m["Ingress/app-example-kpi-ingress"]
	paths := ing["spec"].(map[string]any)["rules"].([]any)[0].(map[string]any)["http"].(map[string]any)["paths"].([]any)
	if len(paths) != 9 {
		t.Errorf("platform ingress paths %d", len(paths))
	}
}

func TestRenderNetworkPoliciesMirror(t *testing.T) {
	in := renderFixture(t)
	objs, _, _ := RenderAppCore(in)
	nps := byKey(RenderNetworkPolicies(in, objs, "203.0.113.7"))
	for _, want := range []string{
		"NetworkPolicy/allow-app-example-shop-app-egress-to-app-example-orders-app", // from the env URL
		"NetworkPolicy/allow-app-example-orders-app-ingress-from-app-example-shop-app",
		"NetworkPolicy/allow-shop-egress-to-app-example-orders-app", // the module token mirrors the base
		"NetworkPolicy/allow-shop-egress-to-app-example-shop-app",
		"NetworkPolicy/allow-app-example-shop-app-egress-to-managed-postgres",
		"NetworkPolicy/allow-app-example-shop-app-egress-to-azure-frontend",
		"NetworkPolicy/allow-app-example-shop-app-egress-to-app-example-kpi",
		"NetworkPolicy/allow-orders-core-egress-to-app-example-orders-app", // "_" becomes "-"
	} {
		if _, ok := nps[want]; !ok {
			t.Errorf("missing %s", want)
		}
	}
	if _, ok := nps["NetworkPolicy/allow-app-example-orders-app-egress-to-managed-postgres"]; ok {
		t.Error("a member without db may not reach Postgres")
	}
	if _, ok := nps["NetworkPolicy/allow-traefik-ingress-from-app-example-shop-app"]; ok {
		t.Error("the front door's HTTPS edges are not mirrored")
	}
	fe := nps["NetworkPolicy/allow-app-example-shop-app-egress-to-azure-frontend"]
	if b, _ := json.Marshal(fe["spec"]); !strings.Contains(string(b), `"cidr":"203.0.113.7/32"`) {
		t.Errorf("frontend cidr %s", b)
	}
}

func TestPlanDiff(t *testing.T) {
	live := map[string]Object{"Service/a": Normalize(Object{"kind": "Service", "metadata": map[string]any{"name": "a"},
		"spec": map[string]any{"ports": []any{map[string]any{"name": "http", "port": 80}}}})}
	same := Object{"kind": "Service", "metadata": map[string]any{"name": "a"}, "spec": map[string]any{"ports": []any{map[string]any{"name": "http", "port": 80}}}}
	changed := Object{"kind": "Service", "metadata": map[string]any{"name": "a"}, "spec": map[string]any{"ports": []any{map[string]any{"name": "http", "port": 81}}}}
	fresh := Object{"kind": "Service", "metadata": map[string]any{"name": "b"}}
	if p := Plan([]Object{same}, live); p[0].Action != "unchanged" {
		t.Errorf("same: %+v", p)
	}
	if p := Plan([]Object{changed}, live); p[0].Action != "change" || !strings.Contains(p[0].Diffs[0], "ports[http]/port") {
		t.Errorf("changed: %+v", p)
	}
	if p := Plan([]Object{fresh}, live); p[0].Action != "create" {
		t.Errorf("fresh: %+v", p)
	}
}

func TestRenderFabricFillsAndGates(t *testing.T) {
	in := renderFixture(t)
	v := FabricVars{ResourceGroup: "rg-x", FrontendName: "fe-x", DurableClass: "disk", DurableSize: "9Gi",
		Release: map[string]map[string]string{"fabric": {"traefik": "reg/traefik@sha256:aa"}}}
	objs, skipped := RenderFabric(in, v)
	m := byKey(objs)
	svc, ok := m["Service/traefik"]
	if !ok {
		t.Fatalf("no traefik service; skipped %v", skipped)
	}
	ann := svc["metadata"].(map[string]any)["annotations"].(map[string]any)
	if ann["service.beta.kubernetes.io/azure-load-balancer-resource-group"] != "rg-x" || ann["service.beta.kubernetes.io/azure-pip-name"] != "fe-x" {
		t.Errorf("traefik annotations %v", ann)
	}
	if svc["metadata"].(map[string]any)["namespace"] != "ns" {
		t.Error("namespace not filled")
	}
	if _, ok := m["Deployment/traefik"]; !ok {
		t.Errorf("traefik deployment missing: %v", skipped)
	}
	// the fixture declares no keycloak, temporal, tracing, clickhouse or helix
	for k := range m {
		for _, absent := range []string{"keycloak", "temporal", "grafana", "tempo", "clickhouse", "helix"} {
			if strings.Contains(k, "/"+absent) {
				t.Errorf("%s rendered without its declaration", k)
			}
		}
	}
	for _, o := range objs {
		if b, _ := json.Marshal(o); strings.Contains(string(b), "{{") {
			t.Errorf("unfilled placeholder in %s", Key(o))
		}
	}
}

func TestRenderDatabases(t *testing.T) {
	in := renderFixture(t)
	cm := Normalize(RenderDatabases(in))
	sql := cm["data"].(map[string]any)["01-databases.sql"].(string)
	if !strings.Contains(sql, "CREATE DATABASE kpi'") || !strings.Contains(sql, "CREATE DATABASE shop'") {
		t.Fatalf("missing databases:\n%s", sql)
	}
	if strings.Index(sql, "CREATE DATABASE kpi'") > strings.Index(sql, "CREATE DATABASE shop'") {
		t.Error("databases must follow descriptor order")
	}
	if strings.Contains(sql, "CREATE DATABASE orders") {
		t.Error("a member without db got a database")
	}
	if n := strings.Count(sql, "\\connect "); n != 3 { // postgres + kpi + shop
		t.Errorf("%d audit connects", n)
	}
	if !strings.HasSuffix(sql, "$;\n") {
		t.Error("file must end after the last audit block")
	}
}

func TestRenderIAMConfig(t *testing.T) {
	in := renderFixture(t)
	people := []DemoPerson{{Email: "z@example.test", Name: "z", Teams: []string{"example.shop.app.access", "unknown.app.access"}},
		{Email: "a@example.test", Name: "a", Teams: []string{"example.kpi.members"}}}
	rt := IAMRuntime{PublicJWK: map[string]json.RawMessage{"example.shop.app": json.RawMessage(`{"kty":"EC"}`)},
		CatalogVersion: map[string]string{}, Subject: map[string]string{"a@example.test": "sub-a"}}
	cm, unknown := RenderIAMConfig(in, people, "realm-x", "client-x", rt)
	data := Normalize(cm)["data"].(map[string]any)
	apps := data["applications.json"].(string)
	if strings.Index(apps, `"example.kpi"`) > strings.Index(apps, `"example.orders.app"`) {
		t.Error("applications must be sorted by id")
	}
	if !strings.Contains(apps, `"catalog_version": "r59de7db43982"`) {
		t.Error("the business catalog version must be computed")
	}
	seed := data["seed.json"].(string)
	if strings.Index(seed, "a@example.test") > strings.Index(seed, "z@example.test") || strings.Contains(seed, "unknown.app.access") {
		t.Errorf("seed order or team filter wrong:\n%s", seed)
	}
	if !strings.Contains(data["providers.json"].(string), `"issuer": "https://keycloak.example.test/realms/realm-x"`) {
		t.Error("provider issuer")
	}
	want := map[string]bool{"catalog version of example.kpi": true, "public key of example.kpi (generated at deploy)": true,
		"public key of example.orders.app (generated at deploy)": true, "subject of z@example.test (assigned at deploy)": true}
	for _, u := range unknown {
		delete(want, u)
	}
	if len(want) != 0 {
		t.Errorf("unreported unknowns %v (got %v)", want, unknown)
	}
}
