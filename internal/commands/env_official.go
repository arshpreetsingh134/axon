package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"axon/internal/check"
	"axon/internal/cli"
	"axon/internal/descriptor"
	"axon/internal/envcfg"
	"axon/internal/official"
	"axon/internal/sh"
	"axon/internal/ui"
)

// officialEnv returns the official layout when env is an official (non-dev)
// environment of a pipeline-released cluster, else nil.
func officialEnv(m *descriptor.Model, e *envcfg.Env) *official.Layout {
	if e == nil || e.Tier == "" || e.Tier == "dev" {
		return nil
	}
	l, ok, err := official.Detect(m.Dir)
	if err != nil || !ok {
		return nil
	}
	return l
}

// clusterID names the cluster in axon's local state.
func clusterID(m *descriptor.Model) string { return descriptor.Slug(m.C.Name) }

// appEnvSecret is the name of the pre-provisioned app env secret.
func appEnvSecret(m *descriptor.Model) string { return m.L.K8s("app-env") }

// officialNoise are descriptor findings that do not apply to an official
// environment: its members are image-backed (no app checkout next to the
// cluster), and the descriptor carries keys of the reference tool.
var officialNoise = map[string]bool{"CONFORMANCE_APP_NOT_PRESENT": true, "CONFORMANCE_UNKNOWN_KEY": true}

// officialEnvFindings replaces the local ${VAR} check for an official
// environment: values live in the cluster's pre-provisioned secret, so the
// check is against that secret's key inventory (plan T0.3, SPEC.md C.5).
func officialEnvFindings(m *descriptor.Model, l *official.Layout, e *envcfg.Env, name string, base []check.Finding) []check.Finding {
	return officialEnvFindingsFor(m, l, e, name, base, m.Found.Path, rel2(m.Dir, m.Found.Path))
}

// officialEnvFindingsFor checks the descriptor at descPath (shown as descName),
// e.g. a copy of the descriptor as it is on the default branch.
func officialEnvFindingsFor(m *descriptor.Model, l *official.Layout, e *envcfg.Env, name string, base []check.Finding, descPath, descName string) []check.Finding {
	var fs []check.Finding
	for _, f := range base {
		if !officialNoise[f.ID] {
			fs = append(fs, f)
		}
	}
	add := func(id, sev, subj, cause, fix string) {
		fs = append(fs, check.Finding{ID: id, Concern: "conformance", Severity: sev, Subject: subj, Cause: cause, Fix: fix})
	}
	envFile := "environments/" + name + ".toml"
	if e.Namespace == "" {
		add("OFFICIAL_NAMESPACE_MISSING", "blocking", envFile, "an official environment deploys into a pre-provisioned namespace, and none is set", "axon env set "+name+" namespace=<namespace>")
	}
	listed := false
	for _, x := range l.Workflow.Environments {
		listed = listed || x == name
	}
	if !listed {
		add("OFFICIAL_ENV_NOT_IN_PIPELINE", "risk", envFile, fmt.Sprintf("%s does not list %q, so the deploy pipeline cannot target it", official.WorkflowFile, name), "add it to environments in "+official.WorkflowFile)
	}

	refs, inComments, err := official.RequiredKeys(descPath)
	if err != nil {
		add("OFFICIAL_DESCRIPTOR_UNREADABLE", "blocking", descName, err.Error(), "")
		return fs
	}
	secret := appEnvSecret(m)
	desc := descName
	// ${NAME} in a comment is not required: a real deploy passed with one
	// present (SPEC.md C.5), so comments are ignored.
	_ = inComments
	inv, ok, err := official.LoadInventory(clusterID(m), name)
	switch {
	case err != nil:
		add("KEY_INVENTORY_UNREADABLE", "blocking", "key inventory", err.Error(), "axon env keys "+name+" --live --context <your-context>")
		return fs
	case !ok:
		add("KEY_INVENTORY_MISSING", "risk", "secret "+secret,
			fmt.Sprintf("no key inventory for %s, so the %d required key(s) cannot be checked against the secret", name, len(official.KeyNames(refs))),
			"axon env keys "+name+" --live --context <your-context>   (or --add NAME,… if you know them)")
		return fs
	}
	where := map[string][]string{}
	for _, r := range refs {
		loc := desc
		if r.Line > 0 {
			loc = fmt.Sprintf("%s:%d", desc, r.Line)
		}
		if r.App != "" {
			loc += " (" + r.App + ")"
		}
		where[r.Name] = append(where[r.Name], loc)
	}
	from := or(inv.Source, "manual")
	if !inv.Updated.IsZero() {
		from += ", " + inv.Updated.Local().Format("2006-01-02 15:04")
	}
	for _, k := range official.KeyNames(refs) {
		if inv.Has(k) {
			continue
		}
		add("KEY_NOT_IN_SECRET", "blocking", "${"+k+"}",
			fmt.Sprintf("required by %s, but secret %s in namespace %s does not hold it (inventory: %s); the deploy would stop at the required-secret gate",
				strings.Join(where[k], ", "), secret, e.Namespace, from),
			fmt.Sprintf("provision %s in secret %s, then `axon env keys %s --live`; or remove the reference", k, secret, name))
	}
	if !inv.Updated.IsZero() && time.Since(inv.Updated) > 7*24*time.Hour {
		add("KEY_INVENTORY_STALE", "note", "key inventory", fmt.Sprintf("last refreshed %s", inv.Updated.Local().Format("2006-01-02")), "axon env keys "+name+" --live --context <your-context>")
	}
	return fs
}

