package official

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// IAMRuntime holds the IAM configuration values a deploy derives from secrets
// or that axon cannot yet recompute: each application's public key and catalog
// version, and the configuration annotation. (Subjects are computed; see
// DemoSubject. Live subjects are kept for reference only.) They are
// read from the live ConfigMap; new entries without one are reported.
type IAMRuntime struct {
	PublicJWK      map[string]json.RawMessage // application id → key
	CatalogVersion map[string]string          // application id → version
	Subject        map[string]string          // email → subject
	Annotation     string
}

// LiveIAMRuntime extracts the runtime values from a live mycel-iam-configuration
// ConfigMap (its last-applied configuration), or returns empty maps.
func LiveIAMRuntime(live Object) IAMRuntime {
	rt := IAMRuntime{PublicJWK: map[string]json.RawMessage{}, CatalogVersion: map[string]string{}, Subject: map[string]string{}}
	if live == nil {
		return rt
	}
	if m, ok := live["metadata"].(map[string]any); ok {
		if a, ok := m["annotations"].(map[string]any); ok {
			rt.Annotation, _ = a["mycel.dev/iam-configuration"].(string)
		}
	}
	data, _ := live["data"].(map[string]any)
	var apps struct {
		Applications []struct {
			ID      string          `json:"application_id"`
			Catalog string          `json:"catalog_version"`
			JWK     json.RawMessage `json:"public_jwk"`
		} `json:"applications"`
	}
	if s, ok := data["applications.json"].(string); ok {
		_ = json.Unmarshal([]byte(s), &apps)
	}
	for _, a := range apps.Applications {
		rt.PublicJWK[a.ID], rt.CatalogVersion[a.ID] = a.JWK, a.Catalog
	}
	var seed struct {
		People []struct {
			Subject string `json:"subject"`
			Email   string `json:"email"`
		} `json:"people"`
	}
	if s, ok := data["seed.json"].(string); ok {
		_ = json.Unmarshal([]byte(s), &seed)
	}
	for _, p := range seed.People {
		rt.Subject[p.Email] = p.Subject
	}
	return rt
}

type iamTeam struct {
	Name  string   `json:"name"`
	Roles []string `json:"roles"`
}

type iamPage struct {
	Path       string `json:"path"`
	Title      string `json:"title"`
	Permission string `json:"permission"`
}

// Catalog is an access catalog a module image declares (label io.mycel.module.access).
type Catalog struct {
	Roles           []string            `json:"roles"`
	RolePermissions map[string][]string `json:"role_permissions,omitempty"`
	Teams           []iamTeam           `json:"teams"`
}

// ParseCatalog reads an io.mycel.module.access label; it must be strict JSON with at least one role.
func ParseCatalog(label string) (Catalog, error) {
	var c struct {
		Version int `json:"version"`
		Catalog
	}
	if err := json.Unmarshal([]byte(label), &c); err != nil {
		return Catalog{}, fmt.Errorf("io.mycel.module.access is not valid JSON: %w", err)
	}
	if len(c.Roles) == 0 {
		return Catalog{}, fmt.Errorf("io.mycel.module.access declares no roles")
	}
	return c.Catalog, nil
}

type iamCatalog struct {
	Roles           []string            `json:"roles"`
	RolePermissions map[string][]string `json:"role_permissions,omitempty"`
	Pages           []iamPage           `json:"pages,omitempty"`
	Teams           []iamTeam           `json:"teams"`
}

