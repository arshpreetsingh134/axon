package commands

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"axon/internal/build"
	"axon/internal/ci"
	"axon/internal/cigen"
	"axon/internal/cli"
	"axon/internal/descriptor"
	"axon/internal/envcfg"
	"axon/internal/tomledit"
	"axon/internal/ui"
)

func ciGenVerbs() []*cli.Verb {
	return []*cli.Verb{
		{Name: "ci init", Group: "ci",
			Summary: "generate the GitHub Actions pipelines (CI, release, deploy) from the descriptor and environment files",
			Help: "Generate .github/workflows/axon-ci.yml from the descriptor and [ci] settings: a fast `check` job (pipeline drift, cluster check, " +
				"env check, every app's unit tests) and an `e2e` job that deploys the whole stack on the runner with `axon up --build`, " +
				"runs `axon env test`, prints diagnostics on failure and always tears down. With [release] registry set, also " +
				"axon-release.yml: on every v* tag it builds, pushes (multi-architecture with [release] platforms) and records the " +
				"release, then attaches the record to the GitHub Release. Environment files with [ci] deploy = true and a [cloud] " +
				"section (eks, gke, aks or kubeconfig) add axon-deploy.yml: one job per environment, bound to a GitHub Environment, " +
				"deployed after each release (auto = true), after the environment it promotes_from, or by hand. Member apps that live in other repositories " +
				"are detected once and recorded under [ci.checkouts] (and [ci.links] for apps that are symlinks into a checkout) in axon.toml, so the generated file is reproducible; a `ci` environment " +
				"(Docker target) is created if missing. Rerun after changing the descriptor; `axon ci check` reports drift.",
			Flags: []cli.Flag{flagDir, flagDryRun, {Name: "redetect", Kind: cli.Bool, Usage: "re-detect member app repositories, replacing [ci.checkouts] and [ci.links]"}},
			Run:   runCIInit},
		{Name: "ci check", Group: "ci", Summary: "exit 1 if the generated pipeline is out of date with the descriptor (--diff shows how)",
			Flags: []cli.Flag{flagDir, {Name: "diff", Kind: cli.Bool, Usage: "print a line diff of the drift"}},
			Run:   runCICheck},
		ciSecretsVerb(),
		ciEnvironmentsVerb(),
	}
}

// ciLayout locates the controller inside its git repository.
func ciLayout(m *descriptor.Model) (top string, lay cigen.Layout, err error) {
	out, err := runGit(m.Dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", lay, fmt.Errorf("%s is not inside a git repository; `git init` it first (CI runs from a repository)", m.Dir)
	}
	top = strings.TrimSpace(out)
	realTop, _ := filepath.EvalSymlinks(top)
	realDir, _ := filepath.EvalSymlinks(m.Dir)
	rel, err := filepath.Rel(realTop, realDir)
	if err != nil {
		return "", lay, err
	}
	return top, cigen.Layout{RepoBase: filepath.Base(realTop), CtrlRel: filepath.ToSlash(rel)}, nil
}

// repoOf finds the git top level and GitHub "owner/repo[@ref]" of a directory.
func repoOf(dir string) (top, spec string, err error) {
	out, err := runGit(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", fmt.Errorf("%s is not a git checkout", dir)
	}
	top = strings.TrimSpace(out)
	if r, err := filepath.EvalSymlinks(top); err == nil {
		top = r
	}
	url, err := runGit(top, "remote", "get-url", "origin")
	if err != nil {
		return top, "", fmt.Errorf("%s has no origin remote", top)
	}
	owner, name, _, ok := ci.ParseRemote(strings.TrimSpace(url))
	if !ok {
		return top, "", fmt.Errorf("%s's origin is not on GitHub", top)
	}
	ref := ""
	if h, err := runGit(top, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		ref = strings.TrimPrefix(strings.TrimSpace(h), "origin/")
	} else if b, err := runGit(top, "symbolic-ref", "--short", "HEAD"); err == nil {
		ref = strings.TrimSpace(b)
	}
	return top, cigen.Checkout{Repo: owner + "/" + name, Ref: ref}.Spec(), nil
}

// assembly is what CI must check out (and link) to rebuild the workspace.
type assembly struct {
	root, ctrlTop string // real paths
	checkouts     map[string]string
	links         map[string]string
	added         []string
	warns         []string
}

