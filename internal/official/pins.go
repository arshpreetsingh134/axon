package official

import (
	"fmt"
	"sort"
	"strings"

	"axon/internal/check"
)

// CheckPins verifies that the layout's pins agree with each other (plan T0.5):
//
//   - every image-backed member names its image by digest;
//   - one image repository is pinned to one digest everywhere it appears (member
//     files, Dockerfiles, workflows);
//   - a workflow that builds a locked source builds it at the locked commit.
//
// Release records are history and are not compared. Findings are sorted.
func CheckPins(l *Layout) ([]check.Finding, error) {
	var fs []check.Finding
	add := func(id, sev, subject, cause, fix string) {
		fs = append(fs, check.Finding{ID: id, Concern: "conformance", Severity: sev, Subject: subject, Cause: cause, Fix: fix})
	}

	members, err := l.Materializations()
	if err != nil {
		return nil, err
	}
	for _, m := range members {
		if len(m.Images) == 0 {
			if m.Sources == 0 {
				add("MEMBER_NO_IMAGE", "blocking", m.Name, "the member file names neither an image nor a source module",
					"set image = \"<registry>/<name>@sha256:<digest>\" in "+l.Rel(m.Dir))
			}
			continue // source-backed: built from the locked source
		}
		for _, img := range m.Images {
			if _, _, ok := ParsePin(img); !ok {
				add("IMAGE_NOT_PINNED", "blocking", m.Name, fmt.Sprintf("image %q is not pinned by digest", img),
					"pin it as <image>@sha256:<digest> (the manifest-list digest from the build)")
			}
		}
	}

	pins, err := l.Pins()
	if err != nil {
		return nil, err
	}
	byImage := map[string]map[string][]string{} // image → digest → places
	for _, p := range pins {
		if byImage[p.Image] == nil {
			byImage[p.Image] = map[string][]string{}
		}
		byImage[p.Image][p.Digest] = append(byImage[p.Image][p.Digest], fmt.Sprintf("%s:%d", p.File, p.Line))
	}
	for img, digests := range byImage {
		if len(digests) < 2 {
			continue
		}
		var parts []string
		for d, places := range digests {
			parts = append(parts, fmt.Sprintf("%s in %s", ShortDigest(d), strings.Join(places, ", ")))
		}
		sort.Strings(parts)
		add("DIGEST_DISAGREES", "blocking", img, fmt.Sprintf("pinned to %d different digests: %s", len(digests), strings.Join(parts, "; ")),
			"pin every place to the same build's digest")
	}

	lock, err := l.Lock()
	if err != nil {
		return nil, err
	}
	locked := map[string]map[string]bool{} // repo → commits
	for _, m := range lock {
		r, err := ParseRepo(m.Repository, l.Host(), l.Owner())
		if err != nil {
			add("LOCK_REPO_UNREADABLE", "blocking", m.AppID, err.Error(), "")
			continue
		}
		if locked[r.String()] == nil {
			locked[r.String()] = map[string]bool{}
		}
		locked[r.String()][m.Commit] = true
	}
	refs, err := l.SourceRefs()
	if err != nil {
		return nil, err
	}
	for _, s := range refs {
		commits, ok := locked[s.Repo.String()]
		if !ok || commits[s.Commit] {
			continue
		}
		var want []string
		for c := range commits {
			want = append(want, ShortCommit(c))
		}
		sort.Strings(want)
		add("BUILD_REVISION_NOT_LOCKED", "blocking", s.Repo.Slug,
			fmt.Sprintf("%s:%d builds %s, but the source lock pins %s", s.File, s.Line, ShortCommit(s.Commit), strings.Join(want, ", ")),
			"move the build revision and the lock entry to the same commit")
	}
	check.Sort(fs)
	return fs, nil
}

// ShortDigest abbreviates "sha256:<64-hex>" to "sha256:<12-hex>".
func ShortDigest(d string) string {
	if strings.HasPrefix(d, "sha256:") && len(d) > 19 {
		return d[:19]
	}
	return d
}

// ShortCommit abbreviates a commit id to 9 characters.
func ShortCommit(c string) string {
	if len(c) > 9 {
		return c[:9]
	}
	return c
}
