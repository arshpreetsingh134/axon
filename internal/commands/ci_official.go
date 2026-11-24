package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"axon/internal/check"
	"axon/internal/ci"
	"axon/internal/cli"
	"axon/internal/descriptor"
	"axon/internal/envcfg"
	"axon/internal/official"
	"axon/internal/release"
	"axon/internal/sh"
	"axon/internal/tomledit"
	"axon/internal/ui"
)

// officialPipeline loads what the pipeline helpers need: the model, the
// official layout and a GitHub client for the cluster repository.
func officialPipeline(c *cli.Ctx, verb string) (*descriptor.Model, *official.Layout, *ci.GitHub, error) {
	m, err := controller(c)
	if err != nil {
		return nil, nil, nil, err
	}
	l, err := officialLayout(m.Dir)
	if err != nil {
		return nil, nil, nil, err
	}
	if l == nil {
		return nil, nil, nil, cli.Usage("%s: no %s here; it applies to pipeline-released clusters", verb, official.WorkflowFile)
	}
	owner, name, ok := l.RepoSlug()
	if !ok {
		return nil, nil, nil, fmt.Errorf("%s: cluster_repository is not set", official.WorkflowFile)
	}
	gh := ci.NewGitHub(owner, name)
	if gh.Token == "" {
		return nil, nil, nil, fmt.Errorf("%s needs a GitHub token: sign in with `gh auth login` or set GITHUB_TOKEN", verb)
	}
	return m, l, gh, nil
}

// activeRuns returns the runs of workflow that are queued, waiting or running.
func activeRuns(gh *ci.GitHub, workflow string) ([]ci.RunDetail, error) {
	runs, err := gh.WorkflowRuns(workflow, 30)
	if err != nil {
		return nil, err
	}
	var out []ci.RunDetail
	for _, r := range runs {
		if r.Active() {
			out = append(out, r)
		}
	}
	return out, nil
}

func printRuns(rs []ci.RunDetail) {
	rows := [][]string{{"RUN", "STATUS", "STARTED BY", "CREATED", "LINK"}}
	for _, r := range rs {
		rows = append(rows, []string{strconv.FormatInt(r.ID, 10), r.Status, or(r.Triggering.Login, "?"), r.Created.Local().Format("15:04:05"), r.URL})
	}
	ui.Table(ui.Stdout, rows, true)
}

// gitFetchMain refreshes origin/main in the cluster checkout.
func gitFetchMain(dir string) error {
	_, err := sh.Run(sh.Cmd{Name: "git", Args: []string{"-C", dir, "fetch", "--quiet", "origin", "main"}, Timeout: 2 * time.Minute})
	return err
}

// showAtMain returns a file's content on origin/main.
func showAtMain(dir, rel string) (string, error) {
	r, err := sh.Run(sh.Cmd{Name: "git", Args: []string{"-C", dir, "show", "origin/main:" + filepath.ToSlash(rel)}, Timeout: time.Minute})
	if err != nil {
		return "", err
	}
	return r.Stdout, nil
}

// ---- ci deploy (plan T2.1) ----

