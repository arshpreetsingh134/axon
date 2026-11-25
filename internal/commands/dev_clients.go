package commands

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"axon/internal/iamadmin"
	"axon/internal/official"
	"axon/internal/sh"
	"axon/internal/ui"
)

// serviceClient is one service principal a member signs in as to call one other application (SPEC.md Appendix
// C.14): a target of a workflow client's identity map, or an app-to-app client. Each is single-site with its own key.
type serviceClient struct {
	Member    string // the member whose environment declares it
	Principal string // its client ID
	Site      string // the application it calls (token audience)
	KeyRef    string // the app-env Secret key its base64 PEM comes from (`${NAME}`)
	Worker    bool   // a workflow worker's identity: falls back to the roles that carry workflow.call
}

var (
	clientIDVar  = regexp.MustCompile(`^([A-Z0-9_]+?)_CLIENT_ID$`)
	secretRefVal = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)
)

// memberEnv is a member's environment with the environment's overrides applied.
func memberEnv(name string, base map[string]string, overrides map[string]map[string]string) map[string]string {
	env := map[string]string{}
	for k, v := range base {
		env[k] = v
	}
	for k, v := range overrides[name] {
		env[k] = v
	}
	return env
}

// serviceClients reads every single-site service client the descriptor's members declare:
//   - a workflow client's identity map: WFC_SERVICE_IDENTITIES (origin → env prefix) with WFC_SERVICE_AUDIENCES
//     (origin → application), <prefix>CLIENT_ID and <prefix>KEY_PEM_BASE64;
//   - an app-to-app client: <T>_CLIENT_ID with <T>_CLIENT_AUDIENCE and <T>_CLIENT_KEY_PEM_BASE64.
//
// Only keys given as an app-env Secret reference (`${NAME}`) are managed; a client whose ID or audience is still a
// reference, or whose key is a file or inline value, is reported in notes and left alone.
func serviceClients(desc *official.Descriptor, overrides map[string]map[string]string) (out []serviceClient, notes []string) {
	add := func(c serviceClient, keyVal string) {
		if strings.Contains(c.Principal, "${") || strings.Contains(c.Site, "${") || c.Principal == "" || c.Site == "" {
			return
		}
		m := secretRefVal.FindStringSubmatch(strings.TrimSpace(keyVal))
		if m == nil {
			notes = append(notes, fmt.Sprintf("%s: %s: its key is not an app-env Secret reference (${NAME}); not managed", c.Member, c.Principal))
			return
		}
		c.KeyRef = m[1]
		out = append(out, c)
	}
	for _, a := range desc.App {
		env := memberEnv(a.Name, a.Env, overrides)
		if strings.TrimSpace(env["WFC_SERVICE_CLIENT_PER_AUDIENCE"]) != "1" && strings.TrimSpace(env["WFC_SERVICE_IDENTITIES"]) != "" {
			var prefixes, audiences map[string]string
			_ = json.Unmarshal([]byte(env["WFC_SERVICE_IDENTITIES"]), &prefixes)
			_ = json.Unmarshal([]byte(env["WFC_SERVICE_AUDIENCES"]), &audiences)
			for origin, prefix := range prefixes {
				add(serviceClient{Member: a.Name, Principal: strings.TrimSpace(env[prefix+"CLIENT_ID"]), Site: strings.TrimSpace(audiences[origin]), Worker: true},
					env[prefix+"KEY_PEM_BASE64"])
			}
		}
		for k, v := range env {
			m := clientIDVar.FindStringSubmatch(k)
			if m == nil || strings.HasPrefix(k, "WFC_") {
				continue
			}
			aud, ok := env[m[1]+"_CLIENT_AUDIENCE"]
			if !ok {
				continue
			}
			add(serviceClient{Member: a.Name, Principal: strings.TrimSpace(v), Site: strings.TrimSpace(aud)}, env[m[1]+"_CLIENT_KEY_PEM_BASE64"])
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Principal != out[j].Principal {
			return out[i].Principal < out[j].Principal
		}
		return out[i].Member < out[j].Member
	})
	// One principal must call one site (IAM 0.2.0-rc.28 refuses multi-site service sessions), with one key.
	seen := map[string]serviceClient{}
	var uniq []serviceClient
	for _, c := range out {
		if p, dup := seen[c.Principal]; dup {
			if p.Site != c.Site || p.KeyRef != c.KeyRef {
				notes = append(notes, fmt.Sprintf("%s is declared for %s (%s) and %s (%s): one principal must call one site with one key; only the first is managed",
					c.Principal, p.Site, p.Member, c.Site, c.Member))
			}
			continue
		}
		seen[c.Principal] = c
		uniq = append(uniq, c)
	}
	return uniq, notes
}

