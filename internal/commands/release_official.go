package commands

import (
	"fmt"
	"sort"
	"strings"

	"axon/internal/check"
	"axon/internal/cli"
	"axon/internal/gitmirror"
	"axon/internal/official"
	"axon/internal/ui"
)

// officialLayout returns the official layout at dir, or nil when dir has none.
func officialLayout(dir string) (*official.Layout, error) {
	l, ok, err := official.Detect(dir)
	if err != nil || !ok {
		return nil, err
	}
	if v := l.Workflow.RunnerVersion; v != "" && v != official.ConformsTo {
		ui.Note("the pipeline pins tool %s; axon's official checks were verified against %s (plan D3)", v, official.ConformsTo)
	}
	return l, nil
}

// lockSubject is one commit the main check looks at.
type lockSubject struct {
	Name   string `json:"name"`
	Source string `json:"source"` // "lock" | "<workflow file>:<line>"
	Repo   string `json:"repository"`
	Commit string `json:"commit"`
	url    string

	Result *gitmirror.Ancestry `json:"result,omitempty"`
	Error  string              `json:"error,omitempty"`
}

// runOfficialLockCheck is `release lock --check` on an official layout: every
// locked commit (and every commit a build workflow pins) must be reachable from
// its repository's default branch. Ancestry is decided with Git on cached
// mirrors, never with a compare API (SPEC.md Appendix C.3).
func runOfficialLockCheck(c *cli.Ctx, l *official.Layout) error {
	lock, err := l.Lock()
	if err != nil {
		return err
	}
	var subjects []*lockSubject
	seen := map[string]bool{}
	for _, m := range lock {
		r, err := official.ParseRepo(m.Repository, l.Host(), l.Owner())
		if err != nil {
			return fmt.Errorf("%s: %w", m.AppID, err)
		}
		subjects = append(subjects, &lockSubject{Name: m.AppID, Source: "lock", Repo: r.Slug, Commit: m.Commit, url: r.URL()})
		seen[r.String()+"@"+m.Commit] = true
	}
	if !c.Bool("lock-only") {
		refs, err := l.SourceRefs()
		if err != nil {
			return err
		}
		for _, s := range refs {
			k := s.Repo.String() + "@" + s.Commit
			if seen[k] {
				continue
			}
			seen[k] = true
			subjects = append(subjects, &lockSubject{Name: s.Repo.Slug, Source: fmt.Sprintf("%s:%d", s.File, s.Line), Repo: s.Repo.Slug, Commit: s.Commit, url: s.Repo.URL()})
		}
	}

	urls := map[string]bool{}
	for _, s := range subjects {
		urls[s.url] = true
	}
	var list []string
	for u := range urls {
		list = append(list, u)
	}
	sort.Strings(list)
	if !c.Bool("json") {
		ui.Step("fetching %d source repositories into %s", len(list), gitmirror.Dir())
	}
	mirrors, errs := gitmirror.SyncAll(list, 6)
	// The pipeline's gate checks the branch named main, whatever a repository's
	// default branch is (SPEC.md Appendix C.3).
	branch := or(c.String("branch"), "main")

	blocking, warnings := 0, 0
	for _, s := range subjects {
		if err := errs[s.url]; err != nil {
			s.Error = err.Error()
		} else if a, err := mirrors[s.url].Check(s.Commit, branch); err != nil {
			s.Error = err.Error()
		} else {
			s.Result = a
		}
		bad := s.Error != "" || s.Result.Status != gitmirror.OnMain
		switch {
		case bad && s.Source == "lock":
			blocking++
		case bad:
			warnings++
		}
	}

	if c.Bool("json") {
		_ = ui.JSON(subjects)
	} else {
		rows := [][]string{{"", "SOURCE", "COMMIT", "RESULT", "FROM"}}
		for _, s := range subjects {
			mark, res := ui.Green("✓"), ""
			switch {
			case s.Error != "":
				mark, res = ui.Red("✗"), "error: "+firstLine(s.Error)
			case s.Result.Status == gitmirror.OnMain && s.Result.Behind == 0:
				res = "head of " + s.Result.Branch
			case s.Result.Status == gitmirror.OnMain:
				res = fmt.Sprintf("on %s, %d commit(s) behind", s.Result.Branch, s.Result.Behind)
			case s.Result.Status == gitmirror.NotOnMain:
				res = "not on " + s.Result.Branch
				if len(s.Result.Branches) > 0 {
					res += " (only on " + strings.Join(s.Result.Branches, ", ") + ")"
				}
			default:
				res = "commit not found on any branch"
			}
			if s.Error != "" || s.Result.Status != gitmirror.OnMain {
				if s.Source == "lock" {
					mark = ui.Red("✗")
				} else {
					mark = ui.Yellow("!")
				}
			}
			rows = append(rows, []string{mark, s.Name, official.ShortCommit(s.Commit), res, s.Source})
		}
		ui.Table(ui.Stdout, rows, true)
		switch {
		case blocking > 0:
			ui.Fail("%d locked source(s) are not on %s; the release pipeline's protected-main gate would refuse them", blocking, branch)
			ui.Note("merge the commit to %s, or move the pin to a commit that is on it", branch)
		default:
			ui.OK("every locked source is on %s (%d checked)", branch, len(lock))
		}
		if warnings > 0 {
			ui.Warn("%d build-workflow revision(s) outside the lock are not on %s or could not be read; the pipeline does not check these", warnings, branch)
		}
	}
	if blocking > 0 {
		return cli.Failed()
	}
	return nil
}

// runOfficialPinCheck is `release check --pins` on an official layout (plan T0.5).
func runOfficialPinCheck(c *cli.Ctx, l *official.Layout) error {
	fs, err := official.CheckPins(l)
	if err != nil {
		return err
	}
	if c.Bool("json") {
		_ = ui.JSON(fs)
	} else if len(fs) == 0 {
		pins, _ := l.Pins()
		ui.OK("image pins agree across member files and workflows (%d pins), and build revisions match the source lock", len(pins))
	} else {
		check.Print(fs)
	}
	if b, _, _ := check.Count(fs); b > 0 {
		return cli.Failed()
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