func runCIDeploy(c *cli.Ctx) error {
	m, l, gh, err := officialPipeline(c, "ci deploy")
	if err != nil {
		return err
	}
	env, version, isCheck := c.Arg(0), c.Arg(1), c.Bool("check")
	wf, ok, err := l.FindDispatch("environment", "version")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("found no single deploy workflow (a workflow_dispatch with environment and version inputs) in .github/workflows")
	}
	if len(wf.EnvOptions) > 0 && !contains(wf.EnvOptions, env) {
		return cli.Usage("ci deploy: %s offers environments %s, not %q", wf.File, strings.Join(wf.EnvOptions, ", "), env)
	}

	// 1. refuse to queue behind another deploy: its inputs are not readable, so
	//    there is no telling whether it is a check or a real deploy (SPEC.md C.7)
	active, err := activeRuns(gh, wf.File)
	if err != nil {
		return err
	}
	if len(active) > 0 {
		ui.Warn("%d run(s) of %s are queued or running:", len(active), wf.File)
		printRuns(active)
		if !c.Bool("queue") {
			ui.Fail("not starting another deploy: it would queue behind these, and their inputs (check or real, which environment) cannot be read")
			ui.Note("wait for them to finish, or pass --queue to queue anyway")
			return cli.Failed()
		}
		ui.Note("--queue: the new run will wait behind them")
	}

	// 2. local preflight against what the deploy will read: the release record
	//    and the descriptor on the default branch head (SPEC.md C.4)
	if !c.Bool("no-local") {
		if err := gitFetchMain(m.Dir); err != nil {
			return fmt.Errorf("fetch origin/main: %w", err)
		}
		var fs []check.Finding
		if _, err := showAtMain(m.Dir, "releases/"+version+".toml"); err != nil {
			fs = append(fs, check.Finding{ID: "RELEASE_NOT_ON_MAIN", Concern: "conformance", Severity: "blocking", Subject: "release " + version,
				Cause: "releases/" + version + ".toml is not on origin/main, and the deploy reads the record from there", Fix: "merge the release record first"})
		}
		e, err := envcfg.Load(m.Dir, m.C.Name, env)
		if err != nil {
			return err
		}
		descRel := rel2(m.Dir, m.Found.Path)
		if text, err := showAtMain(m.Dir, descRel); err == nil {
			tmp, err := os.CreateTemp("", "descriptor-main-*.toml")
			if err != nil {
				return err
			}
			defer os.Remove(tmp.Name())
			_, _ = tmp.WriteString(text)
			_ = tmp.Close()
			fs = append(fs, officialEnvFindingsFor(m, l, e, env, nil, tmp.Name(), "origin/main:"+descRel)...)
		} else {
			ui.Warn("could not read %s on origin/main; checking the local copy", descRel)
			fs = append(fs, officialEnvFindings(m, l, e, env, nil)...)
		}
		check.Sort(fs)
		if len(fs) > 0 {
			check.Print(fs)
		}
		if b, _, _ := check.Count(fs); b > 0 {
			ui.Fail("local preflight found %d blocking problem(s); not starting the deploy (--no-local skips the preflight)", b)
			return cli.Failed()
		}
		ui.OK("local preflight passed (release record and required keys, against origin/main)")
	}

	if c.Bool("dry-run") {
		mode := "real"
		if isCheck {
			mode = "check"
		}
		ui.Note("--dry-run: would start %s with version=%s environment=%s check=%v (a %s deploy); nothing started", wf.File, version, env, isCheck, mode)
		return nil
	}

	// 3. confirm a real deploy
	what := "check-mode (dry-run) deploy"
	if !isCheck {
		what = "REAL deploy"
		if !ui.Confirm(fmt.Sprintf("start a real deploy of release %s to %s?", version, env), c.G.Yes) {
			return fmt.Errorf("not confirmed (pass --yes to skip the question)")
		}
	}

	// 4. start it and find the new run
	me, _ := gh.Login()
	started := time.Now().Add(-10 * time.Second)
	if err := gh.Dispatch(wf.File, "main", map[string]string{"version": version, "environment": env, "check": strconv.FormatBool(isCheck)}); err != nil {
		return err
	}
	ui.Step("started a %s of %s to %s (%s)", what, version, env, wf.File)
	var run ci.RunDetail
	for i := 0; i < 20 && run.ID == 0; i++ {
		time.Sleep(3 * time.Second)
		runs, err := gh.WorkflowRuns(wf.File, 10)
		if err != nil {
			continue
		}
		for _, r := range runs {
			if r.Event == "workflow_dispatch" && r.Created.After(started) && (me == "" || r.Triggering.Login == me) {
				run = r
				break
			}
		}
	}
	if run.ID == 0 {
		ui.Warn("started, but could not find the new run yet; see the Actions tab")
		return nil
	}
	ui.Sub("run %d: %s", run.ID, run.URL)
	if c.Bool("no-watch") {
		return nil
	}
	return watchRun(gh, run.ID)
}

