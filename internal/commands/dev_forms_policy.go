package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"axon/internal/descriptor"
	"axon/internal/iamadmin"
	"axon/internal/ui"
)

// formsGrant is one Forms Policy grant: a principal may do action on a resource (resource_id "*" for every one).
type formsGrant struct {
	Tenant       string `json:"tenant"`
	Principal    string `json:"principal"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
	Action       string `json:"action"`
}

func (g formsGrant) key() string {
	return g.Principal + " " + g.ResourceType + ":" + g.ResourceID + ":" + g.Action
}

// wantedFormsGrants expands the overlay's [forms_policy] grants. A service principal's Forms Policy subject is
// "service:<client-id>", the subject its IAM token carries.
func wantedFormsGrants(cfg *descriptor.FormsPolicyGrants) ([]formsGrant, error) {
	var out []formsGrant
	for principal, specs := range cfg.Grants {
		for _, spec := range specs {
			parts := strings.Split(spec, ":")
			if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
				return nil, fmt.Errorf("[forms_policy] grant %q of %s: want <resource_type>:<resource_id>:<action>", spec, principal)
			}
			out = append(out, formsGrant{Tenant: cfg.Tenant, Principal: "service:" + principal,
				ResourceType: parts[0], ResourceID: parts[1], Action: parts[2]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out, nil
}

// formsPolicyCall sends one request to the Forms Policy grants route as the signed-in deploy actor.
func formsPolicyCall(c *iamadmin.Client, origin, method, tenant string, body any) (int, []byte, error) {
	target := strings.TrimRight(origin, "/") + "/m/forms_policy/grants"
	var rd io.Reader
	if method == http.MethodGet {
		target += "?" + url.Values{"tenant": {tenant}}.Encode()
	} else {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, target, rd)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Forms-Tenant", tenant)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", strings.TrimRight(origin, "/"))
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if !strings.HasPrefix(resp.Request.URL.String(), strings.TrimRight(origin, "/")) {
		return http.StatusUnauthorized, nil, nil // sent to sign in: the session did not reach this app
	}
	return resp.StatusCode, b, nil
}

// provisionFormsPolicy gives each service principal the Forms Policy grants the overlay lists, adding only the
// missing ones. The deploy actor must hold policy_grant admin in the tenant. Nothing here fails the deploy: a refusal
// is reported with what to do.
func provisionFormsPolicy(cfg *descriptor.FormsPolicyGrants, origins map[string]string, iamOrigin, actor, password string) {
	if cfg == nil || len(cfg.Grants) == 0 {
		return
	}
	ui.Step("Forms Policy grants (tenant %s)", cfg.Tenant)
	want, err := wantedFormsGrants(cfg)
	if err != nil {
		ui.Warn("%v", err)
		return
	}
	origin := origins[cfg.App]
	if cfg.Tenant == "" || origin == "" {
		ui.Warn("[forms_policy] needs tenant and app (a hosted member; %q has no front door)", cfg.App)
		return
	}
	if actor == "" || password == "" {
		ui.Warn("[fabric.console] push_actor and push_password are needed to add Forms Policy grants")
		return
	}
	c, err := iamadmin.SignIn(iamOrigin, actor, password)
	if err != nil {
		ui.Warn("sign in to %s as %s: %v", iamOrigin, actor, err)
		return
	}
	if err := signInApp(c, origin); err != nil {
		ui.Warn("Forms Policy at %s: %v; grant them in Forms Policy with policy_grant admin in %s", origin, err, cfg.Tenant)
		return
	}
	var status int
	var body []byte
	for i := 0; i < 10; i++ { // the app may still be rolling out
		if status, body, err = formsPolicyCall(c, origin, http.MethodGet, cfg.Tenant, nil); err == nil && status == http.StatusOK {
			break
		}
		time.Sleep(6 * time.Second)
	}
	if err != nil || status != http.StatusOK {
		// Forms Policy knows a person by the token's subject (their IAM principal ID), not by email.
		subject := actor
		if a, aerr := c.ReadAccount(); aerr == nil {
			for _, p := range a.People {
				if strings.EqualFold(p.Email, actor) && p.ID != "" {
					subject = p.ID
				}
			}
		}
		ui.Warn("Forms Policy at %s: cannot list grants as %s (%s: %s). Give principal %q the grant policy_grant:*:admin in tenant %s, then deploy again",
			origin, actor, or(errText(err), fmt.Sprint("HTTP ", status)), strings.TrimSpace(string(body)), subject, cfg.Tenant)
		return
	}
	var page struct {
		Grants []formsGrant `json:"grants"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		ui.Warn("Forms Policy at %s: unreadable grant list: %v", origin, err)
		return
	}
	have := map[string]bool{}
	for _, g := range page.Grants {
		if g.Tenant == cfg.Tenant {
			have[g.key()] = true
		}
	}
	added, failed := 0, 0
	for _, g := range want {
		if have[g.key()] {
			continue
		}
		st, b, err := formsPolicyCall(c, origin, http.MethodPost, cfg.Tenant, g)
		if err != nil || (st != http.StatusOK && st != http.StatusCreated) {
			ui.Warn("Forms Policy: grant %s %s:%s:%s refused (%s): %s", g.Principal, g.ResourceType, g.ResourceID, g.Action,
				or(errText(err), fmt.Sprint("HTTP ", st)), strings.TrimSpace(string(b)))
			failed++
			continue
		}
		ui.Sub("granted %s %s:%s on %s", g.Principal, g.ResourceType, g.Action, g.ResourceID)
		added++
	}
	if failed == 0 {
		ui.OK("Forms Policy: %d grant(s) in place, %d added", len(want), added)
	}
}

// signInApp opens a page of an app so its own sign-in runs: an app core answers an API request without its session
// with 401, but sends a page request through IAM's authorize step, which the client's IAM session completes, and back
// to the app with that app's session.
func signInApp(c *iamadmin.Client, origin string) error {
	origin = strings.TrimRight(origin, "/")
	var last string
	for i := 0; i < 10; i++ { // the app may still be rolling out
		req, _ := http.NewRequest(http.MethodGet, origin+"/", nil)
		req.Header.Set("Accept", "text/html")
		resp, err := c.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if strings.HasPrefix(resp.Request.URL.String(), origin) && resp.StatusCode < 500 {
				return nil
			}
			last = fmt.Sprintf("sign-in ended at %s (HTTP %d)", resp.Request.URL.Host, resp.StatusCode)
		} else {
			last = err.Error()
		}
		time.Sleep(6 * time.Second)
	}
	return fmt.Errorf("cannot sign in to the app: %s", last)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
