// Package build turns member apps into images: module UIs (npm/vite), Go module
// servers (cross-compiled on the host), core contexts (FROM core_image) and project
// workers (their own Dockerfile). Every step is content-addressed: unchanged
// inputs are skipped and images are tagged with their input hash, so deploys roll
// only what changed.
package build

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"axon/internal/descriptor"
	"axon/internal/sh"
	"axon/internal/ui"
)

// Options control a build.
type Options struct {
	Jobs    int
	Rebuild bool     // ignore caches
	Apps    []string // limit to these app names (empty = all)
	// Only limits the build to these targets: "<app>/<token>" builds one module of a
	// cored app (and reassembles that app's core), "<app>" builds a worker app's image.
	// Every other module and app is left as it is. Empty = no limit.
	Only   []string
	DryRun bool
	Arch   string // linux arch for Go servers (default: docker's)
	Load   string // e.g. "kind:<cluster>" to load images into kind

	// Platforms are the image platforms, e.g. ["linux/amd64", "linux/arm64"]. One
	// platform builds a local image for it; more than one builds multi-architecture
	// images with docker buildx, which must push them (Push) — such images cannot be
	// loaded into the local image store.
	Platforms []string
	// Push is a registry prefix (e.g. ghcr.io/owner); with several Platforms every image
	// is pushed as <Push>/<repo>:<tag> and Image.Pushed records its digest reference.
	Push string
}

// MultiArch reports whether the options ask for multi-architecture images.
func (o Options) MultiArch() bool { return len(o.Platforms) > 1 }

// PlatformArch returns the GOARCH of "linux/<arch>[/<variant>]".
func PlatformArch(p string) (string, error) {
	parts := strings.Split(p, "/")
	if len(parts) < 2 || parts[0] != "linux" || parts[1] == "" {
		return "", fmt.Errorf("platform %q: want linux/<arch>, e.g. linux/amd64", p)
	}
	return parts[1], nil
}

// Image is one image the plan produces.
type Image struct {
	App        string
	Service    string // core | <token> | worker
	Repo       string
	Tag        string
	Context    string
	Dockerfile string
	Cached     bool // repo:tag already present locally (or, multi-arch, in the registry)
	Built      bool
	Pushed     string // multi-arch: <registry>/<repo>@sha256:… once in the registry
}

// Ref returns repo:tag.
func (i *Image) Ref() string { return i.Repo + ":" + i.Tag }

// Result of a build.
type Result struct {
	Images  []*Image
	Tags    map[string]string // repo → tag
	Skipped int
	Took    time.Duration
}

type builder struct {
	m      *descriptor.Model
	o      Options
	cache  *Cache
	root   string // <state>/build
	arch   string
	arches []string // multi-arch: GOARCH per platform
	mu     sync.Mutex
	res    *Result
}

// DockerArch returns docker's server architecture as a GOARCH value.
func DockerArch() string {
	out, err := sh.Output("docker", "info", "--format", "{{.Architecture}}")
	if err != nil {
		return runtime.GOARCH
	}
	switch strings.TrimSpace(out) {
	case "aarch64", "arm64":
		return "arm64"
	case "x86_64", "amd64":
		return "amd64"
	}
	return runtime.GOARCH
}

