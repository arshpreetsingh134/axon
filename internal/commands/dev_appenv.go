package commands

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"axon/internal/descriptor"
	"axon/internal/dotenv"
	"axon/internal/official"
	"axon/internal/secrets"
	"axon/internal/sh"
	"axon/internal/ui"
)

// appEnvRefs lists the keys of Secret mycel-app-env that the rendered objects read without `optional`: a pod whose
// key is missing never starts (CreateContainerConfigError).
func appEnvRefs(objs []official.Object) []string {
	seen := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if ref, ok := x["secretKeyRef"].(map[string]any); ok {
				if ref["name"] == "mycel-app-env" && ref["optional"] != true {
					if k, _ := ref["key"].(string); k != "" {
						seen[k] = true
					}
				}
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		case []map[string]any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	for _, o := range objs {
		walk(map[string]any(o))
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ensureAppEnvRefs makes every key the rendered workloads read from mycel-app-env exist before they roll out (SPEC.md
// Appendix C.11): a key missing from the Secret is copied from the developer's values (the tool's secret store, then
// <infra>/.env, then the process environment; an empty value counts as set), and a key whose value there differs from
// the Secret's is updated, so an edit to .env reaches the next deploy. Keys only the Secret holds (such as generated
// client keys) stay as they are. Anything still missing stops the deploy before it changes a workload. Values are never
// printed.
func ensureAppEnvRefs(objs []official.Object, m *descriptor.Model, envName, ctx, ns string, dry bool) error {
	refs := appEnvRefs(objs)
	if len(refs) == 0 {
		return nil
	}
	have := map[string]string{} // key → base64 value in the Secret
	exists := false
	if out, err := sh.Output("kubectl", "--context", ctx, "-n", ns, "get", "secret", "mycel-app-env", "-o", "json"); err == nil {
		exists = true
		var s struct {
			Data map[string]string `json:"data"`
		}
		_ = json.Unmarshal([]byte(out), &s)
		for k, v := range s.Data {
			have[k] = v
		}
	}
	sources := map[string]string{}
	vals := map[string]string{}
	if st := secrets.New(ctx, descriptor.Slug(m.C.Name), envName); st.Available() {
		if v, err := st.Values(); err == nil {
			for k, x := range v {
				vals[k], sources[k] = x, "secret store"
			}
		}
	}
	envFile := filepath.Join(m.Infra, ".env")
	if v, err := dotenv.Read(envFile); err == nil {
		for k, x := range v {
			if _, ok := vals[k]; !ok {
				vals[k], sources[k] = x, filepath.Join(m.L.Infra, ".env")
			}
		}
	}
	data := map[string]string{}
	var missing []string
	for _, k := range refs {
		cur, inSecret := have[k]
		v, ok := vals[k]
		if !ok && !inSecret {
			if x, set := os.LookupEnv(k); set {
				v, ok, sources[k] = x, true, "environment"
			}
		}
		if !ok {
			if !inSecret {
				missing = append(missing, k)
			}
			continue // a value only the Secret holds (for example a generated client key) stays as it is
		}
		enc := base64.StdEncoding.EncodeToString([]byte(v))
		if inSecret && enc == cur {
			continue
		}
		data[k] = enc
		if dry {
			verb := "add"
			if inSecret {
				verb = "update"
			}
			ui.Sub("would %s %s in Secret mycel-app-env from %s", verb, k, sources[k])
		}
	}
	if len(missing) > 0 {
		msg := fmt.Sprintf("Secret mycel-app-env lacks %s, which the workloads read; set each in %s (an empty value is allowed) or the secret store, then deploy again",
			strings.Join(missing, ", "), filepath.Join(m.L.Infra, ".env"))
		if dry {
			ui.Warn("%s", msg)
		} else {
			return fmt.Errorf("%s", msg)
		}
	}
	if dry || len(data) == 0 {
		return nil
	}
	if !exists {
		obj, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
			"metadata": map[string]any{"name": "mycel-app-env", "namespace": ns}, "data": data})
		if res, err := sh.Run(sh.Cmd{Name: "kubectl", Args: []string{"--context", ctx, "-n", ns, "create", "-f", "-"}, Stdin: string(obj)}); err != nil {
			return fmt.Errorf("create Secret mycel-app-env: %w\n%s", err, strings.TrimSpace(res.Stderr))
		}
	} else {
		patch, _ := json.Marshal(map[string]any{"data": data})
		if res, err := sh.Run(sh.Cmd{Name: "kubectl", Args: []string{"--context", ctx, "-n", ns, "patch", "secret", "mycel-app-env",
			"--type", "merge", "--patch-file", "/dev/stdin"}, Stdin: string(patch)}); err != nil {
			msg := ""
			if res != nil {
				msg = strings.TrimSpace(res.Stderr)
			}
			return fmt.Errorf("write Secret mycel-app-env: %w\n%s", err, msg)
		}
	}
	names := make([]string, 0, len(data))
	for k := range data {
		names = append(names, k)
	}
	sort.Strings(names)
	ui.Sub("Secret mycel-app-env: set %s from the developer's values", strings.Join(names, ", "))
	return nil
}
