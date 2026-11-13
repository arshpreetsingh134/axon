package ci

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"axon/internal/sh"
)

// Repo is the local git checkout.
type Repo struct {
	Dir    string // top-level directory
	Remote string // remote name (origin)
	URL    string // remote URL
	Owner  string
	Name   string
	HTTPS  bool
}

func git(dir string, args ...string) (string, error) {
	r, err := sh.Run(sh.Cmd{Name: "git", Args: append([]string{"-C", dir}, args...), Timeout: 60 * time.Second})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(r.Stdout), nil
}

var remoteRE = regexp.MustCompile(`github\.com[:/]([^/]+)/([^/]+?)(?:\.git)?/?$`)

// ParseRemote extracts owner/repo from a GitHub remote URL (https or ssh).
func ParseRemote(url string) (owner, name string, https bool, ok bool) {
	m := remoteRE.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return "", "", false, false
	}
	return m[1], m[2], strings.HasPrefix(url, "https://"), true
}

// OpenRepo finds the git checkout containing dir and its GitHub remote.
func OpenRepo(dir, remote string) (*Repo, error) {
	top, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%s is not inside a git repository", dir)
	}
	if remote == "" {
		remote = "origin"
	}
	url, err := git(top, "remote", "get-url", remote)
	if err != nil {
		return nil, fmt.Errorf("the repository has no %q remote", remote)
	}
	owner, name, https, ok := ParseRemote(url)
	if !ok {
		return nil, fmt.Errorf("remote %q (%s) is not a GitHub repository", remote, url)
	}
	return &Repo{Dir: top, Remote: remote, URL: url, Owner: owner, Name: name, HTTPS: https}, nil
}

// Head returns the full HEAD commit and the current branch ("" when detached).
func (r *Repo) Head() (sha, branch string, err error) {
	if sha, err = git(r.Dir, "rev-parse", "HEAD"); err != nil {
		return "", "", fmt.Errorf("the repository has no commits yet")
	}
	branch, _ = git(r.Dir, "symbolic-ref", "--short", "-q", "HEAD")
	return sha, branch, nil
}

// Dirty lists modified or staged tracked files (untracked files are ignored: they
// are not part of the tagged commit).
func (r *Repo) Dirty() ([]string, error) {
	out, err := git(r.Dir, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// Unpushed counts local commits not on the upstream branch (-1 when there is no upstream).
func (r *Repo) Unpushed() int {
	out, err := git(r.Dir, "rev-list", "--count", "@{upstream}..HEAD")
	if err != nil {
		return -1
	}
	n, _ := strconv.Atoi(out)
	return n
}

// Version is a semantic version vMAJOR.MINOR.PATCH.
type Version struct{ Major, Minor, Patch int }

func (v Version) String() string { return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch) }

var semverRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)

// ParseVersion parses "v1.2.3" or "1.2.3".
func ParseVersion(s string) (Version, bool) {
	m := semverRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Version{}, false
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	c, _ := strconv.Atoi(m[3])
	return Version{a, b, c}, true
}

// Bump returns the next version.
func (v Version) Bump(part string) (Version, error) {
	switch part {
	case "patch", "":
		return Version{v.Major, v.Minor, v.Patch + 1}, nil
	case "minor":
		return Version{v.Major, v.Minor + 1, 0}, nil
	case "major":
		return Version{v.Major + 1, 0, 0}, nil
	}
	return v, fmt.Errorf("unknown bump %q (patch, minor or major)", part)
}

// Less orders versions.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

// LatestTag returns the highest vX.Y.Z tag (local and remote), if any.
func (r *Repo) LatestTag() (Version, bool) {
	tags := r.tags()
	var vs []Version
	for t := range tags {
		if v, ok := ParseVersion(t); ok && strings.HasPrefix(t, "v") {
			vs = append(vs, v)
		}
	}
	if len(vs) == 0 {
		return Version{}, false
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].Less(vs[j]) })
	return vs[len(vs)-1], true
}

// tags returns every tag name known locally or on the remote.
func (r *Repo) tags() map[string]bool {
	out := map[string]bool{}
	if local, err := git(r.Dir, "tag", "--list"); err == nil {
		for _, t := range strings.Fields(local) {
			out[t] = true
		}
	}
	if remote, err := git(r.Dir, "ls-remote", "--tags", "--refs", r.Remote); err == nil {
		for _, l := range strings.Split(remote, "\n") {
			if f := strings.Fields(l); len(f) == 2 {
				out[strings.TrimPrefix(f[1], "refs/tags/")] = true
			}
		}
	}
	return out
}

// TagExists reports whether tag exists locally or on the remote.
func (r *Repo) TagExists(tag string) bool { return r.tags()[tag] }

// Tag creates an annotated tag at HEAD.
func (r *Repo) Tag(tag, message string) error {
	_, err := git(r.Dir, "tag", "-a", tag, "-m", message)
	return err
}

// DeleteLocalTag removes a local tag (used when the push fails).
func (r *Repo) DeleteLocalTag(tag string) { _, _ = git(r.Dir, "tag", "-d", tag) }

// PushTag pushes one tag. For HTTPS GitHub remotes with a token, the token is
// handed to git through the environment (never on the command line) so the push
// uses the repository owner's account even when another one is active.
func (r *Repo) PushTag(tag, token string) error {
	args := []string{"-C", r.Dir}
	var env []string
	if r.HTTPS && token != "" {
		args = append(args, "-c", "credential.helper=",
			"-c", `credential.helper=!f() { echo username=x-access-token; echo "password=$AXON_GIT_TOKEN"; }; f`)
		env = []string{"AXON_GIT_TOKEN=" + token, "GIT_TERMINAL_PROMPT=0"}
	}
	args = append(args, "push", r.Remote, "refs/tags/"+tag)
	_, err := sh.Run(sh.Cmd{Name: "git", Args: args, Env: env, Timeout: 5 * time.Minute})
	return err
}