// Run builds every app in the model (or o.Apps).
func Run(m *descriptor.Model, o Options) (*Result, error) {
	start := time.Now()
	if o.Jobs <= 0 {
		o.Jobs = defaultJobs()
	}
	b := &builder{m: m, o: o, cache: OpenCache(m.State), root: filepath.Join(m.State, "build"),
		res: &Result{Tags: map[string]string{}}}
	b.arch = o.Arch
	switch {
	case o.MultiArch():
		if o.Push == "" {
			return nil, fmt.Errorf("multi-architecture images (%s) must be pushed to a registry: buildx cannot load them locally", strings.Join(o.Platforms, ", "))
		}
		for _, p := range o.Platforms {
			a, err := PlatformArch(p)
			if err != nil {
				return nil, err
			}
			b.arches = append(b.arches, a)
		}
	case len(o.Platforms) == 1:
		a, err := PlatformArch(o.Platforms[0])
		if err != nil {
			return nil, err
		}
		b.arch = a
	}
	if b.arch == "" {
		b.arch = DockerArch()
	}
	want := map[string]bool{}
	for _, a := range o.Apps {
		want[a] = true
	}
	onlyMods := map[string]map[string]bool{} // app → module tokens
	onlyWorkers := map[string]bool{}
	for _, t := range o.Only {
		app, tok, isMod := strings.Cut(t, "/")
		want[app] = true
		if !isMod {
			onlyWorkers[app] = true
			continue
		}
		if onlyMods[app] == nil {
			onlyMods[app] = map[string]bool{}
		}
		onlyMods[app][tok] = true
	}
	var apps []*descriptor.AppModel
	for _, a := range m.Apps {
		if len(want) > 0 && !want[a.Decl.Name] {
			continue
		}
		if a.Problem != "" {
			if a.Decl.Cored() || a.Decl.Image == "" {
				return nil, fmt.Errorf("%s: %s", a.Decl.Name, a.Problem)
			}
			continue
		}
		apps = append(apps, a)
	}

	// Phase 1: module UIs and servers (parallel).
	var tasks []func() error
	for _, a := range apps {
		if !a.Decl.Cored() {
			continue
		}
		for _, mod := range a.Modules {
			a, mod := a, mod
			if mod.IsImage() || mod.SrcDir == "" {
				continue
			}
			if len(o.Only) > 0 && !onlyMods[a.Decl.Name][mod.Token] {
				continue
			}
			tasks = append(tasks, func() error { return b.module(a, mod) })
		}
	}
	if err := parallel(o.Jobs, tasks); err != nil {
		return nil, err
	}
	// Phase 2: core contexts + worker images (contexts are cheap; images parallel).
	tasks = nil
	for _, a := range apps {
		a := a
		if a.Decl.Cored() {
			if len(o.Only) > 0 && onlyMods[a.Decl.Name] == nil {
				continue
			}
			if err := b.coreContext(a); err != nil {
				return nil, err
			}
		} else if a.Decl.Image == "" {
			if len(o.Only) > 0 && !onlyWorkers[a.Decl.Name] {
				continue
			}
			if err := b.workerImage(a); err != nil {
				return nil, err
			}
		}
	}
	for _, img := range b.res.Images {
		img := img
		if img.Built || img.Context == "" {
			continue
		}
		if o.MultiArch() {
			// the same inputs built for other platforms are a different image
			img.Tag = shortHash(HashStrings(img.Tag, strings.Join(o.Platforms, ",")))
			tasks = append(tasks, func() error { return b.buildxPush(img) })
			continue
		}
		tasks = append(tasks, func() error { return b.dockerBuild(img) })
	}
	if err := parallel(o.Jobs, tasks); err != nil {
		return nil, err
	}
	for _, img := range b.res.Images {
		b.res.Tags[img.Repo] = img.Tag
		if img.Cached {
			b.res.Skipped++
		}
	}
	if strings.HasPrefix(o.Load, "kind:") && !o.DryRun && !o.MultiArch() {
		for _, img := range b.res.Images {
			if _, err := sh.Run(sh.Cmd{Name: "kind", Args: []string{"load", "docker-image", img.Ref(), "--name", strings.TrimPrefix(o.Load, "kind:")}}); err != nil {
				return nil, err
			}
		}
	}
	b.res.Took = time.Since(start)
	sort.Slice(b.res.Images, func(i, j int) bool { return b.res.Images[i].Repo < b.res.Images[j].Repo })
	return b.res, nil
}

func defaultJobs() int {
	n := runtime.NumCPU() / 2
	if n < 2 {
		n = 2
	}
	if n > 6 {
		n = 6
	}
	return n
}

