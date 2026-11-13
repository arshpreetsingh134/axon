package deploy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"axon/internal/descriptor"
	"axon/internal/sh"
	"axon/internal/ui"
)

// Down tears the environment down. Data volumes are kept unless volumes is set.
func (s *Session) Down(volumes bool) (err error) {
	if !s.DryRun {
		lock, err := AcquireLock(s.M.State, s.O.Verb, s.Target, s.Env.Name, s.O.Force)
		if err != nil {
			return err
		}
		defer func() {
			outcome := "ok"
			if err != nil {
				outcome = "failed"
			}
			lock.Release(outcome)
		}()
	}
	ui.Step("teardown plan for %s/%s (%s)", s.M.C.Name, s.Env.Name, s.Target)
	if s.Target == "docker" {
		ui.Out("· remove every container of compose project %s and its %d app project(s)", s.fabricProject(), len(s.M.Apps))
		ui.Out("· remove network %s", s.M.C.NetworkOrDefault())
	} else {
		ui.Out("· delete axon-managed workloads, services and config in namespace %s", s.Namespace)
	}
	if volumes {
		ui.Out("· DELETE the data volumes (Postgres, ClickHouse, Grafana, Tempo)")
	} else {
		ui.Out("· keep the shared Postgres data volume")
	}
	if volumes && !s.DryRun && !s.O.Yes {
		return fmt.Errorf("--volumes deletes data: confirm with --yes (or answer the prompt in a terminal)")
	}
	if err := s.runHooks("pre_down"); err != nil {
		return err
	}
	if s.Target == "docker" {
		err = s.downDocker(volumes)
	} else {
		if err = s.k8sPreflight(); err == nil {
			err = s.downK8s(volumes)
		}
	}
	if err != nil {
		return err
	}
	if err := s.runHooks("post_down"); err != nil {
		return err
	}
	if !s.DryRun {
		ui.OK("%s/%s is down", s.M.C.Name, s.Env.Name)
	}
	return nil
}

// Workload is one running unit in a status report.
type Workload struct {
	App     string `json:"app"`
	Name    string `json:"name"`
	State   string `json:"state"`
	Ready   string `json:"ready"`
	Image   string `json:"image,omitempty"`
	Healthy bool   `json:"healthy"`
}

// Status is the report of `env status`.
type Status struct {
	Cluster    string      `json:"cluster"`
	Env        string      `json:"env"`
	Target     string      `json:"target"`
	Namespace  string      `json:"namespace,omitempty"`
	Verdict    string      `json:"verdict"`
	Workloads  []Workload  `json:"workloads"`
	FrontDoors []FrontDoor `json:"frontDoors"`
	LastRun    *RunState   `json:"lastRun,omitempty"`
}

// Status collects workload state and probes the front doors.
func (s *Session) Status() (*Status, error) {
	st := &Status{Cluster: s.M.C.Name, Env: s.Env.Name, Target: s.Target}
	if rs, err := ReadRunState(s.M.State); err == nil {
		st.LastRun = rs
	}
	var err error
	if s.Target == "docker" {
		st.Workloads, err = s.dockerWorkloads()
	} else {
		st.Namespace = s.Namespace
		st.Workloads, err = s.k8sWorkloads()
	}
	if err != nil {
		return nil, err
	}
	st.FrontDoors = s.FrontDoors()
	bad, down := 0, 0
	for _, w := range st.Workloads {
		if !w.Healthy {
			bad++
		}
	}
	for _, f := range st.FrontDoors {
		if !f.OK {
			down++
		}
	}
	switch {
	case len(st.Workloads) == 0:
		st.Verdict = "not deployed"
	case bad == 0 && down == 0:
		st.Verdict = "healthy"
	default:
		st.Verdict = fmt.Sprintf("degraded (%d unhealthy workload(s), %d front door(s) down)", bad, down)
	}
	return st, nil
}

func (s *Session) projects() map[string]string {
	out := map[string]string{s.fabricProject(): "fabric"}
	for _, a := range s.M.C.Apps {
		out[descriptor.NormProject(descriptor.ComposeProject(s.M.C.Name, a.Name))] = a.Name
	}
	return out
}

func (s *Session) dockerWorkloads() ([]Workload, error) {
	r, err := sh.Run(sh.Cmd{Name: "docker", Args: []string{"ps", "-a", "--filter", "label=com.docker.compose.project",
		"--format", `{{.Label "com.docker.compose.project"}}	{{.Label "com.docker.compose.service"}}	{{.State}}	{{.Status}}	{{.Image}}`}})
	if err != nil {
		return nil, err
	}
	projects := s.projects()
	var out []Workload
	for _, l := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
		f := strings.Split(l, "\t")
		if len(f) < 5 {
			continue
		}
		app, ok := projects[f[0]]
		if !ok {
			continue
		}
		healthy := f[2] == "running" && !strings.Contains(f[3], "unhealthy") && !strings.Contains(f[3], "starting")
		out = append(out, Workload{App: app, Name: f[1], State: f[2], Ready: f[3], Image: f[4], Healthy: healthy})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].App+out[i].Name < out[j].App+out[j].Name })
	return out, nil
}