// catalogFor is the access catalog a module declares (SPEC.md Appendix C,
// observed): business modules grant access or admin; the platform modules
// declare their own.
func catalogFor(token string) iamCatalog {
	switch token {
	case "mycel_iam", "kpi":
		return iamCatalog{Roles: []string{"member", "admin"}, Teams: []iamTeam{{"members", []string{"member"}}, {"administrators", []string{"admin"}}}}
	case "clc":
		return iamCatalog{Roles: []string{"viewer", "operator", "admin"}, Teams: []iamTeam{{"viewers", []string{"viewer"}}, {"operators", []string{"operator"}}, {"admins", []string{"admin"}}}}
	case "workflow_manager":
		return iamCatalog{Roles: []string{"viewer", "operator", "admin"},
			RolePermissions: map[string][]string{
				"admin":    {"workflow_manager.read", "workflow_manager.operate", "workflow_manager.cancel"},
				"operator": {"workflow_manager.read", "workflow_manager.operate"},
				"viewer":   {"workflow_manager.read"}},
			Pages: []iamPage{{"/", "Workflows", "workflow_manager.read"}, {"/api/workflows", "Start a workflow", "workflow_manager.operate"},
				{"/api/workflow/signal", "Signal a workflow", "workflow_manager.operate"}, {"/api/workflow/cancel", "Cancel a workflow", "workflow_manager.cancel"}},
			Teams: []iamTeam{{"viewers", []string{"viewer"}}, {"operators", []string{"operator"}}, {"administrators", []string{"admin"}}}}
	}
	return iamCatalog{Roles: []string{"access", "admin"}, Teams: []iamTeam{{"access", []string{"access"}}, {"administrators", []string{"admin"}}}}
}

// defaultCatalogVersion is "r" + the first 12 hex digits of the SHA-256 of the
// compact roles list; it reproduces the business catalog's version. The
// platform catalogs' versions hash more than their roles and come from IAMRuntime.
func defaultCatalogVersion(c iamCatalog) string {
	b, _ := json.Marshal(c.Roles)
	h := sha256.Sum256(b)
	return "r" + hex.EncodeToString(h[:])[:12]
}

// rawToken is a member's module token as declared (underscores kept).
func rawToken(in *RenderInput, member string) string {
	if key, ok := productKey(in, member); ok {
		return key
	}
	if ts := in.Tokens[member]; len(ts) > 0 {
		return ts[0]
	}
	if host, _, _, _, ok := in.Desc.appEntry(member); ok {
		return platformTokens[host]
	}
	return ""
}

func encodeJSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return b.String()
}

// DemoPerson is one [[people]] entry of demo_people.toml.
type DemoPerson struct {
	Email string   `toml:"email"`
	Name  string   `toml:"name"`
	Teams []string `toml:"teams"`
}

// LoadDemoPeople reads demo_people.toml at the cluster root (none → nil).
func LoadDemoPeople(root string) []DemoPerson {
	var doc struct {
		People []DemoPerson `toml:"people"`
	}
	_, _ = toml.DecodeFile(filepath.Join(root, "demo_people.toml"), &doc)
	return doc.People
}