func parallel(n int, tasks []func() error) error {
	if len(tasks) == 0 {
		return nil
	}
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []string
	for _, t := range tasks {
		t := t
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := t(); err != nil {
				mu.Lock()
				errs = append(errs, err.Error())
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("%d build step(s) failed:\n%s", len(errs), strings.Join(errs, "\n"))
	}
	return nil
}

func (b *builder) addImage(img *Image) {
	b.mu.Lock()
	b.res.Images = append(b.res.Images, img)
	b.mu.Unlock()
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// module builds a source module's UI bundle and server image.
func (b *builder) module(a *descriptor.AppModel, mod *descriptor.Module) error {
	src := mod.SrcDir
	label := a.Decl.Name + "/" + mod.Token
	if exists(filepath.Join(src, "ui", "package.json")) {
		if err := b.ui(label, filepath.Join(src, "ui")); err != nil {
			return fmt.Errorf("%s ui: %w", label, err)
		}
	}
	repo := descriptor.ServiceImage(b.m.C.Name, a.Decl.Name, mod.Token)
	ctx := filepath.Join(b.root, a.Decl.Name, mod.Token)
	svcDir := filepath.Join(src, "service")
	if (mod.Lang == "go" || mod.Lang == "") && exists(filepath.Join(svcDir, "go.mod")) {
		h, err := HashDir(svcDir)
		if err != nil {
			return err
		}
		if len(b.arches) > 0 {
			return b.multiArchServer(a, mod, label, repo, ctx, svcDir, h)
		}
		h = HashStrings(h, b.arch, serverDockerfile)
		key := "server:" + label
		bin := filepath.Join(ctx, "server")
		if b.o.Rebuild || b.cache.Get(key) != h || !exists(bin) {
			if b.o.DryRun {
				ui.Sub("would compile %s (go, linux/%s)", label, b.arch)
			} else {
				ui.Sub("compile %s (go, linux/%s)", label, b.arch)
				if err := os.MkdirAll(ctx, 0o755); err != nil {
					return err
				}
				_, err := sh.Run(sh.Cmd{Name: "go", Args: []string{"build", "-trimpath", "-ldflags=-s -w", "-o", bin, "."},
					Dir: svcDir, Env: []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=" + b.arch}, Timeout: 20 * time.Minute})
				if err != nil {
					return fmt.Errorf("%s server: %w", label, err)
				}
				b.cache.Put(key, h)
			}
		}
		if !b.o.DryRun {
			if _, err := WriteIfChanged(filepath.Join(ctx, "Dockerfile"), serverDockerfile, 0o644); err != nil {
				return err
			}
		}
		b.addImage(&Image{App: a.Decl.Name, Service: mod.Token, Repo: repo, Tag: shortHash(h), Context: ctx, Dockerfile: filepath.Join(ctx, "Dockerfile")})
		return nil
	}
	// any other language: the module's own Dockerfile builds the server
	df := filepath.Join(src, "Dockerfile")
	if !exists(df) {
		return fmt.Errorf("%s: no service/go.mod and no Dockerfile — cannot build the module server", label)
	}
	h, err := HashDir(src, "ui/dist")
	if err != nil {
		return err
	}
	b.addImage(&Image{App: a.Decl.Name, Service: mod.Token, Repo: repo, Tag: shortHash(h), Context: src, Dockerfile: df})
	return nil
}

// multiArchServer cross-compiles a Go module server once per platform; the
// multi-arch Dockerfile picks the binary matching the platform being assembled.
func (b *builder) multiArchServer(a *descriptor.AppModel, mod *descriptor.Module, label, repo, ctx, svcDir, srcHash string) error {
	h := HashStrings(srcHash, strings.Join(b.arches, ","), serverDockerfileMulti)
	for _, arch := range b.arches {
		key := "server:" + label + ":" + arch
		ah := HashStrings(srcHash, arch)
		bin := filepath.Join(ctx, "server-"+arch)
		if !b.o.Rebuild && b.cache.Get(key) == ah && exists(bin) {
			continue
		}
		if b.o.DryRun {
			ui.Sub("would compile %s (go, linux/%s)", label, arch)
			continue
		}
		ui.Sub("compile %s (go, linux/%s)", label, arch)
		if err := os.MkdirAll(ctx, 0o755); err != nil {
			return err
		}
		if _, err := sh.Run(sh.Cmd{Name: "go", Args: []string{"build", "-trimpath", "-ldflags=-s -w", "-o", bin, "."},
			Dir: svcDir, Env: []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=" + arch}, Timeout: 20 * time.Minute}); err != nil {
			return fmt.Errorf("%s server (linux/%s): %w", label, arch, err)
		}
		b.cache.Put(key, ah)
	}
	df := filepath.Join(ctx, "Dockerfile.multiarch")
	if !b.o.DryRun {
		if _, err := WriteIfChanged(df, serverDockerfileMulti, 0o644); err != nil {
			return err
		}
	}
	b.addImage(&Image{App: a.Decl.Name, Service: mod.Token, Repo: repo, Tag: shortHash(h), Context: ctx, Dockerfile: df})
	return nil
}

const serverDockerfileMulti = `# GENERATED by axon — multi-architecture runtime image for a module server
# cross-compiled on the host once per platform (server-<arch>).
FROM alpine:3
ARG TARGETARCH
RUN apk upgrade --no-cache && adduser -D -u 65532 app
WORKDIR /app
COPY server-${TARGETARCH} /app/server
ENV MYCEL_LISTEN=0.0.0.0:7373
EXPOSE 7373
USER 65532
CMD ["/app/server"]
`

const serverDockerfile = `# GENERATED by axon — thin runtime image for a module server compiled on the host.
FROM alpine:3
RUN apk upgrade --no-cache && adduser -D -u 65532 app
WORKDIR /app
COPY server /app/server
ENV MYCEL_LISTEN=0.0.0.0:7373
EXPOSE 7373
USER 65532
CMD ["/app/server"]
`

// ui builds a module UI bundle with npm (skipped when inputs are unchanged).
func (b *builder) ui(label, dir string) error {
	h, err := HashDir(dir, "dist")
	if err != nil {
		return err
	}
	key := "ui:" + label
	if !b.o.Rebuild && b.cache.Get(key) == h && exists(filepath.Join(dir, "dist")) {
		return nil
	}
	// First run: adopt an existing bundle that is newer than every input.
	if !b.o.Rebuild && b.cache.Get(key) == "" && distFresh(dir) {
		if !b.o.DryRun {
			b.cache.Put(key, h)
		}
		return nil
	}
	if b.o.DryRun {
		ui.Sub("would build ui %s", label)
		return nil
	}
	ui.Sub("build ui %s", label)
	lockHash := ""
	if bts, err := os.ReadFile(filepath.Join(dir, "package-lock.json")); err == nil {
		lockHash = HashStrings(string(bts))
	}
	if !exists(filepath.Join(dir, "node_modules")) || b.cache.Get("npm:"+label) != lockHash {
		args := []string{"ci", "--no-audit", "--no-fund", "--loglevel=error"}
		if lockHash == "" {
			args = []string{"install", "--no-audit", "--no-fund", "--loglevel=error"}
		}
		if _, err := sh.Run(sh.Cmd{Name: "npm", Args: args, Dir: dir, Timeout: 20 * time.Minute}); err != nil {
			return err
		}
		b.cache.Put("npm:"+label, lockHash)
	}
	if _, err := sh.Run(sh.Cmd{Name: "npm", Args: []string{"run", "build"}, Dir: dir, Timeout: 20 * time.Minute}); err != nil {
		return err
	}
	h2, _ := HashDir(dir, "dist")
	b.cache.Put(key, h2)
	return nil
}

// coreContext assembles the core build context: Dockerfile FROM core_image, the
// app descriptor, and each module's descriptor + UI bundle.
func (b *builder) coreContext(a *descriptor.AppModel) error {
	core := b.m.SubstImage(b.m.C.CoreImage)
	if core == "" {
		return fmt.Errorf("%s: core_image is not set in the cluster descriptor; axon assembles every core FROM it", a.Decl.Name)
	}
	ctx := filepath.Join(b.root, a.Decl.Name, "core")
	if !b.o.DryRun {
		if err := os.RemoveAll(filepath.Join(ctx, "modules")); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(ctx, "modules"), 0o755); err != nil {
			return err
		}
		if _, err := WriteIfChanged(filepath.Join(ctx, "modules", ".keep"), "", 0o644); err != nil {
			return err
		}
	}
	var copies []string
	for _, mod := range a.Modules {
		dst := filepath.Join(ctx, "modules", mod.Token, "module")
		if mod.IsImage() {
			copies = append(copies,
				fmt.Sprintf("COPY --from=%s /mycelfile /app/modules/%s/module/mycelfile", mod.Image, mod.Token),
				fmt.Sprintf("COPY --from=%s /ui /app/modules/%s/module/ui/dist", mod.Image, mod.Token))
			continue
		}
		if b.o.DryRun {
			continue
		}
		if f := moduleDescriptor(mod.SrcDir); f != "" {
			if err := CopyFile(f, filepath.Join(dst, "mycelfile"), 0o644); err != nil {
				return err
			}
		}
		if dist := filepath.Join(mod.SrcDir, "ui", "dist"); exists(dist) {
			if err := CopyDir(dist, filepath.Join(dst, "ui", "dist")); err != nil {
				return err
			}
		}
	}
	df := coreDockerfile(core, copies)
	repo := descriptor.ServiceImage(b.m.C.Name, a.Decl.Name, "core")
	if b.o.DryRun {
		ui.Sub("would assemble core image %s (FROM %s + %d module(s))", repo, core, len(a.Modules))
		b.addImage(&Image{App: a.Decl.Name, Service: "core", Repo: repo, Tag: "dry-run", Built: true})
		return nil
	}
	if _, err := WriteIfChanged(filepath.Join(ctx, "Dockerfile"), df, 0o644); err != nil {
		return err
	}
	if a.Found != nil {
		if err := CopyFile(a.Found.Path, filepath.Join(ctx, "mycelfile"), 0o644); err != nil {
			return err
		}
	}
	h, err := HashDir(ctx)
	if err != nil {
		return err
	}
	b.addImage(&Image{App: a.Decl.Name, Service: "core", Repo: repo, Tag: shortHash(h), Context: ctx, Dockerfile: filepath.Join(ctx, "Dockerfile")})
	return nil
}

func moduleDescriptor(dir string) string {
	for _, n := range []string{"axonfile", "mycelfile"} {
		if p := filepath.Join(dir, n); exists(p) {
			return p
		}
	}
	return ""
}

func coreDockerfile(core string, copies []string) string {
	var b strings.Builder
	b.WriteString("# GENERATED by axon — the app core, assembled FROM the released core image.\n")
	b.WriteString("FROM alpine:3\nRUN apk upgrade --no-cache\nWORKDIR /app\n")
	fmt.Fprintf(&b, "COPY --from=%s /app_core /app/app_core\n", core)
	fmt.Fprintf(&b, "COPY --from=%s /shell /app/static\n", core)
	b.WriteString("COPY modules/ /app/modules/\n")
	for _, c := range copies {
		b.WriteString(c + "\n")
	}
	b.WriteString("COPY mycelfile /app/mycelfile\n")
	b.WriteString("ENV APP_STATIC_DIR=/app/static\nENV APP_MYCELFILE=/app/mycelfile\nENV MYCEL_RUNTIME=container\nENV PORT=9080\n")
	b.WriteString("EXPOSE 9080\nCMD [\"/app/app_core\"]\n")
	return b.String()
}

// workerImage plans a project worker image from its own Dockerfile.
func (b *builder) workerImage(a *descriptor.AppModel) error {
	if a.File == nil {
		return fmt.Errorf("%s: no descriptor", a.Decl.Name)
	}
	df := ""
	if p := a.File.Project; p != nil && p.Dockerfile != "" {
		df = filepath.Join(a.Dir, p.Dockerfile)
	} else {
		for _, cand := range []string{"Dockerfile", ".axon.infra/Dockerfile", ".mycel.infra/Dockerfile"} {
			if exists(filepath.Join(a.Dir, cand)) {
				df = filepath.Join(a.Dir, cand)
				break
			}
		}
	}
	if df == "" || !exists(df) {
		// no Dockerfile: run the declared build steps; no image to deploy
		if a.File.Project != nil {
			for _, st := range a.File.Project.Build {
				if b.o.DryRun {
					ui.Sub("would run %s build step %q", a.Decl.Name, st.Name)
					continue
				}
				ui.Sub("%s: %s", a.Decl.Name, st.Name)
				if _, err := sh.Run(sh.Cmd{Name: "sh", Args: []string{"-c", st.Command}, Dir: a.Dir}); err != nil {
					return fmt.Errorf("%s build step %q: %w", a.Decl.Name, st.Name, err)
				}
			}
		}
		return fmt.Errorf("%s: project app has no Dockerfile (set [project].dockerfile) and no image", a.Decl.Name)
	}
	h, err := HashDir(a.Dir)
	if err != nil {
		return err
	}
	b.addImage(&Image{App: a.Decl.Name, Service: "worker", Repo: descriptor.WorkerImage(b.m.C.Name, a.Decl.Name),
		Tag: shortHash(h), Context: a.Dir, Dockerfile: df})
	return nil
}

func (b *builder) dockerBuild(img *Image) error {
	if !b.o.Rebuild {
		if _, err := sh.Run(sh.Cmd{Name: "docker", Args: []string{"image", "inspect", "--format", "{{.Id}}", img.Ref()}}); err == nil {
			img.Cached = true
			// keep :latest pointing at the deployed build
			if !b.o.DryRun {
				sh.Run(sh.Cmd{Name: "docker", Args: []string{"tag", img.Ref(), img.Repo + ":latest"}})
			}
			return nil
		}
	}
	if b.o.DryRun {
		ui.Sub("would build image %s", img.Ref())
		return nil
	}
	ui.Sub("build image %s", img.Ref())
	start := time.Now()
	args := []string{"build", "-q", "-f", img.Dockerfile, "-t", img.Ref(), "-t", img.Repo + ":latest", "--label", "axon.dev/app=" + img.App}
	if len(b.o.Platforms) == 1 {
		args = append(args, "--platform", b.o.Platforms[0])
	}
	_, err := sh.Run(sh.Cmd{Name: "docker", Args: append(args, img.Context),
		Env: []string{"DOCKER_BUILDKIT=1"}, Timeout: 45 * time.Minute})
	if err != nil {
		return fmt.Errorf("image %s: %w", img.Ref(), err)
	}
	img.Built = true
	ui.Debug("built %s in %s", img.Ref(), time.Since(start).Round(time.Second))
	return nil
}

// RemoteDigest returns the manifest digest of ref in its registry ("" if absent).
func RemoteDigest(ref string) string {
	out, err := sh.Output("docker", "buildx", "imagetools", "inspect", "--format", "{{json .Manifest}}", ref)
	if err != nil {
		return ""
	}
	var mf struct {
		Digest string `json:"digest"`
	}
	if json.Unmarshal([]byte(out), &mf) != nil || !strings.HasPrefix(mf.Digest, "sha256:") {
		return ""
	}
	return mf.Digest
}

// buildxPush builds a multi-architecture image and pushes it, unless the registry
// already has that tag (tags are input hashes, so an existing tag is the same build).
func (b *builder) buildxPush(img *Image) error {
	reg := strings.TrimSuffix(b.o.Push, "/")
	target := reg + "/" + img.Repo + ":" + img.Tag
	if !b.o.Rebuild {
		if d := RemoteDigest(target); d != "" {
			img.Cached = true
			img.Pushed = reg + "/" + img.Repo + "@" + d
			return nil
		}
	}
	if b.o.DryRun {
		ui.Sub("would build and push %s (%s)", target, strings.Join(b.o.Platforms, ", "))
		return nil
	}
	ui.Sub("build and push %s (%s)", target, strings.Join(b.o.Platforms, ", "))
	start := time.Now()
	meta := filepath.Join(b.root, ".buildx", strings.NewReplacer("/", "_", ":", "_").Replace(img.Repo+":"+img.Tag)+".json")
	if err := os.MkdirAll(filepath.Dir(meta), 0o755); err != nil {
		return err
	}
	_, err := sh.Run(sh.Cmd{Name: "docker", Args: []string{"buildx", "build", "--platform", strings.Join(b.o.Platforms, ","),
		"-f", img.Dockerfile, "-t", target, "--label", "axon.dev/app=" + img.App, "--push", "--metadata-file", meta, img.Context},
		Timeout: 90 * time.Minute})
	if err != nil {
		return fmt.Errorf("image %s: %w\n(multi-platform builds need a buildx builder that supports them — `docker buildx create --use`, "+
			"or Docker's containerd image store — and QEMU for foreign architectures)", target, err)
	}
	digest := ""
	if bts, err := os.ReadFile(meta); err == nil {
		var md map[string]any
		if json.Unmarshal(bts, &md) == nil {
			digest, _ = md["containerimage.digest"].(string)
		}
	}
	if digest == "" {
		digest = RemoteDigest(target)
	}
	if digest == "" {
		return fmt.Errorf("image %s was pushed but its digest could not be read", target)
	}
	img.Built = true
	img.Pushed = reg + "/" + img.Repo + "@" + digest
	ui.Debug("built and pushed %s in %s", img.Pushed, time.Since(start).Round(time.Second))
	return nil
}

// distFresh reports whether ui/dist exists and is newer than every source file.
func distFresh(dir string) bool {
	newestSrc, oldestDist := time.Time{}, time.Time{}
	distSeen := false
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if info.IsDir() {
			if info.Name() == "node_modules" || info.Name() == ".svelte-kit" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(rel, "dist"+string(filepath.Separator)) {
			distSeen = true
			if oldestDist.IsZero() || info.ModTime().Before(oldestDist) {
				oldestDist = info.ModTime()
			}
		} else if info.ModTime().After(newestSrc) {
			newestSrc = info.ModTime()
		}
		return nil
	})
	return distSeen && !oldestDist.Before(newestSrc)
}