func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// covered reports whether a real path is already inside the controller repo or a known checkout.
func (as *assembly) covered(real string) bool {
	if within(real, as.ctrlTop) {
		return true
	}
	rel, err := filepath.Rel(as.root, real)
	if err != nil || strings.HasPrefix(rel, "..") {
		return false
	}
	rel = filepath.ToSlash(rel)
	for p := range as.checkouts {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// checkoutFor records the repository holding real and returns its workspace-relative checkout path.
func (as *assembly) checkoutFor(real, what string, outsideOK bool) (string, bool) {
	top, spec, err := repoOf(real)
	if err != nil {
		as.warns = append(as.warns, fmt.Sprintf("%s: %v — add its repository under [ci.checkouts]", what, err))
		return "", false
	}
	rel, _ := filepath.Rel(as.root, top)
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, "..") {
		if !outsideOK {
			as.warns = append(as.warns, fmt.Sprintf("%s lives in %s, outside the workspace; CI cannot reproduce that path", what, top))
			return "", false
		}
		rel = ".axon-ci/" + strings.ReplaceAll(strings.SplitN(spec, "@", 2)[0], "/", "__")
	}
	if _, ok := as.checkouts[rel]; !ok {
		as.checkouts[rel] = spec
		as.added = append(as.added, fmt.Sprintf("checkout %s → %s", rel, spec))
	}
	inRepo, _ := filepath.Rel(top, real)
	return path.Clean(path.Join(rel, filepath.ToSlash(inRepo))), true
}

// detectAssembly finds the repositories of member apps (and of modules they link to)
// that are not in the controller repo. Existing entries are kept unless redetect.
func detectAssembly(m *descriptor.Model, ctrlTop string, redetect bool) *assembly {
	as := &assembly{checkouts: map[string]string{}, links: map[string]string{}}
	as.root, _ = filepath.EvalSymlinks(m.Root)
	as.ctrlTop, _ = filepath.EvalSymlinks(ctrlTop)
	if !redetect {
		for k, v := range m.C.CI.Checkouts {
			as.checkouts[k] = v
		}
		for k, v := range m.C.CI.Links {
			as.links[k] = v
		}
	}
	for _, a := range m.Apps {
		if a.Dir == "" || a.Problem != "" || a.File == nil { // prebuilt-image apps need no checkout
			continue
		}
		name := a.Decl.Name
		real, err := filepath.EvalSymlinks(a.Dir)
		if err != nil {
			continue
		}
		if _, linked := as.links[name]; !linked && !as.covered(real) {
			at, ok := as.checkoutFor(real, "member app "+name, true)
			if !ok {
				continue
			}
			if at != name {
				as.links[name] = at
				as.added = append(as.added, fmt.Sprintf("link %s → %s", name, at))
			}
		}
		for _, mod := range a.Modules {
			if mod.SrcDir == "" {
				continue
			}
			if r, err := filepath.EvalSymlinks(mod.SrcDir); err == nil && !as.covered(r) && !within(r, real) {
				as.checkoutFor(r, name+": module "+mod.Token, false)
			}
		}
	}
	return as
}

func runCIInit(c *cli.Ctx) error {
	m, err := loadCluster(c, or(c.String("dir"), "."))
	if err != nil {
		return err
	}
	top, lay, err := ciLayout(m)
	if err != nil {
		return err
	}
	dry := c.Bool("dry-run")
	// 1. record the repositories CI assembles the workspace from (once) in the overlay
	ovPath := filepath.Join(m.Infra, descriptor.OverlayName)
	ovText, _ := os.ReadFile(ovPath)
	ov := tomledit.Parse(string(ovText))
	as := detectAssembly(m, top, c.Bool("redetect"))
	warns := as.warns
	changed := c.Bool("redetect") && (len(m.C.CI.Checkouts)+len(m.C.CI.Links) > 0 || len(as.added) > 0)
	if c.Bool("redetect") {
		ov.RemoveTable("ci.checkouts")
		ov.RemoveTable("ci.links")
	}
	if len(as.added) > 0 || c.Bool("redetect") {
		for _, k := range sortedKeysOf(as.checkouts) {
			ov.Set("ci.checkouts", k, tomledit.Quote(as.checkouts[k]))
		}
		for _, k := range sortedKeysOf(as.links) {
			ov.Set("ci.links", k, tomledit.Quote(as.links[k]))
		}
	}
	for _, a := range as.added {
		ui.Sub("%s", a)
	}
	changed = changed || len(as.added) > 0
	m.C.CI.Checkouts, m.C.CI.Links = as.checkouts, as.links // so a dry run previews the same pipeline
	if changed && !dry {
		if len(ovText) == 0 {
			ov = tomledit.Parse("# axon.toml — axon-only settings beside the descriptor.\n" + ov.String())
		}
		if err := os.WriteFile(ovPath, []byte(ov.String()), 0o644); err != nil {
			return err
		}
		if m, err = descriptor.Load(m.Found); err != nil {
			return err
		}
	}
	// 2. the CI environment
	envName := or(m.C.CI.Env, "ci")
	if p := envcfg.PathFor(m.Dir, envName); !fileExists(p) {
		e := &envcfg.Env{Name: envName, Tier: "dev", Namespace: m.C.Name + "-" + envName, Release: "local", Target: "docker"}
		if dry {
			ui.Sub("would create %s", rel2(m.Dir, p))
		} else {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(p, []byte(envcfg.Render(e)), 0o644); err != nil {
				return err
			}
			ui.Sub("created %s (Docker target, used by the pipeline)", rel2(m.Dir, p))
		}
	}
	// 3. the workflow
	res, err := cigen.Generate(m, lay)
	if err != nil {
		return err
	}
	var paths []string
	for p := range res.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		full := filepath.Join(top, p)
		if dry {
			ui.Out("would write %s", full)
			continue
		}
		wrote, err := build.WriteIfChanged(full, res.Files[p], 0o644)
		if err != nil {
			return err
		}
		if wrote {
			ui.OK("wrote %s", full)
		} else {
			ui.OK("%s is up to date", full)
		}
	}
	for _, w := range append(warns, res.Warnings...) {
		ui.Warn("%s", w)
	}
	if len(res.Secrets)+len(res.Optional) > 0 {
		ui.Out("")
		ui.Out("GitHub secrets the pipeline reads (repository → Settings → Secrets and variables → Actions):")
		for _, s := range res.Secrets {
			ui.Out("  %s", s)
		}
		for _, s := range res.Optional {
			why := map[string]string{
				"AXON_REPOS_TOKEN": "a token that can read the member app repositories (only if they are private)",
				"AXON_GHCR_TOKEN":  "a token with read:packages for private ghcr.io images",
			}[s]
			ui.Out("  %s  (optional: %s)", s, why)
		}
	}
	if len(res.Envs) > 0 {
		ui.Out("")
		ui.Out("Deploy pipeline (%s) — GitHub Environments:", cigen.DeployWorkflowPath)
		for _, e := range res.Envs {
			extra := ""
			if len(e.Reviewers) > 0 {
				extra = " · reviewers " + strings.Join(e.Reviewers, ", ")
			}
			if len(res.EnvSecrets[e.Name]) > 0 {
				extra += " · secrets " + strings.Join(res.EnvSecrets[e.Name], ", ")
			}
			ui.Out("  %s%s", e.Name, extra)
		}
		ui.Out("  `axon ci environments --apply` creates them; `axon ci secrets --set` fills their secrets")
	}
	if !dry {
		ui.Out("")
		ui.Out("next: commit .github/workflows%s, push, then `axon ci watch`", map[bool]string{true: " and " + rel2(top, ovPath), false: ""}[changed])
	}
	return nil
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func runCICheck(c *cli.Ctx) error {
	m, err := loadCluster(c, or(c.String("dir"), "."))
	if err != nil {
		return err
	}
	top, lay, err := ciLayout(m)
	if err != nil {
		return err
	}
	res, err := cigen.Generate(m, lay)
	if err != nil {
		return err
	}
	drift := 0
	for p, want := range res.Files {
		full := filepath.Join(top, p)
		have, err := os.ReadFile(full)
		switch {
		case err != nil:
			drift++
			ui.Out("%s %s is missing — run `axon ci init`", ui.Red("✗"), p)
		case string(have) != want:
			drift++
			ui.Out("%s %s is out of date with the descriptor — run `axon ci init`", ui.Yellow("~"), p)
			if c.Bool("diff") {
				ui.Raw(lineDiffColored(string(have), want))
			}
		}
	}
	if drift > 0 {
		return cli.Failed()
	}
	ui.Out("up to date")
	return nil
}

func lineDiffColored(a, b string) string {
	in := map[string]int{}
	for _, l := range strings.Split(a, "\n") {
		in[l]++
	}
	inB := map[string]int{}
	for _, l := range strings.Split(b, "\n") {
		inB[l]++
	}
	var out strings.Builder
	for _, l := range strings.Split(a, "\n") {
		if inB[l] == 0 {
			out.WriteString(ui.Red("- "+l) + "\n")
		}
	}
	for _, l := range strings.Split(b, "\n") {
		if in[l] == 0 {
			out.WriteString(ui.Green("+ "+l) + "\n")
		}
	}
	return out.String()
}

func sortedKeysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
