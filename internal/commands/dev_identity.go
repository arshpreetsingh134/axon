package commands

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"axon/internal/iamadmin"
	"axon/internal/official"
	"axon/internal/sh"
	"axon/internal/ui"
)

// devIdentity is a workflow client (a project member) that signs in to the cluster IAM as a service principal with
// private_key_jwt (SPEC.md Appendix C.13). It is declared by the member's environment, the workflow client's own
// settings: WFC_SERVICE_CLIENT_ID (the principal), WFC_SERVICE_KEY_FILE (where it reads its private key) and
// WFC_SERVICE_AUDIENCES (a JSON object, origin → application ID: the sites it calls).
type devIdentity struct {
	Member   string
	ClientID string
	KeyFile  string
	Sites    []string
	Issuer   string // WFC_IAM_ISSUER, the issuer the worker's assertion names
	Secret   string
	// PerAudience (WFC_SERVICE_CLIENT_PER_AUDIENCE=1): one principal per site, "<ClientID>--<suffix>" with the suffix
	// the site's last dot-separated segment, each granted on its site only and with its own key (data key
	// "<suffix>.pem" of the Secret, read by the worker beside its key file). Mycel IAM 0.2.0-rc.28 refuses, at the
	// host's session check, the sessions of a service principal granted on more than one site; it refuses IDs longer
	// than 64 characters and a public key another principal already registered.
	PerAudience bool
}

// principal is one service principal of an identity: its ID, the Secret data key holding its private key, and the
// sites it is granted on.
type principal struct {
	ID, KeyName string
	Sites       []string
}

// siteSuffix is the last dot-separated segment of an application ID.
func siteSuffix(site string) string {
	if i := strings.LastIndex(site, "."); i >= 0 {
		return site[i+1:]
	}
	return site
}

// principals are the service principals an identity needs.
func (id devIdentity) principals() []principal {
	if !id.PerAudience {
		return []principal{{ID: id.ClientID, KeyName: "key.pem", Sites: id.Sites}}
	}
	var out []principal
	for _, s := range id.Sites {
		out = append(out, principal{ID: id.ClientID + "--" + siteSuffix(s), KeyName: siteSuffix(s) + ".pem", Sites: []string{s}})
	}
	return out
}

// extraKeys are the Secret data keys mounted beside the key file: one per principal when there is one per site.
func (id devIdentity) extraKeys() []string {
	var out []string
	for _, p := range id.principals() {
		if p.KeyName != "key.pem" {
			out = append(out, p.KeyName)
		}
	}
	return out
}

// devIdentities reads the workflow clients of the descriptor, with the environment's overrides applied.
func devIdentities(desc *official.Descriptor, overrides map[string]map[string]string) []devIdentity {
	var out []devIdentity
	for _, a := range desc.App {
		if a.Kind != "project" {
			continue
		}
		env := map[string]string{}
		for k, v := range a.Env {
			env[k] = v
		}
		for k, v := range overrides[a.Name] {
			env[k] = v
		}
		id, file := strings.TrimSpace(env["WFC_SERVICE_CLIENT_ID"]), strings.TrimSpace(env["WFC_SERVICE_KEY_FILE"])
		if id == "" || file == "" || strings.Contains(id, "${") || strings.Contains(file, "${") {
			continue
		}
		var aud, mapped map[string]string
		_ = json.Unmarshal([]byte(env["WFC_SERVICE_AUDIENCES"]), &aud)
		perAudience := strings.TrimSpace(env["WFC_SERVICE_CLIENT_PER_AUDIENCE"]) == "1"
		if !perAudience {
			// targets of the identity map have their own principals (serviceClients, SPEC.md Appendix C.14)
			_ = json.Unmarshal([]byte(env["WFC_SERVICE_IDENTITIES"]), &mapped)
		}
		seen := map[string]bool{}
		var sites []string
		for origin, s := range aud {
			if _, own := mapped[origin]; own {
				continue
			}
			if s = strings.TrimSpace(s); s != "" && !seen[s] {
				seen[s] = true
				sites = append(sites, s)
			}
		}
		sort.Strings(sites)
		if len(sites) == 0 && len(mapped) > 0 {
			continue // every target has its own principal: the default one is not used
		}
		out = append(out, devIdentity{Member: a.Name, ClientID: id, KeyFile: file, Sites: sites, Issuer: strings.TrimSpace(env["WFC_IAM_ISSUER"]),
			Secret: official.K8sName(a.Name) + "-service-key", PerAudience: perAudience})
	}
	return out
}

