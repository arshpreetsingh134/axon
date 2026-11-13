// Package check produces findings: toolchain probes, descriptor conformance,
// generated-config drift, security posture and live-cluster function checks.
package check

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"axon/internal/descriptor"
	"axon/internal/sh"
	"axon/internal/ui"
)

// Finding is one result. IDs are stable for tooling.
type Finding struct {
	ID       string `json:"id"`
	Concern  string `json:"concern"`
	Severity string `json:"severity"` // blocking | risk | note
	Subject  string `json:"subject"`
	Cause    string `json:"cause"`
	Fix      string `json:"fix,omitempty"`
}

// Concerns in report order.
var Concerns = []string{"toolchain", "conformance", "function", "performance", "business", "security", "devx"}

// FromProblems converts descriptor problems to findings.
func FromProblems(ps []descriptor.Problem) []Finding {
	var out []Finding
	for _, p := range ps {
		concern := "conformance"
		if strings.HasPrefix(p.ID, "SECURITY_") {
			concern = "security"
		}
		out = append(out, Finding{ID: p.ID, Concern: concern, Severity: p.Severity, Subject: p.Subject, Cause: p.Cause, Fix: p.Fix})
	}
	return out
}

// Tool is one toolchain row.
type Tool struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Version string `json:"version"`
	Path    string `json:"path"`
}

// Toolchain probes the tools axon shells out to.
func Toolchain() []Tool {
	probes := []struct {
		name string
		args []string
	}{
		{"go", []string{"version"}},
		{"cargo", []string{"--version"}},
		{"node", []string{"--version"}},
		{"npm", []string{"--version"}},
		{"docker", []string{"--version"}},
		{"kubectl", []string{"version", "--client", "-o", "yaml"}},
		{"git", []string{"--version"}},
		{"k6", []string{"version"}},
	}
	var out []Tool
	for _, p := range probes {
		t := Tool{Name: p.name}
		if path, err := exec.LookPath(p.name); err == nil {
			t.Path, t.OK = path, true
			if r, err := sh.Run(sh.Cmd{Name: p.name, Args: p.args, Timeout: 10 * time.Second}); err == nil {
				t.Version = firstVersion(r.Stdout + r.Stderr)
			}
		}
		out = append(out, t)
	}
	return out
}

func firstVersion(s string) string {
	for _, f := range strings.Fields(s) {
		f = strings.Trim(f, "v,\"")
		f = strings.TrimPrefix(f, "go")
		if len(f) > 0 && f[0] >= '0' && f[0] <= '9' && strings.Contains(f, ".") {
			return f
		}
	}
	return strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
}

// ToolchainFindings turns probes into findings for the tools a target needs.
func ToolchainFindings(target string) []Finding {
	var out []Finding
	if !sh.Has("docker") {
		out = append(out, Finding{ID: "TOOLCHAIN_DOCKER_MISSING", Concern: "toolchain", Severity: "blocking", Subject: "docker", Cause: "docker is not on PATH", Fix: "install Docker Desktop or OrbStack"})
	} else if _, err := sh.Run(sh.Cmd{Name: "docker", Args: []string{"info", "--format", "{{.ServerVersion}}"}, Timeout: 15 * time.Second}); err != nil {
		out = append(out, Finding{ID: "TOOLCHAIN_DOCKER_DAEMON_DOWN", Concern: "toolchain", Severity: "blocking", Subject: "docker", Cause: "the docker daemon does not answer", Fix: "start Docker/OrbStack"})
	}
	if target == "k8s" {
		if !sh.Has("kubectl") {
			out = append(out, Finding{ID: "TOOLCHAIN_KUBECTL_MISSING", Concern: "toolchain", Severity: "blocking", Subject: "kubectl", Cause: "kubectl is not on PATH", Fix: "brew install kubectl"})
		} else if ctx, err := sh.Output("kubectl", "config", "current-context"); err != nil || ctx == "" {
			out = append(out, Finding{ID: "TOOLCHAIN_KUBE_CONTEXT_UNSET", Concern: "toolchain", Severity: "blocking", Subject: "kubectl", Cause: "no current kube context", Fix: "set `context` in the env file or `kubectl config use-context`"})
		} else if _, err := sh.Run(sh.Cmd{Name: "kubectl", Args: []string{"get", "ns", "--request-timeout=4s"}, Timeout: 8 * time.Second}); err != nil {
			out = append(out, Finding{ID: "TOOLCHAIN_KUBE_UNREACHABLE", Concern: "toolchain", Severity: "blocking", Subject: ctx, Cause: "the cluster API does not answer", Fix: "start the Kubernetes cluster"})
		}
	}
	return out
}

