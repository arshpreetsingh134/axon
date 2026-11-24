package ci

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// Actor is who started a run.
type Actor struct {
	Login string `json:"login"`
}

// RunDetail is a run with the account that triggered it.
type RunDetail struct {
	Run
	Triggering Actor `json:"triggering_actor"`
}

// Active reports whether a run is queued, waiting or running.
func (r Run) Active() bool { return !r.Done() }

// WorkflowRuns lists the latest runs of one workflow file, newest first.
func (g *GitHub) WorkflowRuns(workflow string, n int) ([]RunDetail, error) {
	var out struct {
		Runs []RunDetail `json:"workflow_runs"`
	}
	err := g.get(fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/runs?per_page=%d", g.Owner, g.Repo, url.PathEscape(workflow), n), &out)
	return out.Runs, err
}

// GetRunDetail reads one run with its triggering account.
func (g *GitHub) GetRunDetail(id int64) (RunDetail, error) {
	var r RunDetail
	err := g.get(fmt.Sprintf("/repos/%s/%s/actions/runs/%d", g.Owner, g.Repo, id), &r)
	return r, err
}

// Dispatch starts a workflow_dispatch run of workflow on ref with inputs.
func (g *GitHub) Dispatch(workflow, ref string, inputs map[string]string) error {
	body, _ := json.Marshal(map[string]any{"ref": ref, "inputs": inputs})
	return g.send("POST", fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/dispatches", g.Owner, g.Repo, url.PathEscape(workflow)), body)
}

// Login is the account the token belongs to.
func (g *GitHub) Login() (string, error) {
	var u Actor
	err := g.get("/user", &u)
	return u.Login, err
}

// Artifact is one artifact of a run.
type Artifact struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Size    int64  `json:"size_in_bytes"`
	Expired bool   `json:"expired"`
}

// Artifacts lists a run's artifacts.
func (g *GitHub) Artifacts(runID int64) ([]Artifact, error) {
	var out struct {
		Artifacts []Artifact `json:"artifacts"`
	}
	err := g.get(fmt.Sprintf("/repos/%s/%s/actions/runs/%d/artifacts?per_page=100", g.Owner, g.Repo, runID), &out)
	return out.Artifacts, err
}

// ArtifactFiles downloads an artifact and returns its regular files whose base
// name matches keep, at most max bytes each. Archive paths are never used to
// write anywhere; callers decide where (if anywhere) the bytes go.
func (g *GitHub) ArtifactFiles(id int64, keep *regexp.Regexp, max int64) (map[string][]byte, error) {
	b, err := g.getRaw(fmt.Sprintf("/repos/%s/%s/actions/artifacts/%d/zip", g.Owner, g.Repo, id))
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, fmt.Errorf("artifact %d is not a zip archive: %w", id, err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		base := path.Base(strings.ReplaceAll(f.Name, "\\", "/"))
		if f.FileInfo().IsDir() || !f.Mode().IsRegular() || !keep.MatchString(base) {
			continue
		}
		if f.UncompressedSize64 > uint64(max) {
			return nil, fmt.Errorf("artifact file %s is larger than %d bytes", base, max)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(rc, max+1))
		rc.Close()
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > max {
			return nil, fmt.Errorf("artifact file %s is larger than %d bytes", base, max)
		}
		out[base] = data
	}
	return out, nil
}

var (
	pushManifestRE = regexp.MustCompile(`pushing manifest for ([a-z0-9][a-z0-9._/:-]*?)(?::[A-Za-z0-9._-]+)?@(sha256:[0-9a-f]{64})`)
	stampRE        = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T[0-9:.]+Z `)
)

// PushedImages reads a build job's log and returns each pushed image
// repository with the digest that was pushed for it (the image index digest for
// a multi-platform build).
func PushedImages(log string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(log, "\n") {
		line = stampRE.ReplaceAllString(strings.TrimRight(line, "\r"), "")
		if m := pushManifestRE.FindStringSubmatch(line); m != nil {
			out[m[1]] = m[2]
		}
	}
	return out
}
