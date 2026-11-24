package commands

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"axon/internal/cli"
	"axon/internal/descriptor"
	"axon/internal/official"
	"axon/internal/release"
	"axon/internal/sh"
	"axon/internal/ui"
)

// attributedChange is a release difference with where it came from.
type attributedChange struct {
	release.Change
	Commit  string `json:"commit,omitempty"`  // cluster-repository commit that moved the pin
	Subject string `json:"subject,omitempty"` // its subject line
	PR      string `json:"pr,omitempty"`      // "#123" when a merge commit names it
	Rebuilt bool   `json:"rebuilt,omitempty"` // the image changed with no pin change: rebuilt by the release
}

var (
	digestInRef = regexp.MustCompile(`sha256:[0-9a-f]{64}`)
	mergePR     = regexp.MustCompile(`^Merge pull request (#[0-9]+)`)
)

// pickaxe finds the first commit in from..to (oldest first) that added needle,
// outside release records.
func pickaxe(dir, from, to, needle string) (commit, subject string) {
	if from == "" || to == "" || needle == "" {
		return "", ""
	}
	out, err := sh.Output("git", "-C", dir, "log", "--reverse", "--no-merges", "--format=%H%x09%s", "-S"+needle, from+".."+to, "--", ".", ":(exclude)releases")
	if err != nil || out == "" {
		return "", ""
	}
	first := strings.SplitN(strings.Split(out, "\n")[0], "\t", 2)
	if len(first) == 2 {
		return first[0], first[1]
	}
	return first[0], ""
}

// prFor finds the pull request whose merge brought commit into to: the merge M
// on the ancestry path where commit is reachable from M's second parent but
// not from its first. A commit pushed straight to the branch has none.
func prFor(dir, commit, to string) string {
	out, err := sh.Output("git", "-C", dir, "log", "--merges", "--ancestry-path", "--reverse", "--format=%H%x09%s", commit+".."+to)
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(out, "\n") {
		h, subj, ok := strings.Cut(l, "\t")
		if !ok {
			continue
		}
		if isAncestor(dir, commit, h+"^1") || !isAncestor(dir, commit, h+"^2") {
			continue
		}
		if m := mergePR.FindStringSubmatch(subj); m != nil {
			return m[1]
		}
		return ""
	}
	return ""
}

func isAncestor(dir, a, b string) bool {
	_, err := sh.Output("git", "-C", dir, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// materializedDigest is the digest a member's materialization pinned at rev.
func materializedDigest(dir, rev, member string) string {
	for _, f := range []string{"mycelfile", "axonfile", ".mycel.infra/Dockerfile", ".axon.infra/Dockerfile"} {
		out, err := sh.Output("git", "-C", dir, "show", rev+":"+filepath.ToSlash(filepath.Join(official.MaterializationsDir, member, f)))
		if err == nil {
			if d := digestInRef.FindString(out); d != "" {
				return d
			}
		}
	}
	return ""
}

func attribute(m *descriptor.Model, a, b *release.Record, ch []release.Change) []attributedChange {
	from, to := a.Str("commit"), b.Str("commit")
	known := from != "" && to != "" && gitHasCommit(m.Dir, from) && gitHasCommit(m.Dir, to)
	var out []attributedChange
	for _, c := range ch {
		ac := attributedChange{Change: c}
		if known && c.New != "" && c.Table != "record" {
			needle := ""
			switch c.Table {
			case "sources":
				needle = c.New
			case "modules":
				// releases re-push images, so look for the member's own pin
				nd, od := materializedDigest(m.Dir, to, c.Key), materializedDigest(m.Dir, from, c.Key)
				switch {
				case nd == "":
					needle = digestInRef.FindString(c.New) // no member file: the image may be pinned as-is elsewhere
				case nd == od:
					ac.Rebuilt = c.Old != ""
				default:
					needle = nd
				}
			default:
				needle = digestInRef.FindString(c.New)
				if needle == "" {
					needle = c.New
				}
			}
			if needle != "" {
				ac.Commit, ac.Subject = pickaxe(m.Dir, from, to, needle)
				if ac.Commit != "" {
					ac.PR = prFor(m.Dir, ac.Commit, to)
				}
			}
			if ac.Commit == "" && c.Table != "sources" && c.Old != "" {
				ac.Rebuilt = true
			}
		}
		out = append(out, ac)
	}
	return out
}

func gitHasCommit(dir, c string) bool {
	_, err := sh.Output("git", "-C", dir, "cat-file", "-e", c+"^{commit}")
	return err == nil
}

// runReleaseDiff is `release diff <a> <b>` (plan T0.4).
func runReleaseDiff(c *cli.Ctx) error {
	m, err := controller(c)
	if err != nil {
		return err
	}
	a, err := loadRelease(m, c.Arg(0))
	if err != nil {
		return err
	}
	b, err := loadRelease(m, c.Arg(1))
	if err != nil {
		return err
	}
	changes := attribute(m, a, b, release.Diff(a, b))
	if c.Bool("json") {
		if changes == nil {
			changes = []attributedChange{}
		}
		return ui.JSON(changes)
	}
	if len(changes) == 0 {
		ui.OK("%s and %s record the same images and sources", a.Version(), b.Version())
		return nil
	}
	md := c.Bool("markdown")
	cur := ""
	for _, ch := range changes {
		if ch.Table != cur {
			cur = ch.Table
			n := 0
			for _, x := range changes {
				if x.Table == cur {
					n++
				}
			}
			if md {
				ui.Raw(fmt.Sprintf("\n**%s** (%d)\n\n", cur, n))
			} else {
				ui.Out("\n%s (%d)", ui.Bold(cur), n)
			}
		}
		line := fmt.Sprintf("%s: %s → %s", ch.Key, shortRefFor(ch.Key, ch.Old), shortRefFor(ch.Key, ch.New))
		switch {
		case ch.Old == "":
			line = fmt.Sprintf("%s: added %s", ch.Key, shortRef(ch.New))
		case ch.New == "":
			line = fmt.Sprintf("%s: removed (was %s)", ch.Key, shortRef(ch.Old))
		}
		why := ""
		switch {
		case ch.Commit != "":
			why = official.ShortCommit(ch.Commit)
			if ch.PR != "" {
				why += " " + ch.PR
			}
			if ch.Subject != "" {
				why += " " + ch.Subject
			}
		case ch.Rebuilt:
			why = "rebuilt by the release (no pin change)"
		}
		if md {
			if why != "" {
				line += " (" + why + ")"
			}
			ui.Raw("- " + line + "\n")
			continue
		}
		mark := ui.Yellow("~")
		if ch.Old == "" {
			mark = ui.Green("+")
		} else if ch.New == "" {
			mark = ui.Red("-")
		}
		if why != "" {
			line += "  " + ui.Dim(why)
		}
		ui.Out("  %s %s", mark, line)
	}
	return nil
}

// shortRefFor drops the repository when its name already contains key.
func shortRefFor(key, s string) string {
	if i := strings.Index(s, "@sha256:"); i >= 0 && strings.HasSuffix(s[:i], key) && len(s) > i+20 {
		return s[i+1 : i+20]
	}
	return shortRef(s)
}

// shortRef abbreviates an image reference's digest or a commit id.
func shortRef(s string) string {
	if i := strings.Index(s, "@sha256:"); i >= 0 && len(s) > i+20 {
		repo := s[:i]
		if j := strings.LastIndex(repo, "/"); j >= 0 {
			repo = repo[j+1:]
		}
		return repo + "@" + s[i+1:i+20]
	}
	if official.IsCommit(s) {
		return official.ShortCommit(s)
	}
	return s
}