// RenderIAMConfig renders the mycel-iam-configuration ConfigMap: the IAM's
// applications (every switcher app with its access catalog and redirect), its
// identity provider, and the declared people. unknown lists values that only a
// deploy can produce (a new application's key, a new person's subject).
func RenderIAMConfig(in *RenderInput, people []DemoPerson, realm, client string, rt IAMRuntime) (obj Object, unknown []string) {
	issuer := fmt.Sprintf("%s/realms/%s", in.url("keycloak"), realm)
	type app struct {
		ID      string              `json:"application_id"`
		Client  string              `json:"client_id"`
		Product string              `json:"product_id"`
		Catalog string              `json:"catalog_version"`
		Roles   []string            `json:"roles"`
		Perms   map[string][]string `json:"role_permissions,omitempty"`
		Pages   []iamPage           `json:"pages,omitempty"`
		Teams   []iamTeam           `json:"teams"`
		Redir   []string            `json:"redirect_uris"`
		JWK     json.RawMessage     `json:"public_jwk,omitempty"`
	}
	var apps []app
	declared := map[string]bool{}
	for _, s := range in.Desc.Switcher.App {
		host, _, _, _, _ := in.Desc.appEntry(s.App)
		tok := rawToken(in, s.App)
		c := catalogFor(tok)
		fromImage := false
		if ic, ok := in.Catalogs[s.App]; ok {
			c.Roles, c.RolePermissions, fromImage = ic.Roles, ic.RolePermissions, true
			if len(ic.Teams) > 0 {
				c.Teams = ic.Teams
			}
		}
		for _, t := range c.Teams { // an app left as it is still declares its teams
			declared[s.App+"."+t.Name] = true
		}
		if in.Exclude[s.App] != "" { // left as it is: not registered by this deploy
			continue
		}
		version := rt.CatalogVersion[s.App]
		if fromImage && version != defaultCatalogVersion(c) && tok != "mycel_iam" && tok != "kpi" && tok != "clc" && tok != "workflow_manager" {
			// the host checks its sessions against the catalog version it derives from its own image the same way
			// (the compact roles list; role permissions are not part of it), so a business catalog read from an image
			// always carries that version, never a stale live one
			version = defaultCatalogVersion(c)
		}
		if version == "" {
			if tok == "mycel_iam" || tok == "kpi" || tok == "clc" || tok == "workflow_manager" {
				unknown = append(unknown, "catalog version of "+s.App)
			} else {
				version = defaultCatalogVersion(c)
			}
		}
		jwk := rt.PublicJWK[s.App]
		if len(jwk) == 0 {
			unknown = append(unknown, "public key of "+s.App+" (generated at deploy)")
		}
		apps = append(apps, app{ID: s.App, Client: s.App, Product: tok, Catalog: version, Roles: c.Roles, Perms: c.RolePermissions,
			Pages: c.Pages, Teams: c.Teams, Redir: []string{in.url(host) + "/auth/callback"}, JWK: jwk})
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].ID < apps[j].ID })
	applications := encodeJSON(struct {
		Schema       int    `json:"schema_version"`
		Realm        string `json:"realm"`
		Account      string `json:"account"`
		Applications []app  `json:"applications"`
	}{1, in.Namespace, in.Namespace, apps})

	providers := encodeJSON(map[string]any{"providers": []any{map[string]any{
		"admit": "any_verified", "client_id": client, "id": "keycloak", "issuer": issuer, "kind": "oidc",
		"name": fmt.Sprintf("Keycloak (%s)", realm)}}})

	type person struct {
		Subject string   `json:"subject"`
		Issuer  string   `json:"issuer"`
		Email   string   `json:"email"`
		Name    string   `json:"name"`
		Teams   []string `json:"teams"`
	}
	var ps []person
	for _, p := range people {
		var teams []string
		for _, t := range p.Teams {
			if declared[t] {
				teams = append(teams, t)
			}
		}
		ps = append(ps, person{Subject: DemoSubject(realm, p.Email), Issuer: issuer, Email: p.Email, Name: p.Name, Teams: teams})
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Email < ps[j].Email })
	seed := encodeJSON(struct {
		Schema  int      `json:"schema_version"`
		Realm   string   `json:"realm"`
		Account string   `json:"account"`
		People  []person `json:"people"`
	}{1, in.Namespace, in.Namespace, ps})

	ann := map[string]any{}
	if rt.Annotation != "" {
		ann["mycel.dev/iam-configuration"] = rt.Annotation
	}
	return Object{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"annotations": ann, "name": "mycel-iam-configuration", "namespace": in.Namespace},
		"data":     map[string]any{"applications.json": applications, "providers.json": providers, "seed.json": seed}}, unknown
}

// dnsNamespace is the RFC 4122 DNS namespace (6ba7b810-9dad-11d1-80b4-00c04fd430c8).
var dnsNamespace = [16]byte{0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}

// DemoSubject is the subject a declared demo person gets in the identity
// provider: a version-5 UUID in the DNS namespace of the realm, a zero byte and
// the lower-cased email (SPEC.md Appendix C.10).
func DemoSubject(realm, email string) string {
	h := sha1.New()
	h.Write(dnsNamespace[:])
	h.Write([]byte(realm + "\x00" + strings.ToLower(email)))
	u := h.Sum(nil)[:16]
	u[6] = u[6]&0x0f | 0x50 // version 5
	u[8] = u[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
