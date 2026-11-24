package official

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DispatchWorkflow is a workflow that is started by hand (workflow_dispatch).
type DispatchWorkflow struct {
	File       string   // file name under .github/workflows
	Name       string   // its name:
	Inputs     []string // input names, sorted
	EnvOptions []string // the choices of an "environment" input, if any
}

// Has reports whether the workflow takes every named input.
func (w DispatchWorkflow) Has(inputs ...string) bool {
	for _, in := range inputs {
		i := sort.SearchStrings(w.Inputs, in)
		if i >= len(w.Inputs) || w.Inputs[i] != in {
			return false
		}
	}
	return true
}

// DispatchWorkflows lists the repository's hand-started workflows.
func (l *Layout) DispatchWorkflows() ([]DispatchWorkflow, error) {
	dir := filepath.Join(l.Root, ".github", "workflows")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []DispatchWorkflow
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !(strings.HasSuffix(n, ".yml") || strings.HasSuffix(n, ".yaml")) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		var doc struct {
			Name string `yaml:"name"`
			On   struct {
				Dispatch *struct {
					Inputs map[string]struct {
						Options []string `yaml:"options"`
					} `yaml:"inputs"`
				} `yaml:"workflow_dispatch"`
			} `yaml:"on"`
		}
		if yaml.Unmarshal(b, &doc) != nil || doc.On.Dispatch == nil {
			continue
		}
		w := DispatchWorkflow{File: n, Name: doc.Name}
		for k, v := range doc.On.Dispatch.Inputs {
			w.Inputs = append(w.Inputs, k)
			if k == "environment" {
				w.EnvOptions = v.Options
			}
		}
		sort.Strings(w.Inputs)
		out = append(out, w)
	}
	return out, nil
}

// FindDispatch returns the hand-started workflow taking every named input; ok
// is false when there is none, or more than one.
func (l *Layout) FindDispatch(inputs ...string) (DispatchWorkflow, bool, error) {
	ws, err := l.DispatchWorkflows()
	if err != nil {
		return DispatchWorkflow{}, false, err
	}
	var hit []DispatchWorkflow
	for _, w := range ws {
		if w.Has(inputs...) {
			hit = append(hit, w)
		}
	}
	if len(hit) != 1 {
		return DispatchWorkflow{}, false, nil
	}
	return hit[0], true, nil
}

// RepoSlug is the cluster repository as owner/name.
func (l *Layout) RepoSlug() (owner, name string, ok bool) {
	parts := strings.Split(strings.Trim(l.Workflow.ClusterRepository, "/"), "/")
	if len(parts) >= 3 {
		return parts[len(parts)-2], parts[len(parts)-1], true
	}
	if len(parts) == 2 {
		return parts[0], parts[1], true
	}
	return "", "", false
}