// chooseClientRoles picks what a client is granted on its site: the overlay's [service_roles] for it; else the
// catalog role named like the principal; else the catalog's only role that carries permissions (besides access and
// admin); else, for a worker identity, the roles that carry workflow.call. Anything else is ambiguous: not granted.
func chooseClientRoles(c serviceClient, declared map[string][]string) roleChooser {
	return func(site string, catalog map[string][]string) ([]string, string) {
		if roles, ok := declared[c.Principal]; ok {
			return roles, ""
		}
		if _, ok := catalog[c.Principal]; ok {
			return []string{c.Principal}, ""
		}
		var scoped []string
		for code, perms := range catalog {
			if code != "access" && code != "admin" && len(perms) > 0 {
				scoped = append(scoped, code)
			}
		}
		sort.Strings(scoped)
		if len(scoped) == 1 {
			return scoped, ""
		}
		if c.Worker {
			if roles, _ := workflowCallRoles(site, catalog); len(roles) > 0 {
				return roles, ""
			}
		}
		return nil, fmt.Sprintf("the catalog offers %s; name the role in [service_roles] of the overlay", strings.Join(scoped, ", "))
	}
}

// ensureClientKeys makes sure every client's key reference holds a key in the app-env Secret the render points at
// (mycel-app-env): a new P-256 key, as base64 of its PEM, for each one missing or empty. It runs before the render,
// so pods never start without their key.
func ensureClientKeys(clients []serviceClient, ctx, ns string, dry bool) error {
	have := map[string]bool{}
	if out, err := sh.Output("kubectl", "--context", ctx, "-n", ns, "get", "secret", "mycel-app-env", "-o", "json"); err == nil {
		var s struct {
			Data map[string]string `json:"data"`
		}
		_ = json.Unmarshal([]byte(out), &s)
		for k, v := range s.Data {
			have[k] = strings.TrimSpace(v) != ""
		}
	}
	data := map[string]string{}
	for _, c := range clients {
		if have[c.KeyRef] || data[c.KeyRef] != "" {
			continue
		}
		if dry {
			ui.Sub("would add a new private key %s to Secret mycel-app-env for %s", c.KeyRef, c.Principal)
			continue
		}
		key, err := iamadmin.NewKey()
		if err != nil {
			return err
		}
		data[c.KeyRef] = base64.StdEncoding.EncodeToString([]byte(base64.StdEncoding.EncodeToString(key)))
	}
	if len(data) == 0 {
		return nil
	}
	patch, _ := json.Marshal(map[string]any{"data": data})
	var res *sh.Result
	var err error
	if len(have) > 0 {
		res, err = sh.Run(sh.Cmd{Name: "kubectl", Args: []string{"--context", ctx, "-n", ns, "patch", "secret", "mycel-app-env", "--type", "merge", "-p", string(patch)}})
	} else {
		obj, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
			"metadata": map[string]any{"name": "mycel-app-env", "namespace": ns}, "data": data})
		res, err = sh.Run(sh.Cmd{Name: "kubectl", Args: []string{"--context", ctx, "-n", ns, "create", "-f", "-"}, Stdin: string(obj)})
	}
	if err != nil {
		msg := ""
		if res != nil {
			msg = strings.TrimSpace(res.Stderr)
		}
		return fmt.Errorf("write Secret mycel-app-env: %w\n%s", err, msg)
	}
	ui.Sub("Secret mycel-app-env: %d new service client key(s)", len(data))
	return nil
}

// clientKey reads a client's private key PEM from the app-env Secret.
func clientKey(ctx, ns, ref string) ([]byte, error) {
	b64, err := readKey(ctx, ns, "mycel-app-env", ref)
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(string(b64)))
}

// provisionClients registers each client in the cluster IAM (its own key, its site's roles) and checks a token.
func provisionClients(clients []serviceClient, declared map[string][]string, iamOrigin, issuer, actor, password, ctx, ns string) error {
	if len(clients) == 0 {
		return nil
	}
	if actor == "" || password == "" {
		return fmt.Errorf("[fabric.console] push_actor and push_password are needed to register service clients in the cluster IAM")
	}
	var c *iamadmin.Client
	var err error
	for i := 0; i < 20; i++ {
		if c, err = iamadmin.SignIn(iamOrigin, actor, password); err == nil {
			break
		}
		time.Sleep(6 * time.Second)
	}
	if err != nil {
		return fmt.Errorf("sign in to %s as %s: %w", iamOrigin, actor, err)
	}
	ui.Step("service clients (%d)", len(clients))
	ok := 0
	for _, sc := range clients {
		key, err := clientKey(ctx, ns, sc.KeyRef)
		if err != nil {
			ui.Warn("%s: %v", sc.Principal, err)
			continue
		}
		n, err := provisionPrincipal(c, sc.Principal, []string{sc.Site}, chooseClientRoles(sc, declared), issuer, key, iamOrigin, actor)
		if err != nil {
			return fmt.Errorf("%s: %w", sc.Principal, err)
		}
		ok += n
	}
	ui.OK("%d of %d service client(s) get a token for their site", ok, len(clients))
	return nil
}