// Security returns static security findings for a cluster.
func Security(m *descriptor.Model, gitignored func(string) bool) []Finding {
	var out []Finding
	add := func(id, sev, subj, cause, fix string) {
		out = append(out, Finding{ID: id, Concern: "security", Severity: sev, Subject: subj, Cause: cause, Fix: fix})
	}
	c := m.C
	envPath := filepath.Join(m.Infra, ".env")
	envVals := readEnv(envPath)
	if _, ok := envVals["POSTGRES_PASSWORD"]; !ok && m.L.Runtime() == "mycel" {
		add("SECURITY_DEFAULT_DB_CREDENTIALS", "risk", "fabric.postgres", "the database uses the default credentials (mycel/mycel)", "set POSTGRES_PASSWORD in .env before the first deploy of a new cluster")
	}
	if _, ok := envVals["KEYCLOAK_ADMIN_PASSWORD"]; !ok && c.Fabric.Keycloak != nil {
		add("SECURITY_ADMIN_PASSWORD", "note", "fabric.keycloak", "the Keycloak admin password is the default (admin)", "set KEYCLOAK_ADMIN_PASSWORD in .env (and in scripts/provision-keycloak.sh)")
	}
	if kc := c.Fabric.Keycloak; kc != nil && !strings.HasPrefix(kc.Secret, "${") && !envVals["OIDC_CLIENT_SECRET"] && (kc.Secret == "" || strings.Contains(kc.Secret, c.Name) || kc.Secret == "secret") {
		add("SECURITY_PREDICTABLE_OIDC_SECRET", "risk", "fabric.keycloak", "the OIDC client secret is predictable and committed in the descriptor", "set OIDC_CLIENT_SECRET in .env (it overrides [fabric.keycloak].secret)")
	}
	key := filepath.Join(m.Infra, "secrets", "iam_signing_key.pem")
	if iam := c.Fabric.IAM; iam != nil && iam.SigningKey != "" {
		key = filepath.Join(m.Dir, iam.SigningKey)
	}
	if st, err := os.Stat(key); err == nil {
		if st.Mode().Perm()&0o077 != 0 {
			add("SECURITY_SIGNING_KEY_PERMISSIONS", "blocking", rel(m.Dir, key), fmt.Sprintf("mode %o; the IAM signing key must be 0600", st.Mode().Perm()), "chmod 600 "+rel(m.Dir, key))
		}
		if gitignored != nil && !gitignored(key) {
			add("SECURITY_SIGNING_KEY_NOT_IGNORED", "blocking", rel(m.Dir, key), "the IAM signing key is not gitignored", "add it to .gitignore")
		}
	}
	for _, old := range []string{strings.TrimSuffix(key, ".pem") + ".prev.pem", key + ".prev"} {
		if _, err := os.Stat(old); err == nil && gitignored != nil && !gitignored(old) {
			add("SECURITY_SIGNING_KEY_NOT_IGNORED", "blocking", rel(m.Dir, old), "a previous IAM signing key is not gitignored", "add it to .gitignore (or finish the rotation)")
		}
	}
	if gitignored != nil {
		if _, err := os.Stat(envPath); err == nil && !gitignored(envPath) {
			add("SECURITY_SECRETS_IN_ENV", "blocking", rel(m.Dir, envPath), ".env holds secrets but is not gitignored", "add it to .gitignore")
		}
	}
	if c.CoreImage != "" && !strings.Contains(m.SubstImage(c.CoreImage), "@sha256:") {
		add("SECURITY_UNPINNED_CORE_IMAGE", "note", "core_image", "the core image is pinned by tag, not digest", "pin it as <ref>@sha256:<digest>")
	}
	for _, a := range m.Apps {
		for _, mod := range a.Modules {
			if mod.IsImage() && !strings.Contains(mod.Image, "@sha256:") {
				add("SECURITY_UNPINNED_IMAGE", "note", "module "+mod.Token, "module image "+mod.Image+" is pinned by tag, not digest", "pin it by digest for reproducible releases")
			}
		}
		for k, v := range a.Decl.Env {
			lk := strings.ToLower(k)
			if (strings.Contains(lk, "password") || strings.Contains(lk, "secret") || strings.HasSuffix(lk, "_key") || strings.Contains(lk, "token")) &&
				v != "" && !strings.Contains(v, "${") {
				add("SECURITY_SECRETS_IN_ENV", "risk", "app "+a.Decl.Name, fmt.Sprintf("[app.env] %s holds a literal secret in the committed descriptor", k), fmt.Sprintf("set %s = \"${%s}\" and put the value in .env", k, k))
			}
		}
	}
	return out
}