// watchRun polls a run until it finishes and prints its outcome lines.
func watchRun(gh *ci.GitHub, id int64) error {
	last := ""
	var r ci.RunDetail
	for {
		var err error
		r, err = gh.GetRunDetail(id)
		if err == nil && r.Status != last {
			ui.Sub("%s", r.Status)
			last = r.Status
		}
		if err == nil && r.Done() {
			break
		}
		time.Sleep(15 * time.Second)
	}
	jobs, _ := gh.Jobs(id)
	for _, j := range jobs {
		log, err := gh.JobLog(j.ID)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(log, "\n") {
			l := strings.TrimSpace(stampOff(line))
			if strings.Contains(l, "check passed:") || strings.HasPrefix(l, "✓ release ") || strings.HasPrefix(l, "mycel: ") {
				ui.Out("  %s", l)
			}
		}
		if j.Failed() {
			ui.Out("%s", ci.FailureExcerpt(log, 20))
		}
	}
	if !r.OK() {
		ui.Fail("run %d finished: %s (%s)", id, r.Conclusion, r.URL)
		return cli.Failed()
	}
	ui.OK("run %d finished: %s", id, r.Conclusion)
	return nil
}

var stampLine = regexp.MustCompile(`^\x{FEFF}?\d{4}-\d{2}-\d{2}T[0-9:.]+Z `)

func stampOff(s string) string { return stampLine.ReplaceAllString(strings.TrimRight(s, "\r"), "") }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---- release next (plan T2.4) ----

var versionRE = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)

func runReleaseNext(c *cli.Ctx) error {
	m, err := controller(c)
	if err != nil {
		return err
	}
	versions := map[string]bool{}
	rs, _ := release.List(m.Dir)
	for _, r := range rs {
		versions[r.Version()] = true
	}
	l, _ := officialLayout(m.Dir)
	if l != nil {
		// the default branch may already hold records this checkout lacks
		if gitFetchMain(m.Dir) == nil {
			if out, err := sh.Output("git", "-C", m.Dir, "ls-tree", "--name-only", "origin/main", "releases/"); err == nil {
				for _, f := range strings.Fields(out) {
					versions[strings.TrimSuffix(filepath.Base(f), ".toml")] = true
				}
			}
		}
	}
	series := c.String("series")
	type v3 struct{ a, b, p int }
	var all []v3
	for v := range versions {
		if mm := versionRE.FindStringSubmatch(v); mm != nil {
			a, _ := strconv.Atoi(mm[1])
			b, _ := strconv.Atoi(mm[2])
			p, _ := strconv.Atoi(mm[3])
			all = append(all, v3{a, b, p})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].a != all[j].a {
			return all[i].a < all[j].a
		}
		if all[i].b != all[j].b {
			return all[i].b < all[j].b
		}
		return all[i].p < all[j].p
	})
	if series == "" {
		if len(all) == 0 {
			series = release.LoadPolicy(m.Dir).Series
		} else {
			last := all[len(all)-1]
			series = fmt.Sprintf("%d.%d", last.a, last.b)
		}
	}
	next := 0
	for _, v := range all {
		if fmt.Sprintf("%d.%d", v.a, v.b) == series && v.p >= next {
			next = v.p + 1
		}
	}
	ui.Out("%s.%d", series, next)
	if l != nil && !c.G.Quiet {
		if _, _, gh, err := officialPipeline(c, "release next"); err == nil {
			if wf, ok, _ := l.FindDispatch("version", "series"); ok {
				if active, err := activeRuns(gh, wf.File); err == nil && len(active) > 0 {
					ui.Warn("%d release run(s) are queued or running and may take this version:", len(active))
					printRuns(active)
				}
			}
		}
	}
	return nil
}

// ---- release pin (plan T2.2) ----