// ensureKeySecrets makes sure every identity's private key Secret exists and holds a key for each of its principals
// (a new P-256 key for each one missing), so the rendered worker can mount them. It returns the identities whose
// Secret changed now.
func ensureKeySecrets(ids []devIdentity, ctx, ns string, dry bool) (created map[string]bool, err error) {
	created = map[string]bool{}
	for _, id := range ids {
		want := append([]string{"key.pem"}, id.extraKeys()...)
		have, exists := secretKeys(ctx, ns, id.Secret)
		data := map[string]any{}
		for _, k := range want {
			if have[k] {
				continue
			}
			if dry {
				ui.Sub("would add a new private key %s to Secret %s for %s", k, id.Secret, id.ClientID)
				continue
			}
			key, err := iamadmin.NewKey()
			if err != nil {
				return nil, err
			}
			data[k] = base64.StdEncoding.EncodeToString(key)
		}
		if len(data) == 0 {
			continue
		}
		var res *sh.Result
		if exists {
			patch, _ := json.Marshal(map[string]any{"data": data})
			res, err = sh.Run(sh.Cmd{Name: "kubectl", Args: []string{"--context", ctx, "-n", ns, "patch", "secret", id.Secret, "--type", "merge", "-p", string(patch)}})
		} else {
			obj, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
				"metadata": map[string]any{"name": id.Secret, "namespace": ns, "labels": map[string]any{"app": official.K8sName(id.Member)}},
				"data":     data})
			res, err = sh.Run(sh.Cmd{Name: "kubectl", Args: []string{"--context", ctx, "-n", ns, "create", "-f", "-"}, Stdin: string(obj)})
		}
		if err != nil {
			msg := ""
			if res != nil {
				msg = strings.TrimSpace(res.Stderr)
			}
			return nil, fmt.Errorf("write Secret %s: %w\n%s", id.Secret, err, msg)
		}
		ui.Sub("Secret %s: %d new private key(s) for %s", id.Secret, len(data), id.ClientID)
		created[id.Member] = true
	}
	return created, nil
}