func readEnv(p string) map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(p)
	if err != nil {
		return out
	}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "export "))
		if i := strings.IndexByte(l, '='); i > 0 && !strings.HasPrefix(l, "#") {
			out[l[:i]] = true
		}
	}
	return out
}

func rel(base, p string) string {
	if r, err := filepath.Rel(base, p); err == nil {
		return r
	}
	return p
}

// GitIgnored returns a predicate backed by `git check-ignore` (nil outside git).
func GitIgnored(dir string) func(string) bool {
	if _, err := sh.Run(sh.Cmd{Name: "git", Args: []string{"-C", dir, "rev-parse", "--is-inside-work-tree"}}); err != nil {
		return nil
	}
	return func(p string) bool {
		_, err := sh.Run(sh.Cmd{Name: "git", Args: []string{"-C", dir, "check-ignore", "-q", p}})
		return err == nil
	}
}

// DevX returns developer-experience findings.
func DevX(m *descriptor.Model) []Finding {
	var out []Finding
	jdir := filepath.Join(m.Infra, "journeys")
	for _, a := range m.Apps {
		if !a.Decl.Cored() {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(jdir, a.Decl.Name+"*.toml"))
		if len(matches) == 0 {
			out = append(out, Finding{ID: "DEVX_NO_JOURNEY_COVERAGE", Concern: "devx", Severity: "note", Subject: "app " + a.Decl.Name,
				Cause: "no journey exercises this app", Fix: "axon cluster generate (scaffolds journeys/" + a.Decl.Name + "_smoke.toml)"})
		}
	}
	return out
}

// Business returns findings about verification freshness and data choices.
func Business(m *descriptor.Model) []Finding {
	var out []Finding
	p := filepath.Join(m.Dir, "verify", "last-run.json")
	st, err := os.Stat(p)
	switch {
	case err != nil:
		out = append(out, Finding{ID: "BUSINESS_NEVER_VERIFIED", Concern: "business", Severity: "note", Subject: "verify", Cause: "the cluster has never been verified", Fix: "axon env test <env>"})
	case time.Since(st.ModTime()) > 7*24*time.Hour:
		out = append(out, Finding{ID: "BUSINESS_VERIFY_STALE", Concern: "business", Severity: "note", Subject: "verify",
			Cause: fmt.Sprintf("last verified %s ago", time.Since(st.ModTime()).Round(time.Hour)), Fix: "axon env test <env>"})
	}
	if t := m.C.Fabric.Temporal; t != nil {
		out = append(out, Finding{ID: "BUSINESS_TEMPORAL_SHARDS_IMMUTABLE", Concern: "business", Severity: "note", Subject: "fabric.temporal",
			Cause: fmt.Sprintf("shards = %d is fixed once Temporal initializes its database", t.Shards), Fix: "changing it requires wiping the temporal databases"})
	}
	return out
}

// Sort orders findings by concern, severity, id.
func Sort(fs []Finding) {
	rank := map[string]int{"blocking": 0, "risk": 1, "note": 2}
	order := map[string]int{}
	for i, c := range Concerns {
		order[c] = i
	}
	sort.SliceStable(fs, func(i, j int) bool {
		if order[fs[i].Concern] != order[fs[j].Concern] {
			return order[fs[i].Concern] < order[fs[j].Concern]
		}
		if rank[fs[i].Severity] != rank[fs[j].Severity] {
			return rank[fs[i].Severity] < rank[fs[j].Severity]
		}
		return fs[i].ID < fs[j].ID
	})
}

// Count returns blocking, risk, note counts.
func Count(fs []Finding) (b, r, n int) {
	for _, f := range fs {
		switch f.Severity {
		case "blocking":
			b++
		case "risk":
			r++
		default:
			n++
		}
	}
	return
}

// Print renders the findings report on stdout.
func Print(fs []Finding) {
	Sort(fs)
	b, r, n := Count(fs)
	head := fmt.Sprintf("%d blocking · %d risk · %d note", b, r, n)
	if b > 0 {
		ui.Out("%s %s", ui.Red("✗"), head)
	} else {
		ui.Out("%s %s", ui.Green("✓"), head)
	}
	cur := ""
	for _, f := range fs {
		if f.Concern != cur {
			cur = f.Concern
			ui.Out("\n%s", strings.ToUpper(cur))
		}
		glyph := map[string]string{"blocking": ui.Red("✗"), "risk": ui.Yellow("!"), "note": ui.Dim("·")}[f.Severity]
		ui.Out("  %s %s  %s — %s", glyph, f.ID, f.Subject, f.Cause)
		if f.Fix != "" {
			ui.Out("      repair: %s", f.Fix)
		}
	}
}
