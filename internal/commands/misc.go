package commands

import (
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"axon/internal/check"
	"axon/internal/cli"
	"axon/internal/deploy"
	"axon/internal/descriptor"
	"axon/internal/gen"
	"axon/internal/layout"
	"axon/internal/tomledit"
	"axon/internal/ui"
)

func runClusterExplain(c *cli.Ctx) error {
	var m *descriptor.Model
	if f, err := layout.FindClusterUpward(dirArg(c, 0), compat(c)); err == nil {
		m, _ = descriptor.Load(f)
	}
	var b strings.Builder
	if m == nil {
		b.WriteString(genericExplain)
		ui.Raw(b.String())
		return nil
	}
	cl := m.C
	fmt.Fprintf(&b, "# %s — architecture\n\n", cl.Name)
	fmt.Fprintf(&b, "Descriptor `%s/%s` (%s layout). Front door: Traefik on host port **%d**; every app answers at `http://<host>.%s:%d`.\n\n",
		m.L.Infra, m.L.File, m.L.Brand, cl.HTTPPort(), cl.DomainOrDefault(), cl.HTTPPort())
	b.WriteString("## Fabric\n\n| Service | Image | Host port |\n|---|---|---|\n")
	f := cl.Fabric
	row := func(n, img string, port int) {
		p := "—"
		if port > 0 {
			p = fmt.Sprint(port)
		}
		fmt.Fprintf(&b, "| %s | `%s` | %s |\n", n, img, p)
	}
	if f.Postgres != nil {
		row("postgres", or(f.Postgres.Image, gen.ImgPostgres), f.Postgres.Port)
	}
	if f.Temporal != nil {
		row("temporal", or(f.Temporal.Image, gen.ImgTemporal), f.Temporal.Port)
		row("temporal-ui", or(f.Temporal.UIImage, gen.ImgTemporalUI), f.Temporal.UIPort)
	}
	if f.Keycloak != nil {
		row("keycloak", or(f.Keycloak.Image, gen.ImgKeycloak), 0)
	}
	row("traefik", gen.ImgTraefik, cl.HTTPPort())
	if f.Tracing != nil {
		row("tempo", or(f.Tracing.TempoImage, gen.ImgTempo), 0)
		row("grafana", or(f.Tracing.GrafanaImage, gen.ImgGrafana), f.Tracing.GrafanaPort)
	}
	if f.Clickhouse != nil {
		row("clickhouse", or(f.Clickhouse.Image, gen.ImgClickhouse), f.Clickhouse.HTTPPort)
	}
	b.WriteString("\n## Apps\n\n| App | Kind | Front door | Database | Modules |\n|---|---|---|---|---|\n")
	for _, a := range m.Apps {
		door := "—"
		if h := gen.RouteHost(cl, a.Decl); h != "" {
			door = cl.FrontDoor(h)
		}
		var mods []string
		for _, mod := range a.Modules {
			src := "source"
			if mod.IsImage() {
				src = "image"
			}
			mods = append(mods, fmt.Sprintf("`%s` (%s)", mod.Token, src))
		}
		if a.Problem != "" {
			mods = []string{"⚠ " + a.Problem}
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", a.Decl.Name, a.Decl.KindOrDefault(), door, or(a.Decl.DB, "—"), strings.Join(mods, ", "))
	}
	if len(m.Skipped) > 0 {
		fmt.Fprintf(&b, "\nLeft out of deploys: %s.\n", strings.Join(m.Skipped, ", "))
	}
	b.WriteString("\n## How a deploy runs (`axon up --build`)\n\n")
	b.WriteString("1. Take the cluster run lock; run `pre_generate` / `pre_build` hooks.\n")
	b.WriteString("2. Build what changed: module UIs (npm), Go module servers (cross-compiled on the host), core images FROM `core_image`, worker images. Unchanged inputs are skipped; images are tagged by content hash.\n")
	b.WriteString("3. Generate the fabric and per-app stacks; bring the fabric up and wait until healthy.\n")
	b.WriteString("4. Ensure every logical database, provision Keycloak, run `post_fabric` hooks.\n")
	b.WriteString("5. Roll out every app (in parallel), wait for health, seed IAM, run `post_deploy` hooks, probe every front door.\n")
	if hooks := cl.Hooks; len(hooks.PreBuild)+len(hooks.PostDeploy)+len(hooks.PostFabric)+len(hooks.PreGenerate) > 0 {
		b.WriteString("\n## Hooks\n\n")
		for _, st := range []string{"pre_generate", "pre_build", "post_build", "post_fabric", "post_deploy", "pre_down", "post_down"} {
			for _, h := range hooks.Stage(st) {
				fmt.Fprintf(&b, "- `%s`: `%s`\n", st, h)
			}
		}
	}
	b.WriteString("\n## Releases\n\n`axon release create` records the images of the last build and every app's source commit into `releases/<version>.toml`, sealed by a SHA-256 digest; `axon env deploy <env> <version>` deploys exactly those images.\n")
	ui.Raw(b.String())
	return nil
}

const genericExplain = `# axon — how a cluster is put together

A **cluster** is a controller directory (` + "`<name>.axon/`" + `) whose descriptor
(` + "`.axon.infra/axonfile`" + `, ` + "`build_type = \"cluster\"`" + `) declares shared **fabric**
(Postgres, Temporal, Keycloak, Traefik, Tempo/Grafana, ClickHouse) and member **apps**.
Every app is a sibling directory with its own descriptor:

- ` + "`build_type = \"app\"`" + ` — a core host plus modules (` + "`[[compose]]`" + `). Each module is a
  server, a UI bundle and a Postgres schema named by its token.
- ` + "`build_type = \"project\"`" + ` — a headless worker built from its own Dockerfile.

` + "`axon up --build`" + ` builds what changed, generates Docker Compose stacks or Kubernetes
manifests, brings the fabric up, provisions databases and Keycloak, rolls out every app
and checks every front door at ` + "`http://<host>.<domain>:<port>`" + `.

Releases (` + "`axon release create`" + `) pin image digests and source commits; environments
(` + "`environments/<env>.toml`" + `) choose the target, namespace and release.

axon also understands mycel repos as they are (` + "`mycelfile`, `.mycel.infra/`" + `): see
` + "`axon migrate`" + ` to switch one to axon names.
`

func migrateVerb() *cli.Verb {
	return &cli.Verb{Name: "migrate", Group: "general", Args: []cli.Arg{{Name: "dir", Optional: true}},
		Summary: "convert a mycel cluster to axon names; shows the plan unless --apply",
		Help: "Plan (default) or apply the move from mycel names to axon names in the cluster controller: " +
			".mycel.infra/ → .axon.infra/, mycelfile → axonfile, the state dir .mycel → .axon, and .gitignore entries. " +
			"Runtime names (the database role, Kubernetes objects, ClickHouse users) stay \"mycel\" via " +
			"[deploy] runtime_prefix in .axon.infra/axon.toml, so existing data and objects keep working. " +
			"Member app repos are left alone (other clusters may still build them with mycel; axon reads mycelfiles either way) " +
			"unless --apps is given. After --apply, mycel can no longer read the controller.",
		Flags: []cli.Flag{flagDir, {Name: "apply", Kind: cli.Bool, Usage: "perform the moves"},
			{Name: "apps", Kind: cli.Bool, Usage: "also rename member app and module descriptors to axonfile"}},
		Run: runMigrate}
}

type move struct{ from, to string }

func runMigrate(c *cli.Ctx) error {
	m, err := loadCluster(c, dirArg(c, 0))
	if err != nil {
		return err
	}
	if !m.L.Compat() {
		ui.Out("%s already uses axon names", m.Dir)
		return nil
	}
	var moves []move
	seen := map[string]bool{}
	add := func(from, to string) {
		if _, err := os.Stat(from); err == nil && !seen[from] {
			seen[from] = true
			moves = append(moves, move{from, to})
		}
	}
	if c.Bool("apps") {
		for _, a := range m.Apps {
			if a.Problem != "" || a.Dir == "" {
				continue
			}
			real, err := filepath.EvalSymlinks(a.Dir)
			if err != nil {
				continue
			}
			add(filepath.Join(real, "mycelfile"), filepath.Join(real, "axonfile"))
			// an app descriptor under .mycel.infra/ moves to the app root (paths in it
			// are relative to the app dir, so they stay valid)
			add(filepath.Join(real, ".mycel.infra", "mycelfile"), filepath.Join(real, "axonfile"))
			for _, mod := range a.Modules {
				if mod.SrcDir != "" {
					add(filepath.Join(mod.SrcDir, "mycelfile"), filepath.Join(mod.SrcDir, "axonfile"))
				}
			}
		}
	}
	infraFrom, infraTo := filepath.Join(m.Dir, ".mycel.infra"), filepath.Join(m.Dir, ".axon.infra")
	descInInfra := filepath.Dir(m.Found.Path) == infraFrom
	ui.Out("migration plan for %s:", m.Dir)
	ui.Out("  rename %s → axonfile", m.Found.Path)
	if _, err := os.Stat(infraFrom); err == nil {
		ui.Out("  rename %s → %s", infraFrom, infraTo)
		ui.Out("  rename %s → .axon (state)", filepath.Join(infraTo, ".mycel"))
	}
	ui.Out("  pin runtime names: [deploy] runtime_prefix = \"mycel\" in .axon.infra/axon.toml (database role, Kubernetes objects, ClickHouse users)")
	for _, mv := range moves {
		to := filepath.Base(mv.to)
		if filepath.Dir(mv.to) != filepath.Dir(mv.from) {
			to = mv.to
		}
		ui.Out("  rename %s → %s", mv.from, to)
	}
	if !c.Bool("apps") {
		ui.Out("  member app repos: unchanged (pass --apps to rename their descriptors too)")
	}
	ui.Out("  add axon entries to .gitignore")
	if !c.Bool("apply") {
		ui.Out("\nnothing changed — rerun with --apply. mycel will no longer read this controller afterwards.")
		return nil
	}
	if !ui.Confirm("Apply the migration (mycel stops reading this controller)?", c.G.Yes) {
		return fmt.Errorf("not migrating without confirmation (pass --yes)")
	}
	// 1. pin runtime names in the overlay (it moves with the infra dir)
	ovDir := infraFrom
	if !descInInfra {
		if _, err := os.Stat(infraFrom); err != nil {
			ovDir = infraFrom
			os.MkdirAll(ovDir, 0o755)
		}
	}
	ovPath := filepath.Join(ovDir, descriptor.OverlayName)
	b, _ := os.ReadFile(ovPath)
	ov := tomledit.Parse(string(b))
	if v, _ := ov.Get("deploy", "runtime_prefix"); v == "" {
		ov.Set("deploy", "runtime_prefix", tomledit.Quote("mycel"))
		if err := os.WriteFile(ovPath, []byte(ov.String()), 0o644); err != nil {
			return err
		}
	}
	// 2. the controller descriptor, then the infra and state dirs
	if err := os.Rename(m.Found.Path, filepath.Join(filepath.Dir(m.Found.Path), "axonfile")); err != nil {
		return err
	}
	if _, err := os.Stat(infraFrom); err == nil {
		if err := os.Rename(infraFrom, infraTo); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(infraTo, ".mycel")); err == nil {
			os.Rename(filepath.Join(infraTo, ".mycel"), filepath.Join(infraTo, ".axon"))
		}
	}
	if !descInInfra {
		// root-level mycelfile: the overlay dir we created becomes .axon.infra
	}
	// 3. member descriptors (only with --apps)
	for _, mv := range moves {
		if _, err := os.Stat(mv.to); err == nil {
			return fmt.Errorf("%s already exists; not overwriting", mv.to)
		}
		if err := os.Rename(mv.from, mv.to); err != nil {
			return err
		}
	}
	gi := filepath.Join(m.Dir, ".gitignore")
	g, _ := os.ReadFile(gi)
	addLines := "\n# axon\n.axon.infra/.env\n.axon.infra/.axon/\n.axon.infra/secrets/*\n!.axon.infra/secrets/.gitkeep\n.axon.infra/fabric/\n.axon.infra/k8s/\n"
	if !strings.Contains(string(g), ".axon.infra/.axon/") {
		os.WriteFile(gi, append(g, []byte(addLines)...), 0o644)
	}
	ui.OK("migrated %s to axon names (%d member descriptor(s) renamed)", m.Dir, len(moves))
	return nil
}

