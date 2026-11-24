package official

import (
	"encoding/json"

	"github.com/BurntSushi/toml"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// LoadFrontendIP reads the front door's public IP from platforms/<env>.outputs.json.
func LoadFrontendIP(root, env string) string {
	var out struct {
		Azure struct {
			FrontendIP string `json:"frontend_ip"`
		} `json:"azure"`
	}
	b, err := os.ReadFile(filepath.Join(root, "platforms", env+".outputs.json"))
	if err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out.Azure.FrontendIP
}

// peer is one allowed destination: a named rule with a port and either a pod
// label or an IP block.
type peer struct {
	name string // the policy name suffix
	app  string // destination pod label app=…, or ""
	cidr string // destination IP block, or ""
	port int
}

func (p peer) to() map[string]any {
	if p.cidr != "" {
		return map[string]any{"ipBlock": map[string]any{"cidr": p.cidr}}
	}
	return map[string]any{"podSelector": map[string]any{"matchLabels": map[string]any{"app": p.app}}}
}

// mirrored reports whether the destination also gets an ingress policy: every
// pod destination except the front door's HTTPS ports.
func (p peer) mirrored() bool {
	return p.app != "" && p.name != "traefik-https-pod" && p.name != "traefik-https-service"
}

var urlHost = regexp.MustCompile(`https?://([a-z0-9-]+)(?:\.[a-z0-9.-]+)?:(\d+)`)

// RenderNetworkPolicies renders the allow-list NetworkPolicies of the rendered
// members (SPEC.md Appendix C, observed): one egress policy per edge on its
// source, and a mirrored ingress policy on each pod destination. Edges come
// from each member's environment and the platform's fixed peers. It reads the
// Deployments already rendered in objs.
func RenderNetworkPolicies(in *RenderInput, objs []Object, frontendIP string) []Object {
	fixed := map[string]peer{
		"kpi":                   {name: "kpi", app: "kpi", port: 7373},
		"tempo":                 {name: "tempo", app: "tempo", port: 4317},
		"kubernetes-api":        {name: "kubernetes-api", cidr: "0.0.0.0/0", port: 443},
		"managed-postgres":      {name: "managed-postgres", cidr: "0.0.0.0/0", port: 5432},
		"temporal":              {name: "temporal", app: "temporal", port: 7233},
		"clickhouse":            {name: "clickhouse", app: "clickhouse", port: 9000},
		"keycloak":              {name: "keycloak", app: "keycloak", port: 8080},
		"mycel-iam":             {name: "mycel-iam", app: "mycel-iam", port: 7373},
		"azure-frontend":        {name: "azure-frontend", cidr: frontendIP + "/32", port: 443},
		"traefik-https-pod":     {name: "traefik-https-pod", app: "traefik", port: 8443},
		"traefik-https-service": {name: "traefik-https-service", app: "traefik", port: 443},
	}
	appPeer := func(N string) peer { return peer{name: N, app: N, port: 9080} }
	iamN, kpiN := "", ""
	for _, a := range in.Desc.App {
		switch {
		case a.Kind == "iam":
			iamN = K8sName(a.Name)
		case a.Host == "kpi":
			kpiN = K8sName(a.Name)
		}
	}

	memberOf := map[string]string{}
	for _, a := range in.Desc.Switcher.App {
		memberOf[K8sName(a.App)] = a.App
	}
	worker := map[string]bool{}
	wfmN := ""
	for _, a := range in.Desc.App {
		if a.Kind == "project" {
			worker[K8sName(a.Name)] = true
		}
		if a.Host == "wfm" {
			wfmN = K8sName(a.Name)
		}
	}
	type edgeSet map[string]peer
	edges := map[string]edgeSet{} // source label → peers
	add := func(src string, p peer) {
		if edges[src] == nil {
			edges[src] = edgeSet{}
		}
		edges[src][p.name] = p
	}
	// a worker reaches the apps its environment names (the KPI app included),
	// the workflow manager, and the fabric it uses
	addWorkerEdges := func(N string, o Object) {
		pod := o["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		env, _ := pod["containers"].([]any)[0].(map[string]any)["env"].([]any)
		for _, e := range env {
			if v, ok := e.(map[string]any)["value"].(string); ok {
				for _, h := range urlHost.FindAllStringSubmatch(v, -1) {
					if strings.HasPrefix(h[1], "app-") && h[1] != N {
						add(N, appPeer(h[1]))
					}
				}
			}
		}
		if wfmN != "" { // workers talk to the workflow manager on its worker port
			wt := tokenLabel(in, memberOf[wfmN])
			add(N, peer{name: wfmN, app: wfmN, port: 7380})
			add(N, peer{name: wt, app: wt, port: 7380})
		}
		for _, k := range []string{"kpi", "tempo", "temporal"} {
			add(N, fixed[k])
		}
	}
	for _, o := range objs {
		if o["kind"] != "Deployment" {
			continue
		}
		N := o["metadata"].(map[string]any)["name"].(string)
		member, ok := memberOf[N]
		if !ok {
			if worker[N] {
				addWorkerEdges(N, o)
			}
			continue
		}
		var env []any
		pod := o["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		env, _ = pod["containers"].([]any)[0].(map[string]any)["env"].([]any)
		names := map[string]bool{}
		base := edgeSet{}
		for _, e := range env {
			m := e.(map[string]any)
			names[m["name"].(string)] = true
			if v, ok := m["value"].(string); ok {
				for _, h := range urlHost.FindAllStringSubmatch(v, -1) {
					if strings.HasPrefix(h[1], "app-") && h[1] != N && h[1] != kpiN {
						base[h[1]] = appPeer(h[1])
					}
				}
			}
		}
		for _, k := range []string{"kpi", "tempo", "kubernetes-api"} {
			base[k] = fixed[k]
		}
		if names["DATABASE_URL"] {
			base["managed-postgres"] = fixed["managed-postgres"]
		}
		if names["TEMPORAL_HOSTPORT"] {
			base["temporal"] = fixed["temporal"]
		}
		if names["CLICKHOUSE_DSN"] {
			base["clickhouse"] = fixed["clickhouse"]
		}
		if N == iamN { // the IAM module signs in through keycloak and the front door
			for _, k := range []string{"azure-frontend", "keycloak", "traefik-https-pod", "traefik-https-service"} {
				base[k] = fixed[k]
			}
		}
		token := tokenLabel(in, member)
		delete(base, token)                                    // a module never lists its own token as a peer
		if _, isProduct := productKey(in, member); isProduct { // the console reads every app
			for _, a := range in.Desc.Switcher.App {
				if K8sName(a.App) != N {
					base[K8sName(a.App)] = appPeer(K8sName(a.App))
				}
			}
		}
		// the module token's pods: base plus the member app
		for _, p := range base {
			add(token, p)
		}
		add(token, appPeer(N))
		// the member app: base plus the platform peers
		for _, p := range base {
			add(N, p)
		}
		add(N, peer{name: token, app: token, port: 7373})
		for _, k := range []string{"azure-frontend", "mycel-iam", "traefik-https-pod", "traefik-https-service"} {
			add(N, fixed[k])
		}
		if kpiN != "" && N != kpiN {
			add(N, appPeer(kpiN))
		}
		if iamN != "" && N != iamN {
			add(N, appPeer(iamN))
		}
	}

	// the fabric's own edges
	for _, a := range in.Desc.Switcher.App { // the front door routes to every app
		add("traefik", appPeer(K8sName(a.App)))
	}
	add("traefik", fixed["kubernetes-api"])
	if in.Desc.HasFabric("keycloak") {
		add("traefik", fixed["keycloak"])
		add("keycloak", fixed["managed-postgres"])
	}
	if in.Desc.HasFabric("tracing") {
		add("traefik", peer{name: "grafana", app: "grafana", port: 3000})
		add("grafana", peer{name: "tempo", app: "tempo", port: 3200})
		for _, k := range []string{"azure-frontend", "traefik-https-pod", "traefik-https-service"} {
			add("grafana", fixed[k])
		}
	}
	if in.Desc.HasFabric("temporal") {
		add("temporal", fixed["managed-postgres"])
		add("temporal-search-attrs", fixed["temporal"])
	}
	if _, ok := in.Desc.Product["helix"]; ok {
		add("helix", fixed["managed-postgres"])
	}
	add("ensure-databases", fixed["managed-postgres"])
	if iamN != "" {
		add(iamN+"-review-close", appPeer(iamN))
	}

	meta := func(name string) map[string]any {
		return map[string]any{"annotations": map[string]any{}, "name": name, "namespace": in.Namespace}
	}
	var out []Object
	var srcs []string
	for s := range edges {
		srcs = append(srcs, s)
	}
	sort.Strings(srcs)
	for _, src := range srcs {
		for _, p := range edges[src] {
			out = append(out, Object{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
				"metadata": meta("allow-" + src + "-egress-to-" + p.name),
				"spec": map[string]any{
					"egress":      []any{map[string]any{"ports": []any{map[string]any{"port": p.port, "protocol": "TCP"}}, "to": []any{p.to()}}},
					"podSelector": map[string]any{"matchLabels": map[string]any{"app": src}},
					"policyTypes": []any{"Egress"}}})
			if p.mirrored() {
				out = append(out, Object{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
					"metadata": meta("allow-" + p.app + "-ingress-from-" + src),
					"spec": map[string]any{
						"ingress":     []any{map[string]any{"from": []any{map[string]any{"podSelector": map[string]any{"matchLabels": map[string]any{"app": src}}}}, "ports": []any{map[string]any{"port": p.port, "protocol": "TCP"}}}},
						"podSelector": map[string]any{"matchLabels": map[string]any{"app": p.app}},
						"policyTypes": []any{"Ingress"}}})
			}
		}
	}
	return out
}

// productKey returns the product key of a member that is a product (clc).
func productKey(in *RenderInput, member string) (string, bool) {
	for key, p := range in.Desc.Product {
		if p.App == member {
			return key, true
		}
	}
	return "", false
}

// tokenLabel is the pod label of a member's module token: its first
// materialization token, or a platform app's token, with "_" as "-".
func tokenLabel(in *RenderInput, member string) string {
	t := ""
	for key, p := range in.Desc.Product {
		if p.App == member {
			return key // a product's module token is its key (clc)
		}
	}
	if ts := in.Tokens[member]; len(ts) > 0 {
		t = ts[0]
	} else if host, _, _, _, ok := in.Desc.appEntry(member); ok {
		t = platformTokens[host]
	}
	if t == "mycel_iam" {
		return "mycel-iam"
	}
	return strings.ReplaceAll(t, "_", "-")
}

// RenderSingletons renders the namespace-wide policies: deny all ingress by
// default, allow DNS, let the front door take TLS, and let a vault-mode
// knowledge base reach Azure login and its vault over HTTPS.
func RenderSingletons(in *RenderInput, helixVaultURL string) []Object {
	meta := func(name string) map[string]any {
		return map[string]any{"annotations": map[string]any{}, "name": name, "namespace": in.Namespace}
	}
	np := func(name string, spec map[string]any) Object {
		return Object{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": meta(name), "spec": spec}
	}
	out := []Object{
		np("default-deny-ingress", map[string]any{"podSelector": map[string]any{}, "policyTypes": []any{"Ingress"}}),
		np("allow-dns-egress", map[string]any{
			"egress": []any{map[string]any{
				"ports": []any{map[string]any{"port": 53, "protocol": "UDP"}, map[string]any{"port": 53, "protocol": "TCP"}},
				"to":    []any{map[string]any{"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": "kube-system"}}}}}},
			"podSelector": map[string]any{}, "policyTypes": []any{"Egress"}}),
		np("allow-traefik-ingress-from-anywhere", map[string]any{
			"ingress":     []any{map[string]any{"ports": []any{map[string]any{"port": 8080, "protocol": "TCP"}}}},
			"podSelector": map[string]any{"matchLabels": map[string]any{"app": "traefik"}}, "policyTypes": []any{"Ingress"}}),
		np("allow-traefik-tls-ingress", map[string]any{
			"ingress":     []any{map[string]any{"ports": []any{map[string]any{"port": 8443, "protocol": "TCP"}}}},
			"podSelector": map[string]any{"matchLabels": map[string]any{"app": "traefik"}}, "policyTypes": []any{"Ingress"}}),
	}
	if helixVaultURL != "" {
		host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(helixVaultURL, "https://"), "http://"), "/")
		for _, h := range []string{"login.microsoftonline.com", host} {
			out = append(out, np("allow-helix-egress-https-"+strings.ReplaceAll(h, ".", "-"), map[string]any{
				"egress":      []any{map[string]any{"ports": []any{map[string]any{"port": 443, "protocol": "TCP"}}, "to": []any{map[string]any{"ipBlock": map[string]any{"cidr": "0.0.0.0/0"}}}}},
				"podSelector": map[string]any{"matchLabels": map[string]any{"app": "helix"}}, "policyTypes": []any{"Egress"}}))
		}
	}
	return out
}

// LoadHelixVaultURL reads [helix] vault_url from environments/<env>.toml.
func LoadHelixVaultURL(root, env string) string {
	var doc struct {
		Helix struct {
			VaultURL string `toml:"vault_url"`
		} `toml:"helix"`
	}
	_, _ = toml.DecodeFile(filepath.Join(root, "environments", env+".toml"), &doc)
	return doc.Helix.VaultURL
}