// liveKeyNames reads the key NAMES of the app env secret with the developer's
// own kube context (plan D1). Values are never printed or stored.
func liveKeyNames(context, namespace, secret string) ([]string, error) {
	if context == "" {
		return nil, fmt.Errorf("pass --context <your kube context for the cluster> (set it once with `axon env set <env> context=<name>`)")
	}
	if namespace == "" {
		return nil, fmt.Errorf("the environment has no namespace")
	}
	r, err := sh.Run(sh.Cmd{Name: "kubectl", Args: []string{"--context", context, "-n", namespace, "get", "secret", secret,
		"-o", `go-template={{range $k, $v := .data}}{{$k}}{{"\n"}}{{end}}`}, Timeout: 60 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("read key names of secret %s in %s (context %s): %w", secret, namespace, context, err)
	}
	var keys []string
	for _, k := range strings.Fields(r.Stdout) {
		if official.ValidKeyName(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// refreshInventory replaces the inventory with the live key names.
func refreshInventory(m *descriptor.Model, e *envcfg.Env, name, context string) (*official.Inventory, error) {
	ctx := or(context, e.Context)
	keys, err := liveKeyNames(ctx, e.Namespace, appEnvSecret(m))
	if err != nil {
		return nil, err
	}
	inv := &official.Inventory{Cluster: clusterID(m), Env: name, Keys: keys, Path: official.InventoryPath(clusterID(m), name),
		Source: fmt.Sprintf("live (context %s, secret %s)", ctx, appEnvSecret(m))}
	if err := inv.Save(); err != nil {
		return nil, err
	}
	return inv, nil
}

// runEnvKeys shows or updates an official environment's key inventory.
func runEnvKeys(c *cli.Ctx) error {
	m, err := controller(c)
	if err != nil {
		return err
	}
	name := c.Arg(0)
	e, err := envcfg.Load(m.Dir, m.C.Name, name)
	if err != nil {
		return err
	}
	if officialEnv(m, e) == nil {
		return cli.Usage("env keys: %s is not an official environment of a pipeline-released cluster (needs %s and a non-dev tier)", name, official.WorkflowFile)
	}
	if err := inventoryOutsideRepos(m, name); err != nil {
		return err
	}
	inv, ok, err := official.LoadInventory(clusterID(m), name)
	if err != nil {
		return err
	}
	changed := false
	if c.Bool("live") {
		if inv, err = refreshInventory(m, e, name, c.String("context")); err != nil {
			return err
		}
		ok, changed = true, false
		ui.OK("read %d key name(s) from secret %s (values were not read into the inventory)", len(inv.Keys), appEnvSecret(m))
	}
	for _, list := range c.Strings("add") {
		for _, k := range strings.Split(list, ",") {
			if k = strings.TrimSpace(k); k == "" {
				continue
			}
			if !official.ValidKeyName(k) {
				return cli.Usage("env keys: %q is not a key name", k)
			}
			inv.Keys = append(inv.Keys, k)
			changed = true
		}
	}
	for _, list := range c.Strings("remove") {
		for _, k := range strings.Split(list, ",") {
			k = strings.TrimSpace(k)
			var kept []string
			for _, x := range inv.Keys {
				if x != k {
					kept = append(kept, x)
				}
			}
			changed = changed || len(kept) != len(inv.Keys)
			inv.Keys = kept
		}
	}
	if changed {
		inv.Source, inv.Updated = "manual", time.Time{}
		if err := inv.Save(); err != nil {
			return err
		}
		ok = true
	}

	refs, _, err := official.RequiredKeys(m.Found.Path)
	if err != nil {
		return err
	}
	required := official.KeyNames(refs)
	if c.Bool("json") {
		return ui.JSON(map[string]any{"path": inv.Path, "exists": ok, "source": inv.Source, "updated": inv.Updated, "keys": inv.Keys, "required": required})
	}
	if !ok {
		ui.Note("no inventory yet at %s", inv.Path)
		ui.Note("record it from the cluster: axon env keys %s --live --context <your-context>", name)
	} else {
		ui.Out("%s  (%s%s)", inv.Path, or(inv.Source, "manual"), stamp(inv.Updated))
	}
	req := map[string]bool{}
	for _, k := range required {
		req[k] = true
	}
	rows := [][]string{{"", "KEY", "REQUIRED BY THE DESCRIPTOR", "IN THE SECRET"}}
	all := append(append([]string{}, required...), inv.Keys...)
	sort.Strings(all)
	seen := map[string]bool{}
	for _, k := range all {
		if seen[k] {
			continue
		}
		seen[k] = true
		mark, r, s := ui.Green("✓"), "", ""
		if req[k] {
			r = "yes"
		}
		if inv.Has(k) {
			s = "yes"
		}
		switch {
		case req[k] && !inv.Has(k) && ok:
			mark, s = ui.Red("✗"), "missing"
		case req[k] && !ok:
			mark, s = ui.Dim("?"), "unknown"
		case !req[k]:
			mark = ui.Dim("·")
		}
		rows = append(rows, []string{mark, k, r, s})
	}
	ui.Table(ui.Stdout, rows, true)
	return nil
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return ", " + t.Local().Format("2006-01-02 15:04")
}

// inventoryOutsideRepos guards plan decision D2: the inventory path must not be
// inside the cluster repository.
func inventoryOutsideRepos(m *descriptor.Model, name string) error {
	p, _ := filepath.Abs(official.InventoryPath(clusterID(m), name))
	root, _ := filepath.Abs(m.Dir)
	if strings.HasPrefix(p, root+string(os.PathSeparator)) {
		return fmt.Errorf("the key inventory path %s is inside the cluster repository; set XDG_CONFIG_HOME outside it", p)
	}
	return nil
}
