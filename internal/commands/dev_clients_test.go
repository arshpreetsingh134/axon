package commands

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"axon/internal/official"
)

func loadDesc(t *testing.T, body string) *official.Descriptor {
	t.Helper()
	f := filepath.Join(t.TempDir(), "mycelfile")
	if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := official.LoadDescriptor(f)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

const clientsDesc = `
[[app]]
name = "shop.wfc.worker"
kind = "project"
[app.env]
WFC_SERVICE_CLIENT_ID = "shop.wfc"
WFC_SERVICE_KEY_FILE = "/keys/key.pem"
WFC_SERVICE_AUDIENCES = '{"http://billing:9080":"shop.billing","http://orders:9080":"shop.orders","http://misc:9080":"shop.misc"}'

[[app]]
name = "shop.quote"
[app.env]
RATING_CLIENT_ID = "quote-rating-client"
RATING_CLIENT_AUDIENCE = "shop.rating"
RATING_CLIENT_KEY_PEM_BASE64 = "${SHOP_QUOTE_RATING_KEY}"
BILLING_CLIENT_ID = "quote-billing-client"
BILLING_CLIENT_AUDIENCE = "shop.billing"
BILLING_CLIENT_KEY_PEM_BASE64 = "inline-not-managed"
`

func TestServiceClientsReadsTheIdentityMapAndAppClients(t *testing.T) {
	d := loadDesc(t, clientsDesc)
	over := map[string]map[string]string{"shop.wfc.worker": {
		"WFC_SERVICE_CLIENT_PER_AUDIENCE": "0",
		"WFC_SERVICE_IDENTITIES":          `{"http://billing:9080":"WFC_BILLING_SERVICE_","http://orders:9080":"WFC_ORDERS_SERVICE_"}`,
		"WFC_BILLING_SERVICE_CLIENT_ID":   "billing-worker", "WFC_BILLING_SERVICE_KEY_PEM_BASE64": "${SHOP_BILLING_WORKER_KEY}",
		"WFC_ORDERS_SERVICE_CLIENT_ID": "orders-worker", "WFC_ORDERS_SERVICE_KEY_PEM_BASE64": "${SHOP_ORDERS_WORKER_KEY}",
	}}
	got, notes := serviceClients(d, over)
	want := []serviceClient{
		{Member: "shop.wfc.worker", Principal: "billing-worker", Site: "shop.billing", KeyRef: "SHOP_BILLING_WORKER_KEY", Worker: true},
		{Member: "shop.wfc.worker", Principal: "orders-worker", Site: "shop.orders", KeyRef: "SHOP_ORDERS_WORKER_KEY", Worker: true},
		{Member: "shop.quote", Principal: "quote-rating-client", Site: "shop.rating", KeyRef: "SHOP_QUOTE_RATING_KEY"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("clients\n got %+v\nwant %+v", got, want)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "quote-billing-client") {
		t.Fatalf("notes %v", notes)
	}
	// the default worker principal keeps only the target the map does not cover
	ids := devIdentities(d, over)
	if len(ids) != 1 || !reflect.DeepEqual(ids[0].Sites, []string{"shop.misc"}) {
		t.Fatalf("identities %+v", ids)
	}
}

func TestServiceClientsLeavesPerAudienceWorkersToTheirOwnPath(t *testing.T) {
	d := loadDesc(t, clientsDesc)
	over := map[string]map[string]string{"shop.wfc.worker": {"WFC_SERVICE_CLIENT_PER_AUDIENCE": "1",
		"WFC_SERVICE_IDENTITIES": `{"http://billing:9080":"WFC_BILLING_SERVICE_"}`, "WFC_BILLING_SERVICE_CLIENT_ID": "billing-worker",
		"WFC_BILLING_SERVICE_KEY_PEM_BASE64": "${K}"}}
	got, _ := serviceClients(d, over)
	for _, c := range got {
		if c.Worker {
			t.Fatalf("per-audience worker produced a map client: %+v", c)
		}
	}
	if ids := devIdentities(d, over); len(ids) != 1 || len(ids[0].Sites) != 3 {
		t.Fatalf("identities %+v", ids)
	}
}

func TestChooseClientRoles(t *testing.T) {
	rating := map[string][]string{"access": nil, "admin": nil, "workflow-worker": {"workflow.call"},
		"rating-quote-client": {"rating.quote.call"}, "migration-rating-client": {"migration.rating.call"}}
	middleware := map[string][]string{"access": nil, "admin": nil, "migration-enrichment-worker": {"migration.enrichment.call"}}
	policy := map[string][]string{"access": nil, "admin": nil, "policy-renewal-worker": {"policy.renewal.workflow"}}
	cases := []struct {
		name     string
		c        serviceClient
		declared map[string][]string
		catalog  map[string][]string
		want     []string
	}{
		{"declared wins", serviceClient{Principal: "workspace-rating-client"}, map[string][]string{"workspace-rating-client": {"rating-quote-client"}}, rating, []string{"rating-quote-client"}},
		{"role named like the principal", serviceClient{Principal: "policy-renewal-worker", Worker: true}, nil, policy, []string{"policy-renewal-worker"}},
		{"the only scoped role", serviceClient{Principal: "migration-middleware-worker", Worker: true}, nil, middleware, []string{"migration-enrichment-worker"}},
		{"worker falls back to workflow.call", serviceClient{Principal: "rating-worker", Worker: true}, nil, rating, []string{"workflow-worker"}},
		{"ambiguous app client is not granted", serviceClient{Principal: "workspace-rating-client"}, nil, rating, nil},
	}
	for _, tc := range cases {
		got, why := chooseClientRoles(tc.c, tc.declared)("site", tc.catalog)
		if !reflect.DeepEqual(got, tc.want) || (tc.want == nil && !strings.Contains(why, "service_roles")) {
			t.Errorf("%s: got %v (%q), want %v", tc.name, got, why, tc.want)
		}
	}
}

// The Forms shapes of insurance.safepoint.mycel's dev overrides: a receiver's single-entry audience map, and the
// worker's per-target clients sharing one map, resolved by each target's *_URL.
const formsDesc = `
[[app]]
name = "forms.filemanager"
[app.env]
FORMS_SERVICE_CLIENT_ID = "forms-filemanager-policy"
FORMS_SERVICE_AUDIENCES = '{"http://app-forms-policy:9080": "forms.forms-policy"}'
FORMS_SERVICE_KEY_PEM_BASE64 = "${FM_POLICY_KEY}"

[[app]]
name = "forms.wfc"
kind = "project"
[app.env]
FORMS_WORKER_AUDIENCES = '{"http://app-forms-filemanager:9080": "forms.filemanager", "http://app-forms-registry:9080": "forms.registry", "http://app-forms-delivery:9080": "forms.delivery"}'
FORMS_WORKER_FILEMANAGER_CLIENT_ID = "forms-worker-filemanager"
FORMS_WORKER_FILEMANAGER_KEY_PEM_BASE64 = "${W_FM_KEY}"
MYCEL_PAS_FILEMANAGER_URL = "http://app-forms-filemanager:9080"
FORMS_WORKER_REGISTRY_CLIENT_ID = "forms-worker-registry"
FORMS_WORKER_REGISTRY_KEY_PEM_BASE64 = "${W_REG_KEY}"
MYCEL_PAS_REGISTRY_URL = "http://app-forms-registry:9080"
FORMS_WORKER_DELIVERY_CLIENT_ID = "forms-worker-delivery"
FORMS_WORKER_DELIVERY_KEY_PEM_BASE64 = "${W_DEL_KEY}"
OIDC_CLIENT_ID = "not-a-service-client"
`

func TestServiceClientsFormsShapes(t *testing.T) {
	got, notes := serviceClients(loadDesc(t, formsDesc), nil)
	var have []string
	for _, c := range got {
		have = append(have, c.Principal+" "+c.Site+" "+c.KeyRef)
	}
	want := []string{
		"forms-filemanager-policy forms.forms-policy FM_POLICY_KEY",
		"forms-worker-delivery forms.delivery W_DEL_KEY", // no *_DELIVERY_URL: found by the site's last segment
		"forms-worker-filemanager forms.filemanager W_FM_KEY",
		"forms-worker-registry forms.registry W_REG_KEY",
	}
	if !reflect.DeepEqual(have, want) || len(notes) != 0 {
		t.Fatalf("clients:\n%s\nwant:\n%s\nnotes: %v", strings.Join(have, "\n"), strings.Join(want, "\n"), notes)
	}
}

func TestMapClientSiteAmbiguous(t *testing.T) {
	env := map[string]string{
		"X_SERVICE_KEY_PEM_BASE64":  "${K}",
		"X_SERVICE_AUDIENCES":       `{"http://a:9080":"s.a","http://b:9080":"s.b"}`, // two entries: no single site
		"X_WORKER_A_KEY_PEM_BASE64": "${K2}",
		"X_WORKER_AUDIENCES":        `{"http://a:9080":"one.a","http://b:9080":"two.a"}`, // two sites end in "a"
	}
	if site, ok := mapClientSite(env, "X_SERVICE"); ok {
		t.Fatalf("X_SERVICE resolved to %s", site)
	}
	if site, ok := mapClientSite(env, "X_WORKER_A"); ok {
		t.Fatalf("X_WORKER_A resolved to %s", site)
	}
	if _, ok := mapClientSite(map[string]string{"P_AUDIENCES": `{"o":"s"}`}, "P"); ok {
		t.Fatal("a client without a key reference resolved")
	}
}

func TestAppEnvRefs(t *testing.T) {
	objs := []official.Object{{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{
		map[string]any{"env": []any{
			map[string]any{"name": "A", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "mycel-app-env", "key": "PULSE_API_KEY"}}},
			map[string]any{"name": "B", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "mycel-app-env", "key": "OPT", "optional": true}}},
			map[string]any{"name": "C", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "other", "key": "X"}}},
		}}}}}}}}
	if got := appEnvRefs(objs); !reflect.DeepEqual(got, []string{"PULSE_API_KEY"}) {
		t.Fatalf("refs: %v", got)
	}
}