func runReleasePin(c *cli.Ctx) error {
	m, l, gh, err := officialPipeline(c, "release pin")
	if err != nil {
		return err
	}
	_ = m
	var only []string
	for _, v := range c.Strings("only") {
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				only = append(only, t)
			}
		}
	}
	keep := func(image string) bool {
		if len(only) == 0 {
			return true
		}
		base := image[strings.LastIndex(image, "/")+1:]
		for _, t := range only {
			if base == t || strings.HasSuffix(base, "-"+t) {
				return true
			}
		}
		return false
	}
	built := map[string]string{} // image → digest
	from := map[string]int64{}
	for _, s := range c.Strings("from-run") {
		for _, part := range strings.Split(s, ",") {
			id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
			if err != nil {
				return cli.Usage("release pin: --from-run wants run ids, got %q", part)
			}
			r, err := gh.GetRunDetail(id)
			if err != nil {
				return err
			}
			if !r.Done() {
				return fmt.Errorf("run %d is still %s", id, r.Status)
			}
			jobs, err := gh.Jobs(id)
			if err != nil {
				return err
			}
			for _, j := range jobs {
				if j.Failed() {
					ui.Warn("run %d job %q failed; its images are skipped", id, j.Name)
					continue
				}
				log, err := gh.JobLog(j.ID)
				if err != nil {
					return err
				}
				for img, d := range ci.PushedImages(log) {
					if !keep(img) {
						continue
					}
					if old, dup := built[img]; dup && old != d {
						return fmt.Errorf("%s was pushed with two digests (%s, %s); pin from one run at a time", img, official.ShortDigest(old), official.ShortDigest(d))
					}
					built[img], from[img] = d, id
				}
			}
		}
	}
	if len(built) == 0 {
		return fmt.Errorf("found no pushed images in those runs' logs")
	}
	pins, err := l.Pins()
	if err != nil {
		return err
	}
	type edit struct{ file, image, old, new string }
	var edits []edit
	pinned := map[string]bool{}
	for _, p := range pins {
		d, ok := built[p.Image]
		if !ok {
			continue
		}
		pinned[p.Image] = true
		if p.Digest != d {
			edits = append(edits, edit{p.File, p.Image, p.Digest, d})
		}
	}
	var imgs []string
	for img := range built {
		imgs = append(imgs, img)
	}
	sort.Strings(imgs)
	rows := [][]string{{"IMAGE", "DIGEST", "RUN", "PINNED IN"}}
	for _, img := range imgs {
		n := 0
		for _, p := range pins {
			if p.Image == img {
				n++
			}
		}
		where := fmt.Sprintf("%d place(s)", n)
		if !pinned[img] {
			where = "not pinned anywhere"
		}
		rows = append(rows, []string{img, official.ShortDigest(built[img]), strconv.FormatInt(from[img], 10), where})
	}
	ui.Table(ui.Stdout, rows, true)
	if len(edits) == 0 {
		ui.OK("every pin already names these digests")
		return nil
	}
	byFile := map[string][]edit{}
	for _, e := range edits {
		byFile[e.file] = append(byFile[e.file], e)
	}
	var files []string
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		for _, e := range byFile[f] {
			ui.Sub("%s: %s %s → %s", f, e.image[strings.LastIndex(e.image, "/")+1:], official.ShortDigest(e.old), official.ShortDigest(e.new))
		}
	}
	if c.Bool("dry-run") {
		ui.Note("--dry-run: nothing written (%d change(s) in %d file(s))", len(edits), len(files))
		return nil
	}
	for _, f := range files {
		p := filepath.Join(l.Root, f)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		text := string(b)
		for _, e := range byFile[f] {
			re := regexp.MustCompile(regexp.QuoteMeta(e.image) + `((?::[A-Za-z0-9._-]+)?)@` + regexp.QuoteMeta(e.old))
			text = re.ReplaceAllString(text, e.image+"${1}@"+e.new)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			return err
		}
	}
	ui.OK("updated %d pin(s) in %d file(s)", len(edits), len(files))
	fs, err := official.CheckPins(l)
	if err != nil {
		return err
	}
	if len(fs) > 0 {
		check.Print(fs)
		return cli.Failed()
	}
	ui.OK("pins agree across member files and workflows")
	ui.Note("review with `git diff`, then commit on a branch and open a pull request to main")
	return nil
}

