// Package ci talks to the CI provider (GitHub Actions) and to git for releases:
// run status for a commit, failing-log extraction, and tag-driven releases.
package ci

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/nacl/box"

	"axon/internal/sh"
)

// GitHub is a minimal GitHub Actions REST client.
type GitHub struct {
	API   string // https://api.github.com (AXON_GITHUB_API overrides, for tests/GHES)
	Owner string
	Repo  string
	Token string // may be empty (public repos, read-only, rate-limited)
	HTTP  *http.Client
}

// NewGitHub builds a client for owner/repo, resolving a token for that owner.
func NewGitHub(owner, repo string) *GitHub {
	api := os.Getenv("AXON_GITHUB_API")
	if api == "" {
		api = "https://api.github.com"
	}
	return &GitHub{API: strings.TrimRight(api, "/"), Owner: owner, Repo: repo, Token: ResolveToken(owner),
		HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// ResolveToken finds a GitHub token, preferring one for the repository owner:
// AXON_GITHUB_TOKEN, GITHUB_TOKEN, GH_TOKEN, then the GitHub CLI account named
// after the owner (so a machine signed in to a work and a personal account uses
// the right one), then the CLI's active account. The token is never printed.
func ResolveToken(owner string) string {
	for _, k := range []string{"AXON_GITHUB_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	if !sh.Has("gh") {
		return ""
	}
	if owner != "" {
		if r, err := sh.Run(sh.Cmd{Name: "gh", Args: []string{"auth", "token", "--hostname", "github.com", "--user", owner}, Timeout: 15 * time.Second}); err == nil {
			if t := strings.TrimSpace(r.Stdout); t != "" {
				return t
			}
		}
	}
	if r, err := sh.Run(sh.Cmd{Name: "gh", Args: []string{"auth", "token", "--hostname", "github.com"}, Timeout: 15 * time.Second}); err == nil {
		return strings.TrimSpace(r.Stdout)
	}
	return ""
}

// APIError is a non-2xx answer.
type APIError struct {
	Status int
	Path   string
	Msg    string
}

func (e *APIError) Error() string {
	p := e.Path
	if i := strings.IndexByte(p, '?'); i > 0 {
		p = p[:i]
	}
	return fmt.Sprintf("GitHub API %d on %s: %s", e.Status, p, e.Msg)
}

func (g *GitHub) get(path string, out any) error {
	body, err := g.getRaw(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

func (g *GitHub) getRaw(path string) ([]byte, error) {
	req, err := http.NewRequest("GET", g.API+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "axon-cli")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	client := *g.HTTP
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		// never forward the token to another host (logs redirect to blob storage)
		if next.URL.Host != via[0].URL.Host {
			next.Header.Del("Authorization")
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &m)
		msg := m.Message
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		if resp.StatusCode == 404 && g.Token == "" {
			msg += " (no GitHub token found: set GITHUB_TOKEN or sign in with `gh auth login`)"
		}
		if resp.StatusCode == 403 || resp.StatusCode == 404 {
			msg += fmt.Sprintf(" — is the token's account allowed to read %s/%s?", g.Owner, g.Repo)
		}
		return nil, &APIError{Status: resp.StatusCode, Path: path, Msg: msg}
	}
	return b, nil
}

// Run is one workflow run.
type Run struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Title      string    `json:"display_title"`
	Event      string    `json:"event"`
	Branch     string    `json:"head_branch"`
	SHA        string    `json:"head_sha"`
	Status     string    `json:"status"`     // queued | in_progress | completed | waiting | pending | requested
	Conclusion string    `json:"conclusion"` // success | failure | cancelled | skipped | timed_out | action_required | neutral
	URL        string    `json:"html_url"`
	Created    time.Time `json:"created_at"`
	Started    time.Time `json:"run_started_at"`
	Updated    time.Time `json:"updated_at"`
	Attempt    int       `json:"run_attempt"`
}

// Done reports whether the run finished.
func (r Run) Done() bool { return r.Status == "completed" }

// OK reports whether the run finished successfully (skipped/neutral count as OK).
func (r Run) OK() bool {
	return r.Done() && (r.Conclusion == "success" || r.Conclusion == "skipped" || r.Conclusion == "neutral")
}

// Step is one step of a job.
type Step struct {
	Name       string `json:"name"`
	Number     int    `json:"number"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// Job is one job of a run.
type Job struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	Started    time.Time `json:"started_at"`
	Completed  time.Time `json:"completed_at"`
	URL        string    `json:"html_url"`
	Steps      []Step    `json:"steps"`
}

// Failed reports whether the job finished unsuccessfully.
func (j Job) Failed() bool {
	return j.Status == "completed" && j.Conclusion != "success" && j.Conclusion != "skipped" && j.Conclusion != "neutral"
}

// RunsForCommit lists the runs of a commit, newest first.
func (g *GitHub) RunsForCommit(sha string) ([]Run, error) {
	var out struct {
		Runs []Run `json:"workflow_runs"`
	}
	err := g.get(fmt.Sprintf("/repos/%s/%s/actions/runs?head_sha=%s&per_page=50", g.Owner, g.Repo, sha), &out)
	return out.Runs, err
}

// RunsForBranch lists the latest runs on a branch (or tag), newest first.
func (g *GitHub) RunsForBranch(branch string, n int) ([]Run, error) {
	var out struct {
		Runs []Run `json:"workflow_runs"`
	}
	err := g.get(fmt.Sprintf("/repos/%s/%s/actions/runs?branch=%s&per_page=%d", g.Owner, g.Repo, branch, n), &out)
	return out.Runs, err
}

// GetRun reads one run.
func (g *GitHub) GetRun(id int64) (Run, error) {
	var r Run
	err := g.get(fmt.Sprintf("/repos/%s/%s/actions/runs/%d", g.Owner, g.Repo, id), &r)
	return r, err
}

// Jobs lists a run's jobs (latest attempt).
func (g *GitHub) Jobs(runID int64) ([]Job, error) {
	var out struct {
		Jobs []Job `json:"jobs"`
	}
	err := g.get(fmt.Sprintf("/repos/%s/%s/actions/runs/%d/jobs?per_page=100", g.Owner, g.Repo, runID), &out)
	return out.Jobs, err
}

// JobLog downloads a job's plain-text log.
func (g *GitHub) JobLog(jobID int64) (string, error) {
	if g.Token == "" {
		return "", errors.New("reading job logs needs a GitHub token: set GITHUB_TOKEN or sign in with `gh auth login`")
	}
	b, err := g.getRaw(fmt.Sprintf("/repos/%s/%s/actions/jobs/%d/logs", g.Owner, g.Repo, jobID))
	return string(b), err
}

// ReleaseURL returns the html_url of the GitHub Release for a tag ("" if none yet).
func (g *GitHub) ReleaseURL(tag string) string {
	var r struct {
		URL string `json:"html_url"`
	}
	if err := g.get(fmt.Sprintf("/repos/%s/%s/releases/tags/%s", g.Owner, g.Repo, tag), &r); err != nil {
		return ""
	}
	return r.URL
}

// SecretNames lists the repository's Actions secrets (names only; GitHub never
// returns values).
func (g *GitHub) secretsBase(env string) string {
	if env == "" {
		return fmt.Sprintf("/repos/%s/%s/actions", g.Owner, g.Repo)
	}
	return fmt.Sprintf("/repos/%s/%s/environments/%s", g.Owner, g.Repo, url.PathEscape(env))
}

// SecretNames lists the repository's Actions secrets (names only).
func (g *GitHub) SecretNames() (map[string]bool, error) { return g.EnvSecretNames("") }

// EnvSecretNames lists the secrets of a GitHub Environment ("" = the repository's).
func (g *GitHub) EnvSecretNames(env string) (map[string]bool, error) {
	var out struct {
		Secrets []struct {
			Name string `json:"name"`
		} `json:"secrets"`
	}
	if err := g.get(g.secretsBase(env)+"/secrets?per_page=100", &out); err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, s := range out.Secrets {
		names[s.Name] = true
	}
	return names, nil
}

// SetSecret encrypts value with the repository's public key (libsodium sealed
// box) and stores it as an Actions secret. The value never leaves this process
// unencrypted.
func (g *GitHub) SetSecret(name, value string) error { return g.SetEnvSecret("", name, value) }

// SetEnvSecret is SetSecret for a GitHub Environment ("" = the repository).
func (g *GitHub) SetEnvSecret(env, name, value string) error {
	if g.Token == "" {
		return errors.New("changing repository settings needs a GitHub token: set GITHUB_TOKEN or sign in with `gh auth login`")
	}
	var key struct {
		ID  string `json:"key_id"`
		Key string `json:"key"`
	}
	base := g.secretsBase(env)
	if err := g.get(base+"/secrets/public-key", &key); err != nil {
		return err
	}
	enc, err := SealForGitHub(key.Key, value)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"encrypted_value": enc, "key_id": key.ID})
	return g.send("PUT", base+"/secrets/"+url.PathEscape(name), body)
}

// Environment is the protection of a GitHub Environment.
type Environment struct {
	Name        string
	WaitMinutes int
	Reviewers   []string // user logins or "org/team" slugs
	Branches    []string // allowed deployment branches (empty = any)
}

// EnvironmentNames lists the repository's GitHub Environments.
func (g *GitHub) EnvironmentNames() (map[string]bool, error) {
	var out struct {
		Environments []struct {
			Name string `json:"name"`
		} `json:"environments"`
	}
	if err := g.get(fmt.Sprintf("/repos/%s/%s/environments?per_page=100", g.Owner, g.Repo), &out); err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, e := range out.Environments {
		names[e.Name] = true
	}
	return names, nil
}

// PutEnvironment creates or updates a GitHub Environment with required reviewers,
// a wait timer and (optionally) the branches allowed to deploy to it.
func (g *GitHub) PutEnvironment(e Environment) error {
	var reviewers []map[string]any
	for _, r := range e.Reviewers {
		var who struct {
			ID int64 `json:"id"`
		}
		kind := "User"
		path := "/users/" + url.PathEscape(r)
		if org, team, ok := strings.Cut(r, "/"); ok {
			kind, path = "Team", "/orgs/"+url.PathEscape(org)+"/teams/"+url.PathEscape(team)
		}
		if err := g.get(path, &who); err != nil {
			return fmt.Errorf("reviewer %s: %w", r, err)
		}
		reviewers = append(reviewers, map[string]any{"type": kind, "id": who.ID})
	}
	body := map[string]any{"wait_timer": e.WaitMinutes, "reviewers": reviewers, "deployment_branch_policy": nil}
	if reviewers == nil {
		body["reviewers"] = []any{}
	}
	if len(e.Branches) > 0 {
		body["deployment_branch_policy"] = map[string]bool{"protected_branches": false, "custom_branch_policies": true}
	}
	raw, _ := json.Marshal(body)
	base := fmt.Sprintf("/repos/%s/%s/environments/%s", g.Owner, g.Repo, url.PathEscape(e.Name))
	if err := g.send("PUT", base, raw); err != nil {
		return err
	}
	if len(e.Branches) == 0 {
		return nil
	}
	var have struct {
		Policies []struct {
			Name string `json:"name"`
		} `json:"branch_policies"`
	}
	if err := g.get(base+"/deployment-branch-policies?per_page=100", &have); err != nil {
		return err
	}
	known := map[string]bool{}
	for _, p := range have.Policies {
		known[p.Name] = true
	}
	for _, b := range e.Branches {
		if known[b] {
			continue
		}
		raw, _ := json.Marshal(map[string]string{"name": b, "type": "branch"})
		if err := g.send("POST", base+"/deployment-branch-policies", raw); err != nil {
			return fmt.Errorf("branch policy %s: %w", b, err)
		}
	}
	return nil
}

// SealForGitHub encrypts value for a base64 Curve25519 public key (sealed box).
func SealForGitHub(publicKeyB64, value string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(publicKeyB64)
	if err != nil || len(raw) != 32 {
		return "", errors.New("GitHub returned an unexpected public key")
	}
	var pk [32]byte
	copy(pk[:], raw)
	sealed, err := box.SealAnonymous(nil, []byte(value), &pk, rand.Reader)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (g *GitHub) send(method, path string, body []byte) error {
	req, err := http.NewRequest(method, g.API+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "axon-cli")
	req.Header.Set("Content-Type", "application/json")
	if g.Token == "" {
		return errors.New("changing repository settings needs a GitHub token: set GITHUB_TOKEN or sign in with `gh auth login`")
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &m)
		return &APIError{Status: resp.StatusCode, Path: path, Msg: or(m.Message, http.StatusText(resp.StatusCode))}
	}
	return nil
}

func or(a, b string) string {
	if a == "" {
		return b
	}
	return a
}
