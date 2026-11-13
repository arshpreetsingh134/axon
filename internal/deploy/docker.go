package deploy

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"axon/internal/descriptor"
	"axon/internal/gen"
	"axon/internal/sh"
	"axon/internal/ui"
)

func (s *Session) fabricProject() string { return descriptor.NormProject(s.M.C.Name) }

func (s *Session) fabricCompose() string {
	return filepath.Join(s.M.Infra, "fabric", "docker-compose.yml")
}

func (s *Session) appCompose(app string) string {
	return filepath.Join(s.M.Infra, gen.AppComposeFile(s.M, app))
}

// compose runs `docker compose` with secrets in the process environment only.
func (s *Session) compose(project, file string, stream bool, args ...string) (*sh.Result, error) {
	full := append([]string{"compose", "-p", project, "-f", file}, args...)
	return sh.Run(sh.Cmd{Name: "docker", Args: full, Env: s.Environ(), Stream: stream, Prefix: "  │ ", Timeout: s.timeout() + time.Minute})
}

func (s *Session) timeout() time.Duration {
	if s.O.Timeout > 0 {
		return s.O.Timeout
	}
	return 10 * time.Minute
}

func (s *Session) composeRunning(project string) bool {
	out, err := sh.Output("docker", "ps", "-q", "--filter", "label=com.docker.compose.project="+project)
	return err == nil && strings.TrimSpace(out) != ""
}

func (s *Session) dockerPreflight() error {
	if !sh.Has("docker") {
		return fmt.Errorf("docker is not installed (TOOLCHAIN_DOCKER_MISSING)")
	}
	if _, err := sh.Run(sh.Cmd{Name: "docker", Args: []string{"info", "--format", "{{.ServerVersion}}"}, Timeout: 20 * time.Second}); err != nil {
		return fmt.Errorf("the docker daemon is not reachable (TOOLCHAIN_DOCKER_DAEMON_DOWN): start Docker/OrbStack and retry")
	}
	if s.composeRunning(s.fabricProject()) {
		return nil
	}
	// Host-port preflight: fail before starting anything if a published port is taken.
	var busy []string
	for name, p := range s.publishedPorts() {
		if p > 0 && !portFree(p) {
			busy = append(busy, fmt.Sprintf("%s :%d", name, p))
		}
	}
	if len(busy) > 0 {
		return fmt.Errorf("host port(s) already in use: %s — stop whatever holds them or change the ports in the descriptor", strings.Join(busy, ", "))
	}
	return nil
}

func (s *Session) publishedPorts() map[string]int {
	f := s.M.C.Fabric
	out := map[string]int{"traefik": s.M.C.HTTPPort()}
	if f.Postgres != nil {
		out["postgres"] = f.Postgres.Port
	}
	if f.Temporal != nil {
		out["temporal"], out["temporal-ui"] = f.Temporal.Port, f.Temporal.UIPort
	}
	if f.Tracing != nil {
		out["grafana"] = f.Tracing.GrafanaPort
	}
	if f.Clickhouse != nil {
		out["clickhouse"] = f.Clickhouse.HTTPPort
	}
	return out
}

func portFree(p int) bool {
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

func (s *Session) ensureNetwork() error {
	net := s.M.C.NetworkOrDefault()
	if _, err := sh.Run(sh.Cmd{Name: "docker", Args: []string{"network", "inspect", net}}); err == nil {
		return nil
	}
	if s.DryRun {
		ui.Sub("would create docker network %s", net)
		return nil
	}
	_, err := sh.Run(sh.Cmd{Name: "docker", Args: []string{"network", "create", "--label", "axon.dev/cluster=" + s.M.C.Name, net}})
	if err == nil {
		ui.Sub("created docker network %s", net)
	}
	return err
}

func (s *Session) upDocker() error {
	if err := s.ensureNetwork(); err != nil {
		return err
	}
	if s.DryRun {
		changes, err := s.Generate(true)
		if err != nil {
			return err
		}
		n := 0
		for _, c := range changes {
			if c.Changed {
				n++
				ui.Sub("would write %s", c.Path)
			}
		}
		ui.Sub("would start the fabric (%s) and %d app stack(s)", s.fabricProject(), len(s.M.Apps))
		return nil
	}
	if _, err := s.Generate(false); err != nil {
		return err
	}
	ui.Step("fabric up (%s)", s.fabricProject())
	// Postgres and the databases first (an existing volume skips the init SQL).
	if _, err := s.compose(s.fabricProject(), s.fabricCompose(), false, "up", "-d", "--wait",
		"--wait-timeout", strconv.Itoa(int(s.timeout().Seconds())), "postgres"); err != nil {
		return s.composeFailure(s.fabricProject(), s.fabricCompose(), err)
	}
	if err := s.ensureDatabases(); err != nil {
		return err
	}
	if _, err := s.compose(s.fabricProject(), s.fabricCompose(), false, "up", "-d", "--remove-orphans", "--wait",
		"--wait-timeout", strconv.Itoa(int(s.timeout().Seconds()))); err != nil {
		return s.composeFailure(s.fabricProject(), s.fabricCompose(), err)
	}
	ui.OK("fabric healthy")
	if err := s.provisionKeycloak(); err != nil {
		return err
	}
	if err := s.runHooks("post_fabric"); err != nil {
		return err
	}
	ui.Step("apps up (%d)", len(s.M.Apps))
	var mu sync.Mutex
	var failed []string
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(2, s.O.Jobs))
	for _, a := range s.deployableApps() {
		a := a
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			proj := descriptor.NormProject(descriptor.ComposeProject(s.M.C.Name, a.Decl.Name))
			if _, err := s.compose(proj, s.appCompose(a.Decl.Name), false, "up", "-d", "--remove-orphans", "--wait",
				"--wait-timeout", strconv.Itoa(int(s.timeout().Seconds()))); err != nil {
				mu.Lock()
				failed = append(failed, a.Decl.Name)
				mu.Unlock()
				ui.Fail("%s: %v", a.Decl.Name, s.composeFailure(proj, s.appCompose(a.Decl.Name), err))
				return
			}
			ui.OK("%s", a.Decl.Name)
		}()
	}
	wg.Wait()
	if len(failed) > 0 {
		return fmt.Errorf("%d app(s) did not become healthy: %s (inspect with `axon env logs %s <app>`)", len(failed), strings.Join(failed, ", "), s.Env.Name)
	}
	return s.seedIAM()
}