func (s *Session) k8sWorkloads() ([]Workload, error) {
	r, err := s.kubectl("", "get", "deploy,statefulset", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				Replicas int `json:"replicas"`
				Template struct {
					Metadata struct {
						Annotations map[string]string `json:"annotations"`
					} `json:"metadata"`
					Spec struct {
						Containers []struct {
							Image string `json:"image"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
			Status struct {
				ReadyReplicas int `json:"readyReplicas"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &list); err != nil {
		return nil, err
	}
	var out []Workload
	for _, it := range list.Items {
		app := it.Spec.Template.Metadata.Annotations["axon.dev/app"]
		if app == "" {
			app = it.Metadata.Labels["axon.dev/component"]
		}
		img := ""
		if len(it.Spec.Template.Spec.Containers) > 0 {
			img = it.Spec.Template.Spec.Containers[0].Image
		}
		ready := fmt.Sprintf("%d/%d", it.Status.ReadyReplicas, it.Spec.Replicas)
		healthy := it.Status.ReadyReplicas >= it.Spec.Replicas
		state := "running"
		if !healthy {
			state = "not ready"
		}
		out = append(out, Workload{App: app, Name: strings.ToLower(it.Kind) + "/" + it.Metadata.Name, State: state, Ready: ready, Image: img, Healthy: healthy})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// PrintStatus renders a status report.
func PrintStatus(st *Status) {
	where := st.Target
	if st.Namespace != "" {
		where += " · namespace " + st.Namespace
	}
	ui.Out("%s/%s — %s — %s", st.Cluster, st.Env, where, st.Verdict)
	if st.LastRun != nil {
		ui.Out("last run: %s %s at %s (%s)", st.LastRun.Verb, or(st.LastRun.Outcome, "in progress"), st.LastRun.StartedAt, or(st.LastRun.Took, "-"))
	}
	if len(st.Workloads) > 0 {
		rows := [][]string{{"APP", "WORKLOAD", "STATE", "READY"}}
		for _, w := range st.Workloads {
			state := w.State
			if !w.Healthy {
				state = ui.Red(state)
			}
			rows = append(rows, []string{w.App, w.Name, state, w.Ready})
		}
		ui.Out("")
		ui.Table(ui.Stdout, rows, true)
	}
	if len(st.FrontDoors) > 0 {
		ui.Out("")
		rows := [][]string{{"FRONT DOOR", "URL", "HTTP"}}
		for _, f := range st.FrontDoors {
			code := "unreachable"
			if f.Status > 0 {
				code = fmt.Sprint(f.Status)
			}
			if !f.OK {
				code = ui.Red(code)
			}
			rows = append(rows, []string{f.Name, f.URL, code})
		}
		ui.Table(ui.Stdout, rows, true)
	}
}

// Logs streams logs of an app (core by default, or one service/module).
func (s *Session) Logs(app, svc string, follow bool, tail string) error {
	if tail == "" {
		tail = "100"
	}
	if s.Target == "docker" {
		proj := s.fabricProject()
		if app != "fabric" {
			if s.M.C.App(app) == nil {
				return fmt.Errorf("no app %q in %s", app, s.M.C.Name)
			}
			proj = descriptor.NormProject(descriptor.ComposeProject(s.M.C.Name, app))
		}
		args := []string{"compose", "-p", proj, "logs", "--tail", tail}
		if follow {
			args = append(args, "-f")
		}
		if svc != "" {
			args = append(args, svc)
		}
		return sh.Interactive(s.M.Dir, s.Environ(), "docker", args...)
	}
	target := ""
	if a := s.M.C.App(app); a != nil {
		target = "deployment/" + descriptor.CoreName(app)
		if svc != "" && svc != "core" {
			target = "deployment/" + descriptor.DNS(svc)
		}
	} else {
		target = "deployment/" + app
	}
	args := s.kargs("logs", target, "--tail="+tail, "--all-containers", "--prefix")
	if follow {
		args = append(args, "-f")
	}
	return sh.Interactive(s.M.Dir, nil, "kubectl", args...)
}

// Restart restarts an app's workloads.
func (s *Session) Restart(app string) error {
	a := s.M.AppByName(app)
	if a == nil {
		return fmt.Errorf("no app %q in %s", app, s.M.C.Name)
	}
	if s.Target == "docker" {
		proj := descriptor.NormProject(descriptor.ComposeProject(s.M.C.Name, app))
		if _, err := sh.Run(sh.Cmd{Name: "docker", Args: []string{"compose", "-p", proj, "restart"}, Env: s.Environ(), Timeout: 5 * time.Minute}); err != nil {
			return err
		}
		ui.OK("restarted %s", app)
		AppendSpool(s.M.State, map[string]any{"at": time.Now().UTC().Format(time.RFC3339), "verb": "restart", "env": s.Env.Name, "app": app, "outcome": "ok"})
		return nil
	}
	names := []string{"deployment/" + descriptor.CoreName(app)}
	for _, m := range a.Modules {
		names = append(names, "deployment/"+descriptor.DNS(m.Token))
	}
	for _, n := range names {
		if _, err := s.kubectl("", "rollout", "restart", n); err != nil {
			return err
		}
	}
	var deps []string
	for _, n := range names {
		deps = append(deps, strings.TrimPrefix(n, "deployment/"))
	}
	if err := s.waitRollouts(deps, nil, false); err != nil {
		return err
	}
	AppendSpool(s.M.State, map[string]any{"at": time.Now().UTC().Format(time.RFC3339), "verb": "restart", "env": s.Env.Name, "app": app, "outcome": "ok"})
	ui.OK("restarted %s", app)
	return nil
}