func keyInfo(m *descriptor.Model) (path string, err error) {
	path = filepath.Join(m.Infra, "secrets", "iam_signing_key.pem")
	if iam := m.C.Fabric.IAM; iam != nil && iam.SigningKey != "" {
		path = filepath.Join(m.Dir, iam.SigningKey)
	}
	return path, nil
}

func runIAMKeys(c *cli.Ctx) error {
	m, err := controller(c)
	if err != nil {
		return err
	}
	p, _ := keyInfo(m)
	st, err := os.Stat(p)
	if err != nil {
		ui.Out("no signing key at %s (axon generates one on the first deploy)", rel2(m.Dir, p))
		if c.Bool("check") {
			return cli.Failed()
		}
		return nil
	}
	b, _ := os.ReadFile(p)
	fp := "unparseable"
	if blk, _ := pem.Decode(b); blk != nil {
		if k, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
			if pub, err := x509.MarshalPKIXPublicKey(publicOf(k)); err == nil {
				sum := sha256.Sum256(pub)
				fp = "SHA256:" + hex.EncodeToString(sum[:])[:32]
			}
		}
	}
	ignored := "unknown (not a git repo)"
	if gi := check.GitIgnored(m.Dir); gi != nil {
		ignored = fmt.Sprint(gi(p))
	}
	rows := [][]string{{"key", rel2(m.Dir, p)}, {"mode", fmt.Sprintf("%o", st.Mode().Perm())}, {"fingerprint", fp}, {"gitignored", ignored}, {"modified", st.ModTime().UTC().Format("2006-01-02T15:04:05Z")}}
	if _, err := os.Stat(prevKey(p)); err == nil {
		rows = append(rows, []string{"previous", rel2(m.Dir, prevKey(p)) + " (rotation in progress)"})
	}
	ui.Table(ui.Stdout, rows, false)
	if c.Bool("check") && (st.Mode().Perm()&0o077 != 0 || ignored == "false") {
		return cli.Failed()
	}
	return nil
}