func (s *Session) deployableApps() []*descriptor.AppModel {
	want := map[string]bool{}
	for _, a := range s.O.Apps {
		want[a] = true
	}
	var out []*descriptor.AppModel
	for _, a := range s.M.Apps {
		if a.Problem != "" && (a.Decl.Cored() || a.Decl.Image == "") {
			continue
		}
		if len(want) > 0 && !want[a.Decl.Name] {
			continue
		}
		out = append(out, a)
	}
	return out
}

// composeFailure enriches a compose error with the unhealthy services' last logs.
func (s *Session) composeFailure(project, file string, err error) error {
	r, _ := s.compose(project, file, false, "ps", "-a", "--format", "{{.Service}}\t{{.State}}\t{{.Health}}")
	var bad []string
	if r != nil {
		for _, l := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
			f := strings.Split(l, "\t")
			if len(f) >= 2 && (f[1] != "running" || (len(f) > 2 && f[2] == "unhealthy")) {
				bad = append(bad, f[0])
			}
		}
	}
	msg := err.Error()
	for _, svc := range bad {
		if lr, _ := s.compose(project, file, false, "logs", "--no-color", "--tail", "15", svc); lr != nil {
			msg += fmt.Sprintf("\n  last logs of %s:\n%s", svc, indentLines(lr.Stdout+lr.Stderr, "    "))
		}
	}
	return fmt.Errorf("%s", msg)
}

func indentLines(s, p string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = p + lines[i]
	}
	return strings.Join(lines, "\n")
}

// moduleHealth reads /api/system through the front door and counts SERVING modules.
func moduleHealth(port int, host string) (serving, total int) {
	client := &http.Client{Timeout: 6 * time.Second}
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/api/system", port), nil)
	req.Host = host
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0
	}
	defer resp.Body.Close()
	var rep struct {
		Modules []struct {
			Health string `json:"health"`
		} `json:"modules"`
	}
	if json.NewDecoder(resp.Body).Decode(&rep) != nil {
		return 0, 0
	}
	for _, m := range rep.Modules {
		total++
		if m.Health == "SERVING" {
			serving++
		}
	}
	return
}

// probe sends GET path to the front door on 127.0.0.1:<port> with a Host header.
func probe(port int, host, path string) int {
	client := &http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	req.Host = host
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (s *Session) downDocker(volumes bool) error {
	apps := s.M.Apps
	for i := len(apps) - 1; i >= 0; i-- {
		a := apps[i]
		proj := descriptor.NormProject(descriptor.ComposeProject(s.M.C.Name, a.Decl.Name))
		args := []string{"compose", "-p", proj, "down", "--remove-orphans"}
		if s.DryRun {
			ui.Sub("would remove compose project %s", proj)
			continue
		}
		if _, err := sh.Run(sh.Cmd{Name: "docker", Args: args, Env: s.Environ()}); err != nil {
			ui.Warn("%s: %v", proj, err)
		} else {
			ui.Sub("removed %s", proj)
		}
	}
	args := []string{"compose", "-p", s.fabricProject(), "down", "--remove-orphans"}
	if volumes {
		args = append(args, "--volumes")
	}
	if s.DryRun {
		ui.Sub("would remove fabric project %s%s", s.fabricProject(), map[bool]string{true: " and its volumes", false: " (volumes kept)"}[volumes])
		return nil
	}
	if _, err := sh.Run(sh.Cmd{Name: "docker", Args: args, Env: s.Environ()}); err != nil {
		return err
	}
	ui.Sub("removed fabric %s", s.fabricProject())
	if _, err := sh.Run(sh.Cmd{Name: "docker", Args: []string{"network", "rm", s.M.C.NetworkOrDefault()}}); err == nil {
		ui.Sub("removed network %s", s.M.C.NetworkOrDefault())
	}
	return nil
}
