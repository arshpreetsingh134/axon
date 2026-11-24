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
