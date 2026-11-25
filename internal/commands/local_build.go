package commands

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"axon/internal/descriptor"
	"axon/internal/official"
	"axon/internal/sh"
	"axon/internal/ui"
)

// localBuild is one image-backed member `up --build` builds from the developer's checkout (SPEC.md Appendix C.12).
type localBuild struct {
	Target     string
	Why        string
	Dockerfile string
	Stage      string // docker build --target
	Context    string
	Contexts   map[string]string
	Args       map[string]string
}

// gitRunner answers git questions about a checkout; tests replace it.
type gitRunner interface {
	head(dir string) (string, error)
	dirty(dir, path string) (bool, error)
	// contains reports whether commit holds the pin's work under path: the pin is its ancestor, or every commit the
	// pin has and commit lacks is a merge (main's merge of develop is beside develop, yet holds nothing develop lacks).
	contains(dir, ancestor, commit, path string) (known, ok bool)
	// same reports whether path holds the same content at both commits (known is false when a commit is missing).
	same(dir, a, b, path string) (known, ok bool)
}

type realGit struct{}

func (realGit) head(dir string) (string, error) {
	return sh.Output("git", "-C", dir, "rev-parse", "HEAD")
}
func (realGit) dirty(dir, path string) (bool, error) {
	out, err := sh.Output("git", "-C", dir, "status", "--porcelain", "--", or(path, "."))
	return strings.TrimSpace(out) != "", err
}
func (realGit) same(dir, a, b, path string) (known, ok bool) {
	for _, c := range []string{a, b} {
		if _, err := sh.Output("git", "-C", dir, "cat-file", "-e", c+"^{commit}"); err != nil {
			return false, false
		}
	}
	_, err := sh.Run(sh.Cmd{Name: "git", Args: []string{"-C", dir, "diff", "--quiet", a, b, "--", or(path, ".")}})
	return true, err == nil
}

func (realGit) contains(dir, ancestor, commit, path string) (known, ok bool) {
	if _, err := sh.Output("git", "-C", dir, "cat-file", "-e", ancestor+"^{commit}"); err != nil {
		return false, false
	}
	if _, err := sh.Run(sh.Cmd{Name: "git", Args: []string{"-C", dir, "merge-base", "--is-ancestor", ancestor, commit}}); err == nil {
		return true, true
	}
	// Beside: the pin's own (non-merge) commits under path that commit lacks. None means the pin only merged work
	// that commit already has.
	out, err := sh.Output("git", "-C", dir, "rev-list", "--no-merges", commit+".."+ancestor, "--", or(path, "."))
	return true, err == nil && strings.TrimSpace(out) == ""
}