// secretKeys are the data keys of a Secret, and whether it exists.
func secretKeys(ctx, ns, secret string) (map[string]bool, bool) {
	out, err := sh.Output("kubectl", "--context", ctx, "-n", ns, "get", "secret", secret, "-o", "json")
	if err != nil {
		return map[string]bool{}, false
	}
	var s struct {
		Data map[string]string `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &s)
	have := map[string]bool{}
	for k := range s.Data {
		have[k] = true
	}
	return have, true
}

func readKey(ctx, ns, secret, name string) ([]byte, error) {
	out, err := sh.Output("kubectl", "--context", ctx, "-n", ns, "get", "secret", secret, "-o", "jsonpath={.data."+strings.ReplaceAll(name, ".", "\\.")+"}")
	if err != nil || strings.TrimSpace(out) == "" {
		return nil, fmt.Errorf("read %s of Secret %s: %v", name, secret, err)
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(out))
}

// provisionIdentities registers each identity in the cluster IAM as the cluster's console actor (an account
// administrator the cluster's own provisioning seeds): the service account with its public key, owned by that actor,
// and on each site it calls the catalog roles that carry workflow.call. Then it asks the token endpoint for a token per
// site, which proves the key and the grants. Idempotent: a re-run changes nothing that is already in place.
func provisionIdentities(ids []devIdentity, iamOrigin, actor, password, ctx, ns string) error {
	if len(ids) == 0 {
		return nil
	}
	if actor == "" || password == "" {
		return fmt.Errorf("[fabric.console] push_actor and push_password are needed to register workflow clients in the cluster IAM")
	}
	var c *iamadmin.Client
	var err error
	for i := 0; i < 20; i++ { // the IAM may still be restarting after its configuration changed
		if c, err = iamadmin.SignIn(iamOrigin, actor, password); err == nil {
			break
		}
		time.Sleep(6 * time.Second)
	}
	if err != nil {
		return fmt.Errorf("sign in to %s as %s: %w", iamOrigin, actor, err)
	}
	for _, id := range ids {
		ui.Step("workflow client %s (%s)", id.ClientID, id.Member)
		ok, total := 0, 0
		for _, pr := range id.principals() {
			key, err := readKey(ctx, ns, id.Secret, pr.KeyName)
			if err != nil {
				return err
			}
			n, err := provisionPrincipal(c, pr.ID, pr.Sites, workflowCallRoles, or(id.Issuer, iamOrigin), key, iamOrigin, actor)
			if err != nil {
				return err
			}
			ok += n
			total += len(pr.Sites)
		}
		ui.OK("%s gets tokens for %d of %d site(s) it calls", id.ClientID, ok, total)
	}
	return nil
}

// imageCatalogs reads each member image's access catalog label (io.mycel.module.access): from the local image store,
// else from the registry without pulling. A missing label keeps the observed default; a malformed one is reported
// (the deploy of such an image would be refused) and also keeps the default here.
func imageCatalogs(images map[string]string) map[string]official.Catalog {
	out := map[string]official.Catalog{}
	for member, img := range images {
		label, err := sh.Output("docker", "image", "inspect", "--format", `{{index .Config.Labels "io.mycel.module.access"}}`, img)
		if err != nil {
			raw, rerr := sh.Output("docker", "buildx", "imagetools", "inspect", img, "--format", "{{json .Image.Config.Labels}}")
			if rerr != nil {
				continue
			}
			var labels map[string]string
			if json.Unmarshal([]byte(raw), &labels) != nil {
				continue
			}
			label = labels["io.mycel.module.access"]
		}
		label = strings.TrimSpace(label)
		if label == "" || label == "<no value>" {
			continue
		}
		c, err := official.ParseCatalog(label)
		if err != nil {
			ui.Warn("%s: %v (image %s): its deploy would be refused", member, err, img)
			continue
		}
		out[member] = c
	}
	return out
}

// roleChooser picks the catalog roles to grant on a site from its roles (code → permissions); "" or a reason when none.
type roleChooser func(site string, catalog map[string][]string) ([]string, string)

// workflowCallRoles are the roles that carry workflow.call: what a workflow worker needs on the apps it calls.
func workflowCallRoles(_ string, catalog map[string][]string) ([]string, string) {
	var out []string
	for code, perms := range catalog {
		for _, p := range perms {
			if p == "workflow.call" {
				out = append(out, code)
			}
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, "no role of its catalog grants workflow.call"
	}
	return out, ""
}

// provisionPrincipal registers one service principal with exactly this environment's key, grants it the roles choose
// picks on each of its sites, and returns the number of sites it gets a token for.
func provisionPrincipal(c *iamadmin.Client, principal string, sites []string, choose roleChooser, issuer string, key []byte, iamOrigin, actor string) (int, error) {
	pub, err := iamadmin.PublicPEM(key)
	if err != nil {
		return 0, err
	}
	thumb, err := iamadmin.Thumbprint(key)
	if err != nil {
		return 0, err
	}
	svcs, err := c.Services()
	if err != nil {
		return 0, err
	}
	var svc *iamadmin.Service
	for i, s := range svcs {
		if s.ID == principal || s.ClientID == principal {
			svc = &svcs[i]
		}
	}
	switch {
	case svc == nil:
		if _, err := c.Act("", "create_service", url.Values{"service": {principal}, "name": {principal},
			"purpose": {"Workflow client of this development environment (registered by its deploy)"}, "owner": {actor}, "public_key": {pub}}); err != nil {
			return 0, err
		}
		ui.Sub("registered service principal %s (owner %s)", principal, actor)
	default:
		have := false
		for _, k := range svc.Keys {
			have = have || k.Thumbprint == thumb
		}
		if !have {
			if _, err := c.Act("", "add_service_key", url.Values{"service": {principal}, "public_key": {pub}}); err != nil {
				return 0, err
			}
			ui.Sub("added this environment's key to %s", principal)
		}
		for _, k := range svc.Keys {
			if k.Thumbprint != thumb {
				if _, err := c.Act("", "revoke_service_key", url.Values{"service": {principal}, "thumbprint": {k.Thumbprint}}); err != nil {
					return 0, err
				}
				ui.Sub("revoked an earlier key of %s", principal)
			}
		}
	}
	var granted []string
	for _, site := range sites {
		s, err := c.ReadSite(site)
		if err != nil {
			ui.Warn("%s: cannot read site %s: %v", principal, site, err)
			continue
		}
		catalog := map[string][]string{}
		holding := map[string]bool{}
		for _, r := range s.Roles {
			catalog[r.Code] = r.Permissions
			for _, p := range r.People {
				if p.ID == principal {
					holding[r.Code] = true
				}
			}
		}
		roles, why := choose(site, catalog)
		var known []string
		missing := false
		held := len(roles) > 0
		for _, r := range roles {
			if _, ok := catalog[r]; !ok {
				ui.Warn("%s: role %s is not in the catalog of %s", principal, r, site)
				held = false
				continue
			}
			known = append(known, r)
			missing = missing || !holding[r]
		}
		if missing {
			// IAM's grant sets the principal's role list on the site: send every role it should hold, not only the
			// missing ones, which would drop the roles it already holds.
			if _, err := c.Act(site, "grant", url.Values{"target": {principal}, "role": known}); err != nil {
				return 0, err
			}
			ui.Sub("granted %s on %s to %s", strings.Join(known, ", "), site, principal)
			held = true
		}
		if !held {
			ui.Sub("%s on %s: %s; not granted", principal, site, or(why, "no role to grant"))
			continue
		}
		granted = append(granted, site)
	}
	ok := 0
	for _, site := range granted {
		if err := iamadmin.TokenCheck(iamOrigin, issuer, principal, site, key); err != nil {
			ui.Warn("%s: no token for %s: %v", principal, site, err)
			continue
		}
		ok++
	}
	return ok, nil
}
