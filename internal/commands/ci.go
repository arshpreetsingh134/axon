package commands

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"axon/internal/ci"
	"axon/internal/cli"
	"axon/internal/sh"
	"axon/internal/ui"
)

func ciVerbs() []*cli.Verb {
	repoFlags := []cli.Flag{flagDir, {Name: "remote", Kind: cli.String, Default: "origin", Usage: "git remote that points at GitHub"}}
	return []*cli.Verb{
		{Name: "ci deploy", Args: []cli.Arg{{Name: "env"}, {Name: "release"}}, Group: "ci",
			Summary: "start the cluster's deploy pipeline for a recorded release (--check for a dry run), with guards and a local preflight",
			Help: "For a pipeline-released cluster (github/workflow.toml). Refuses while another run of the deploy workflow is queued or " +
				"running (its inputs cannot be read, so a new run could silently queue behind a real deploy); --queue overrides. Then " +
				"checks, against origin/main (what the deploy reads), that the release record is merged and that the environment's " +
				"secret holds every required key (see `axon env check`); --no-local skips that. A real deploy asks for confirmation " +
				"(--yes skips it). Finally starts the run and follows it to the end (--no-watch returns at once).",
			Flags: []cli.Flag{flagDir, {Name: "check", Kind: cli.Bool, Usage: "rehearse: every apply and delete is a server-side dry run"},
				{Name: "queue", Kind: cli.Bool, Usage: "start even if another deploy run is queued or running"},
				{Name: "no-local", Kind: cli.Bool, Usage: "skip the local preflight"},
				{Name: "no-watch", Kind: cli.Bool, Usage: "return once the run has started"},
				{Name: "dry-run", Kind: cli.Bool, Usage: "run the guards and the preflight, then stop before starting anything"}},
			Run: runCIDeploy},
		{Name: "ci status", Group: "ci", Summary: "CI status of the current commit: every workflow run and job, with durations and links",
			Flags: append(repoFlags, flagJSON, cli.Flag{Name: "ref", Kind: cli.String, Usage: "a commit or branch instead of HEAD"}),
			Run:   runCIStatus},
		{Name: "ci watch", Group: "ci", Summary: "wait for the current commit's CI to finish, printing each job as it completes (exit 1 if anything fails)",
			Flags: append(repoFlags, cli.Flag{Name: "ref", Kind: cli.String, Usage: "a commit or branch instead of HEAD"},
				cli.Flag{Name: "timeout", Kind: cli.String, Default: "60m", Usage: "give up after this long"}),
			Run: runCIWatch},
		{Name: "ci logs", Group: "ci", Summary: "print CI job logs; --failed prints only the failing step of each failed job",
			Flags: append(repoFlags,
				cli.Flag{Name: "failed", Kind: cli.Bool, Usage: "only failed jobs, trimmed to the failing step and its error"},
				cli.Flag{Name: "run", Kind: cli.String, Usage: "a run ID (default: the latest run of the current commit)"},
				cli.Flag{Name: "job", Kind: cli.String, Usage: "only jobs whose name contains this text"},
				cli.Flag{Name: "lines", Kind: cli.Int, Default: "60", Usage: "with --failed: lines to keep per job"},
				cli.Flag{Name: "ref", Kind: cli.String, Usage: "a commit or branch instead of HEAD"}),
			Run: runCILogs},
		{Name: "release cut", Group: "release",
			Summary: "tag the next version (vX.Y.Z) once CI is green, push the tag and follow the release pipeline",
			Help: "Cut a release from the current commit: work out the next version from the latest vX.Y.Z tag (--bump patch|minor|major, " +
				"or --version), refuse if tracked files are modified, if commits are not pushed, or if CI on HEAD is not green, then create " +
				"an annotated tag, push it, and wait for the workflow it triggers (e.g. GoReleaser) to finish. Works in any GitHub " +
				"repository, with or without an axonfile. The tag is pushed with the repository owner's GitHub account when the GitHub " +
				"CLI is signed in to several.",
			Flags: append(repoFlags,
				cli.Flag{Name: "bump", Kind: cli.String, Default: "patch", Usage: "patch | minor | major"},
				cli.Flag{Name: "version", Kind: cli.String, Usage: "explicit version (e.g. v1.2.0) instead of a bump"},
				cli.Flag{Name: "skip-ci-check", Kind: cli.Bool, Usage: "tag even if CI on HEAD is not green"},
				cli.Flag{Name: "no-wait", Kind: cli.Bool, Usage: "do not wait for the release pipeline"},
				cli.Flag{Name: "timeout", Kind: cli.String, Default: "60m", Usage: "how long to wait for CI and the release pipeline"},
				flagDryRun),
			Run: runReleaseCut},
	}
}