// planLocalBuilds decides which image-backed members to build. Without --app or --rebuild a member is built when the
// committed content under its source path differs from its pin's and its checkout holds the pin's work (the source
// lock's commit is an ancestor of HEAD, or only merges separate them: main's merge of develop), or when it has uncommitted changes under its source path or a watched
// directory. A checkout whose source path holds the pin's content is at its pin, whatever its history (a develop merge
// of the same change as main's). One whose content differs but is behind or beside its pin is not built (that could
// deploy older code): it is reported; --app builds it anyway. --app <member> builds exactly the named members; --rebuild
// builds every member a recipe covers. Notes explain every member left on its pin.
func planLocalBuilds(recipes []descriptor.LocalBuild, mats []official.Materialization, lock []official.LockMember,
	tokens map[string][]string, workspace, cluster string, apps []string, all bool, git gitRunner) ([]localBuild, []string, error) {
	byTarget := map[string]official.Materialization{}
	for _, m := range mats {
		byTarget[m.Target] = m
	}
	pinned := map[string]string{}
	for _, m := range lock {
		pinned[m.AppID] = m.Commit
	}
	recipeOf := map[string]*descriptor.LocalBuild{}
	var order []string
	for i := range recipes {
		for _, t := range recipes[i].Targets {
			if _, dup := recipeOf[t]; dup {
				return nil, nil, fmt.Errorf("[[local_build]]: member %s is in two recipes", t)
			}
			recipeOf[t] = &recipes[i]
			order = append(order, t)
		}
	}
	want := map[string]bool{}
	for _, a := range apps {
		if recipeOf[a] == nil {
			return nil, nil, fmt.Errorf("--app %s: no [[local_build]] recipe builds that member (members with one: %s)", a, strings.Join(sortedKeys(recipeOf), ", "))
		}
		want[a] = true
	}
	var out []localBuild
	var notes []string
	for _, t := range order {
		if len(want) > 0 && !want[t] {
			continue
		}
		mat, ok := byTarget[t]
		if !ok {
			return nil, nil, fmt.Errorf("[[local_build]]: %s is not a [[materialization]] target of the workflow file", t)
		}
		checkout := mat.LocalCheckoutPath
		if !filepath.IsAbs(checkout) {
			checkout = filepath.Join(workspace, checkout)
		}
		head, err := git.head(checkout)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s: no Git checkout at %s; it keeps its pinned image", t, checkout))
			continue
		}
		vars := map[string]string{"workspace": workspace, "cluster": cluster, "target": t, "checkout": checkout,
			"source_path": mat.SourcePath, "source": filepath.Join(checkout, mat.SourcePath), "commit": head}
		if ts := tokens[t]; len(ts) > 0 {
			vars["token"] = ts[0]
		}
		r := recipeOf[t]
		why := ""
		switch {
		case all:
			why = "--rebuild"
		case want[t]:
			why = "--app"
		default:
			pin := pinned[mat.SourceMember]
			if pin != "" && pin != head {
				if known, same := git.same(checkout, pin, head, mat.SourcePath); known && same {
					pin = head // the source path holds the pin's content: the pinned image is that build
				}
			}
			if pin != "" && pin != head {
				switch known, ok := git.contains(checkout, pin, head, mat.SourcePath); {
				case ok:
					why = fmt.Sprintf("checkout %s is ahead of the pin %s (%s changed)", short7(head), short7(pin), or(mat.SourcePath, "."))
				case known:
					notes = append(notes, fmt.Sprintf("%s: checkout %s differs from the pin %s and is behind or beside it; it keeps its pinned image (--app %s builds it anyway)", t, short7(head), short7(pin), t))
					continue
				default:
					notes = append(notes, fmt.Sprintf("%s: the pin %s is not in the checkout (fetch it); it keeps its pinned image (--app %s builds it anyway)", t, short7(pin), t))
					continue
				}
			}
			if why == "" {
				if d, _ := git.dirty(checkout, mat.SourcePath); d {
					why = "uncommitted changes in " + filepath.Join(mat.LocalCheckoutPath, mat.SourcePath)
				}
			}
			if why == "" {
				for _, w := range r.Watch {
					dir := absUnder(cluster, expand(w, vars))
					if d, _ := git.dirty(dir, "."); d {
						why = "uncommitted changes in " + dir
						break
					}
				}
			}
			if why == "" {
				continue // the checkout is the pin: the pinned image is that build
			}
		}
		b := localBuild{Target: t, Why: why, Dockerfile: absUnder(cluster, expand(r.Dockerfile, vars)),
			Context: absUnder(cluster, expand(or(r.Context, "."), vars)), Contexts: map[string]string{}, Args: map[string]string{}}
		for k, v := range r.Contexts {
			b.Contexts[k] = absUnder(cluster, expand(v, vars))
		}
		for k, v := range r.Args {
			b.Args[k] = expand(v, vars)
		}
		for k, v := range r.TargetArgs[t] {
			b.Args[k] = expand(v, vars)
		}
		b.Stage = expand(r.Target, vars)
		if st, ok := r.TargetStages[t]; ok {
			b.Stage = expand(st, vars)
		}
		out = append(out, b)
	}
	return out, notes, nil
}

// buildLocal builds each member's image with the pipeline's recipe and returns member → image. The image is tagged
// local/<name>:<12 hex of its image ID>, so an unchanged build keeps its tag (nothing rolls) and a changed one rolls.
func buildLocal(builds []localBuild, noCache bool) (map[string]string, error) {
	images := map[string]string{}
	var failed []string
	defer func() {
		if len(failed) > 0 {
			ui.Warn("%d member(s) did not build and run their pinned images: %s", len(failed), strings.Join(failed, ", "))
		}
	}()
	for _, b := range builds {
		repo := "local/" + official.K8sName(b.Target)
		args := []string{"build", "-f", b.Dockerfile, "-t", repo + ":build"}
		if noCache {
			args = append(args, "--no-cache")
		}
		if b.Stage != "" {
			args = append(args, "--target", b.Stage)
		}
		for _, k := range sortedKeys(b.Contexts) {
			args = append(args, "--build-context", k+"="+b.Contexts[k])
		}
		for _, k := range sortedKeys(b.Args) {
			args = append(args, "--build-arg", k+"="+b.Args[k])
		}
		args = append(args, b.Context)
		ui.Step("building %s (%s)", b.Target, b.Why)
		if _, err := sh.Run(sh.Cmd{Name: "docker", Args: args, Env: []string{"DOCKER_BUILDKIT=1"}, Stream: true, Prefix: "  │ ", Timeout: 45 * time.Minute}); err != nil {
			// one member that does not build keeps its pinned image; the others still deploy
			ui.Warn("%s did not build, so it keeps its pinned image: %v", b.Target, err)
			failed = append(failed, b.Target)
			continue
		}
		id, err := sh.Output("docker", "image", "inspect", "--format", "{{.Id}}", repo+":build")
		if err != nil {
			return nil, fmt.Errorf("inspect the image of %s: %w", b.Target, err)
		}
		tag := repo + ":" + strings.TrimPrefix(id, "sha256:")[:12]
		if _, err := sh.Output("docker", "tag", repo+":build", tag); err != nil {
			return nil, fmt.Errorf("tag the image of %s: %w", b.Target, err)
		}
		ui.Sub("%s -> %s", b.Target, tag)
		images[b.Target] = tag
	}
	return images, nil
}

func expand(s string, vars map[string]string) string {
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}

func absUnder(base, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