// ---- release record (plan T2.3) ----

var recordName = regexp.MustCompile(`^\d+\.\d+\.\d+\.toml$`)

func runReleaseRecord(c *cli.Ctx) error {
	m, l, gh, err := officialPipeline(c, "release record")
	if err != nil {
		return err
	}
	id, err := strconv.ParseInt(c.String("from-run"), 10, 64)
	if err != nil {
		return cli.Usage("release record: --from-run wants the release run id")
	}
	r, err := gh.GetRunDetail(id)
	if err != nil {
		return err
	}
	if !r.OK() {
		return fmt.Errorf("run %d is %s/%s, not a successful release", id, r.Status, r.Conclusion)
	}
	arts, err := gh.Artifacts(id)
	if err != nil {
		return err
	}
	var files map[string][]byte
	for _, a := range arts {
		if a.Expired {
			continue
		}
		got, err := gh.ArtifactFiles(a.ID, recordName, 1<<20)
		if err != nil {
			return err
		}
		if len(got) > 0 {
			if files != nil {
				return fmt.Errorf("run %d has more than one artifact holding a release record", id)
			}
			files = got
		}
	}
	if len(files) != 1 {
		return fmt.Errorf("run %d has no single release record artifact (found %d record files)", id, len(files))
	}
	var name string
	var data []byte
	for k, v := range files {
		name, data = k, v
	}
	version := strings.TrimSuffix(name, ".toml")
	// parse the downloaded bytes from an isolated temp dir before trusting them
	tmpDir, err := os.MkdirTemp("", "release-record-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	tmp := filepath.Join(tmpDir, name)
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	rec, err := release.Load(tmp)
	if err != nil {
		return fmt.Errorf("downloaded record does not parse: %w", err)
	}
	if rec.Version() != version || rec.Str("commit") == "" || rec.Str("digest") == "" || len(rec.Table("modules")) == 0 {
		return fmt.Errorf("downloaded %s is not a complete release record (version %q, commit, digest and modules required)", name, rec.Version())
	}
	dest := filepath.Join(release.Dir(m.Dir), name)
	if old, err := os.ReadFile(dest); err == nil {
		if string(old) != string(data) {
			return fmt.Errorf("%s already exists with different content; release versions are immutable", rel2(m.Dir, dest))
		}
		ui.Note("%s already holds this record", rel2(m.Dir, dest))
	} else if err := os.WriteFile(dest, data, 0o644); err != nil {
		return err
	} else {
		ui.OK("wrote %s (run %d, %s, built from %s)", rel2(m.Dir, dest), id, rec.Str("tool_version"), official.ShortCommit(rec.Str("commit")))
	}

	if env := c.String("select"); env != "" {
		p := envcfg.PathFor(m.Dir, env)
		b, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("environment %s: %w", env, err)
		}
		doc := tomledit.Parse(string(b))
		prev, _ := doc.Get("", "release")
		doc.Set("", "release", tomledit.Quote(version))
		if err := os.WriteFile(p, []byte(doc.String()), 0o644); err != nil {
			return err
		}
		prev = strings.Trim(prev, `"`)
		ui.OK("%s: release %s → %s", rel2(m.Dir, p), or(prev, "(none)"), version)
		if prev != "" && prev != version {
			if a, err := loadRelease(m, prev); err == nil {
				ch := attribute(m, a, rec, release.Diff(a, rec))
				ui.Note("%d change(s) since %s; `axon release diff %s %s --markdown` gives a pull request body", len(ch), prev, prev, version)
			}
		}
	}
	_ = l
	ui.Note("commit on a branch and open a pull request to main; the deploy reads the record from main")
	return nil
}