// prevKey names the previous key so that "*.pem" ignore rules cover it.
func prevKey(p string) string { return strings.TrimSuffix(p, ".pem") + ".prev.pem" }

func runRotateIAMKey(c *cli.Ctx) error {
	m, err := controller(c)
	if err != nil {
		return err
	}
	p, _ := keyInfo(m)
	prev := prevKey(p)
	switch {
	case c.Bool("reset"):
		if _, err := os.Stat(prev); err != nil {
			return fmt.Errorf("no previous key to restore")
		}
		if err := os.Rename(prev, p); err != nil {
			return err
		}
		ui.OK("restored the previous key; redeploy with `axon env deploy %s`", c.Arg(0))
	case c.Bool("finish"):
		if err := os.Remove(prev); err != nil {
			return fmt.Errorf("no rotation in progress")
		}
		ui.OK("rotation finished; the previous key is deleted")
	default:
		if _, err := os.Stat(prev); err == nil && !c.Bool("force") {
			return fmt.Errorf("a rotation is already in progress (%s); --finish, --reset or --force", rel2(m.Dir, prev))
		}
		if _, err := os.Stat(p); err == nil {
			if err := os.Rename(p, prev); err != nil {
				return err
			}
		}
		k, err := deploy.NewSigningKeyPEM()
		if err != nil {
			return err
		}
		if err := deploy.WriteSigningKey(p, k); err != nil {
			return err
		}
		ui.OK("new IAM signing key at %s (previous kept as %s)", rel2(m.Dir, p), filepath.Base(prev))
		ui.Out("next: axon env deploy %s   (existing sessions end when the IAM module restarts)", c.Arg(0))
		ui.Out("then: axon env rotate-iam-key %s --finish", c.Arg(0))
	}
	return nil
}

func publicOf(k any) any {
	if signer, ok := k.(crypto.Signer); ok {
		return signer.Public()
	}
	return nil
}