type ciCtx struct {
	repo   *ci.Repo
	gh     *ci.GitHub
	sha    string
	branch string
}

func openCI(c *cli.Ctx) (*ciCtx, error) {
	repo, err := ci.OpenRepo(or(c.String("dir"), "."), c.String("remote"))
	if err != nil {
		return nil, err
	}
	sha, branch, err := repo.Head()
	if err != nil {
		return nil, err
	}
	if ref := c.String("ref"); ref != "" {
		if s, err := gitRevParse(repo.Dir, ref); err == nil {
			sha = s
			branch = ref
		} else {
			return nil, fmt.Errorf("unknown ref %q", ref)
		}
	}
	return &ciCtx{repo: repo, gh: ci.NewGitHub(repo.Owner, repo.Name), sha: sha, branch: branch}, nil
}

func gitRevParse(dir, ref string) (string, error) {
	r, err := runGit(dir, "rev-parse", ref+"^{commit}")
	return strings.TrimSpace(r), err
}

func short7(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func dur(a, b time.Time) string {
	if a.IsZero() || b.IsZero() || b.Before(a) {
		return "-"
	}
	d := b.Sub(a).Round(time.Second)
	if d >= time.Minute {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return d.String()
}

func runMark(status, conclusion string) string {
	switch {
	case status != "completed":
		return ui.Yellow("●") + " " + strings.ReplaceAll(status, "_", " ")
	case conclusion == "success":
		return ui.Green("✓") + " success"
	case conclusion == "skipped" || conclusion == "neutral":
		return ui.Dim("·") + " " + conclusion
	default:
		return ui.Red("✗") + " " + conclusion
	}
}

func runCIStatus(c *cli.Ctx) error {
	x, err := openCI(c)
	if err != nil {
		return err
	}
	runs, err := x.gh.RunsForCommit(x.sha)
	if err != nil {
		return err
	}
	runs = latestPerWorkflow(runs)
	type jobOut struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		Duration   string `json:"duration"`
		URL        string `json:"url"`
	}
	type runOut struct {
		ci.Run
		Jobs []jobOut `json:"jobs"`
	}
	var out []runOut
	failed, pending := 0, 0
	for _, r := range runs {
		jobs, err := x.gh.Jobs(r.ID)
		if err != nil {
			return err
		}
		ro := runOut{Run: r}
		for _, j := range jobs {
			ro.Jobs = append(ro.Jobs, jobOut{j.Name, j.Status, j.Conclusion, dur(j.Started, j.Completed), j.URL})
		}
		out = append(out, ro)
		switch {
		case !r.Done():
			pending++
		case !r.OK():
			failed++
		}
	}
	verdict := "all checks passed"
	switch {
	case len(runs) == 0:
		verdict = "no CI runs for this commit"
	case failed > 0:
		verdict = fmt.Sprintf("%d workflow(s) failed", failed)
	case pending > 0:
		verdict = fmt.Sprintf("%d workflow(s) still running", pending)
	}
	if c.Bool("json") {
		if out == nil {
			out = []runOut{}
		}
		ui.JSON(map[string]any{"repository": x.repo.Owner + "/" + x.repo.Name, "commit": x.sha, "branch": x.branch, "verdict": verdict, "runs": out})
	} else {
		mark := ui.Green("✓")
		if failed > 0 {
			mark = ui.Red("✗")
		} else if pending > 0 || len(runs) == 0 {
			mark = ui.Yellow("●")
		}
		ui.Out("%s/%s @ %s (%s) — %s %s", x.repo.Owner, x.repo.Name, short7(x.sha), or(x.branch, "detached"), mark, verdict)
		if len(runs) == 0 && x.repo.Unpushed() > 0 {
			ui.Out("  this commit is not pushed yet — git push")
		}
		for _, r := range out {
			ui.Out("")
			ui.Out("%s  %s  %s  %s", ui.Bold(r.Name), runMark(r.Status, r.Conclusion), dur(r.Started, r.Updated), r.URL)
			for _, j := range r.Jobs {
				ui.Out("  %s  %s  %s", runMark(j.Status, j.Conclusion), j.Name, ui.Dim(j.Duration))
			}
		}
	}
	if failed > 0 {
		return cli.Failed()
	}
	return nil
}

// latestPerWorkflow keeps the newest run of each workflow (re-runs replace older ones).
func latestPerWorkflow(runs []ci.Run) []ci.Run {
	seen := map[string]bool{}
	var out []ci.Run
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].Created.After(runs[j].Created) })
	for _, r := range runs {
		if !seen[r.Name] {
			seen[r.Name] = true
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// watchRuns polls fetch until every run is completed, printing each job once it
// finishes. It returns the final runs; appear is how long to wait for a first run.
func watchRuns(x *ciCtx, fetch func() ([]ci.Run, error), timeout, appear time.Duration) ([]ci.Run, error) {
	deadline := time.Now().Add(timeout)
	firstSeen := time.Now().Add(appear)
	reported := map[int64]bool{}
	announced := map[int64]bool{}
	for {
		runs, err := fetch()
		if err != nil {
			return nil, err
		}
		runs = latestPerWorkflow(runs)
		if len(runs) == 0 && time.Now().After(firstSeen) {
			return nil, nil
		}
		allDone := len(runs) > 0
		for _, r := range runs {
			if !announced[r.ID] {
				announced[r.ID] = true
				ui.Sub("%s started — %s", r.Name, r.URL)
			}
			jobs, err := x.gh.Jobs(r.ID)
			if err != nil {
				return nil, err
			}
			for _, j := range jobs {
				if j.Status == "completed" && !reported[j.ID] {
					reported[j.ID] = true
					ui.Sub("%s  %s / %s  %s", runMark(j.Status, j.Conclusion), r.Name, j.Name, ui.Dim(dur(j.Started, j.Completed)))
				}
			}
			allDone = allDone && r.Done()
		}
		if allDone {
			return runs, nil
		}
		if time.Now().After(deadline) {
			return runs, fmt.Errorf("timed out after %s waiting for CI", timeout)
		}
		time.Sleep(10 * time.Second)
	}
}

func parseTimeout(c *cli.Ctx) (time.Duration, error) {
	d, err := time.ParseDuration(c.String("timeout"))
	if err != nil {
		return 0, cli.Usage("%s: --timeout wants a duration like 30m", c.Verb.Name)
	}
	return d, nil
}

func summarize(runs []ci.Run) (failed []string) {
	for _, r := range runs {
		if !r.OK() {
			failed = append(failed, r.Name+" ("+r.Conclusion+")")
		}
	}
	return
}

func runCIWatch(c *cli.Ctx) error {
	timeout, err := parseTimeout(c)
	if err != nil {
		return err
	}
	x, err := openCI(c)
	if err != nil {
		return err
	}
	ui.Step("watching CI for %s/%s @ %s", x.repo.Owner, x.repo.Name, short7(x.sha))
	runs, err := watchRuns(x, func() ([]ci.Run, error) { return x.gh.RunsForCommit(x.sha) }, timeout, 2*time.Minute)
	if err != nil {
		return err
	}
	if runs == nil {
		return fmt.Errorf("no CI run started for %s (is the commit pushed, and does the repository have workflows?)", short7(x.sha))
	}
	if f := summarize(runs); len(f) > 0 {
		ui.Fail("CI failed: %s — `axon ci logs --failed` shows why", strings.Join(f, ", "))
		return cli.Failed()
	}
	ui.OK("CI passed for %s", short7(x.sha))
	return nil
}

func runCILogs(c *cli.Ctx) error {
	x, err := openCI(c)
	if err != nil {
		return err
	}
	var runs []ci.Run
	if id := c.String("run"); id != "" {
		var n int64
		if _, err := fmt.Sscan(id, &n); err != nil {
			return cli.Usage("ci logs: --run wants a numeric run ID")
		}
		r, err := x.gh.GetRun(n)
		if err != nil {
			return err
		}
		runs = []ci.Run{r}
	} else {
		all, err := x.gh.RunsForCommit(x.sha)
		if err != nil {
			return err
		}
		runs = latestPerWorkflow(all)
		if len(runs) == 0 {
			return fmt.Errorf("no CI runs for %s; pass --run <id>", short7(x.sha))
		}
	}
	printed := 0
	for _, r := range runs {
		jobs, err := x.gh.Jobs(r.ID)
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if c.Bool("failed") && !j.Failed() {
				continue
			}
			if f := c.String("job"); f != "" && !strings.Contains(j.Name, f) {
				continue
			}
			log, err := x.gh.JobLog(j.ID)
			var apiErr *ci.APIError
			noLog := errors.As(err, &apiErr) && apiErr.Status == 404 && !j.Started.IsZero() && j.Conclusion == "cancelled"
			if err != nil && !noLog && !(errors.As(err, &apiErr) && apiErr.Status == 404 && j.Conclusion != "failure") {
				return err
			}
			if printed > 0 {
				ui.Out("")
			}
			printed++
			ui.Out("%s %s / %s — %s  %s", ui.Bold("──"), r.Name, j.Name, runMark(j.Status, j.Conclusion), j.URL)
			if err != nil {
				ui.Out("no log: the job was %s before it produced output", j.Conclusion)
				continue
			}
			if c.Bool("failed") {
				for _, s := range j.Steps {
					if s.Conclusion == "failure" {
						ui.Out("failed step: %s", s.Name)
					}
				}
				ui.Out("%s", ci.FailureExcerpt(log, c.Int("lines")))
			} else {
				ui.Raw(log)
			}
		}
	}
	if printed == 0 {
		if c.Bool("failed") {
			ui.Out("no failed jobs for %s", short7(x.sha))
		} else {
			ui.Out("no jobs matched")
		}
	}
	return nil
}

func runReleaseCut(c *cli.Ctx) error {
	timeout, err := parseTimeout(c)
	if err != nil {
		return err
	}
	x, err := openCI(c)
	if err != nil {
		return err
	}
	dry := c.Bool("dry-run")
	// 1. version
	var next ci.Version
	if v := c.String("version"); v != "" {
		var ok bool
		if next, ok = ci.ParseVersion(v); !ok {
			return cli.Usage("release cut: --version wants vMAJOR.MINOR.PATCH, got %q", v)
		}
	} else {
		latest, ok := x.repo.LatestTag()
		if !ok {
			next = ci.Version{Major: 0, Minor: 1, Patch: 0}
		} else if next, err = latest.Bump(c.String("bump")); err != nil {
			return cli.Usage("release cut: %v", err)
		}
	}
	tag := next.String()
	ui.Step("release %s of %s/%s from %s (%s)", tag, x.repo.Owner, x.repo.Name, short7(x.sha), or(x.branch, "detached"))
	// 2. guards
	if x.repo.TagExists(tag) {
		return fmt.Errorf("tag %s already exists; choose another --version or --bump", tag)
	}
	if dirty, err := x.repo.Dirty(); err != nil {
		return err
	} else if len(dirty) > 0 {
		return fmt.Errorf("tracked files have uncommitted changes (%d); commit or stash them first:\n  %s", len(dirty), strings.Join(dirty, "\n  "))
	}
	switch n := x.repo.Unpushed(); {
	case n > 0:
		return fmt.Errorf("%d commit(s) are not pushed; `git push` first so CI tests exactly what you release", n)
	case n < 0:
		ui.Warn("the branch has no upstream; make sure %s is on the remote", short7(x.sha))
	}
	// 3. CI must be green on HEAD
	if c.Bool("skip-ci-check") {
		ui.Warn("skipping the CI check (--skip-ci-check)")
	} else {
		runs, err := x.gh.RunsForCommit(x.sha)
		if err != nil {
			return err
		}
		runs = latestPerWorkflow(runs)
		if len(runs) == 0 {
			return fmt.Errorf("no CI run found for %s; push it and let CI run, or pass --skip-ci-check", short7(x.sha))
		}
		pending := false
		for _, r := range runs {
			pending = pending || !r.Done()
		}
		if pending && !dry {
			ui.Sub("CI is still running on %s; waiting", short7(x.sha))
			if runs, err = watchRuns(x, func() ([]ci.Run, error) { return x.gh.RunsForCommit(x.sha) }, timeout, time.Minute); err != nil {
				return err
			}
		}
		if f := summarize(runs); len(f) > 0 && !(dry && pending) {
			return fmt.Errorf("CI is not green on %s: %s — fix it first (`axon ci logs --failed`)", short7(x.sha), strings.Join(f, ", "))
		}
		if pending && dry {
			ui.Sub("CI is still running on %s (a real cut would wait for it)", short7(x.sha))
		} else {
			ui.OK("CI is green on %s", short7(x.sha))
		}
	}
	if dry {
		ui.Out("would create annotated tag %s at %s and push it to %s", tag, short7(x.sha), x.repo.Remote)
		ui.OK("plan complete — nothing was changed")
		return nil
	}
	// 4. tag and push
	if err := x.repo.Tag(tag, fmt.Sprintf("%s %s", x.repo.Name, tag)); err != nil {
		return fmt.Errorf("create tag: %w", err)
	}
	if err := x.repo.PushTag(tag, x.gh.Token); err != nil {
		x.repo.DeleteLocalTag(tag)
		return fmt.Errorf("push tag %s (the local tag was removed again): %w", tag, err)
	}
	ui.OK("tagged %s and pushed it", tag)
	if c.Bool("no-wait") {
		return nil
	}
	// 5. follow the pipeline the tag triggered
	ui.Step("release pipeline")
	runs, err := watchRuns(x, func() ([]ci.Run, error) { return x.gh.RunsForBranch(tag, 10) }, timeout, 2*time.Minute)
	if err != nil {
		return err
	}
	if runs == nil {
		ui.Note("no workflow ran for %s (the repository has no tag-triggered workflow)", tag)
		return nil
	}
	if f := summarize(runs); len(f) > 0 {
		ui.Fail("release pipeline failed: %s — `axon ci logs --failed --ref %s`", strings.Join(f, ", "), tag)
		return cli.Failed()
	}
	if u := x.gh.ReleaseURL(tag); u != "" {
		ui.OK("released %s — %s", tag, u)
	} else {
		ui.OK("release pipeline for %s succeeded", tag)
	}
	return nil
}

func runGit(dir string, args ...string) (string, error) {
	r, err := sh.Run(sh.Cmd{Name: "git", Args: append([]string{"-C", dir}, args...), Timeout: time.Minute})
	if err != nil {
		return "", err
	}
	return r.Stdout, nil
}
