package ci

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/nacl/box"
)

func TestParseRemote(t *testing.T) {
	cases := map[string][3]string{
		"https://github.com/arshpreetsingh134/axon.git": {"arshpreetsingh134", "axon", "https"},
		"https://github.com/acme/repo":                  {"acme", "repo", "https"},
		"git@github.com:acme/repo.git":                  {"acme", "repo", "ssh"},
		"ssh://git@github.com/acme/repo.git":            {"acme", "repo", "ssh"},
	}
	for url, want := range cases {
		o, n, https, ok := ParseRemote(url)
		if !ok || o != want[0] || n != want[1] || https != (want[2] == "https") {
			t.Errorf("ParseRemote(%q) = %q %q %v %v", url, o, n, https, ok)
		}
	}
	if _, _, _, ok := ParseRemote("https://gitlab.com/a/b.git"); ok {
		t.Error("non-GitHub remote accepted")
	}
}

func TestVersionBump(t *testing.T) {
	v, ok := ParseVersion("v1.4.9")
	if !ok {
		t.Fatal("parse")
	}
	for part, want := range map[string]string{"patch": "v1.4.10", "minor": "v1.5.0", "major": "v2.0.0", "": "v1.4.10"} {
		if got, _ := v.Bump(part); got.String() != want {
			t.Errorf("bump %s = %s, want %s", part, got, want)
		}
	}
	if _, err := v.Bump("huge"); err == nil {
		t.Error("bad bump accepted")
	}
	a, _ := ParseVersion("0.1.10")
	b, _ := ParseVersion("v0.1.9")
	if a.Less(b) || !b.Less(a) {
		t.Error("versions must compare numerically")
	}
	if _, ok := ParseVersion("v1.2"); ok {
		t.Error("incomplete version accepted")
	}
}

func TestFailureExcerpt(t *testing.T) {
	log := strings.Join([]string{
		"2026-11-13T11:19:00.0000000Z ##[group]Run make lint",
		"2026-11-13T11:19:01.0000000Z lint ok",
		"2026-11-13T11:19:02.0000000Z ##[endgroup]",
		"2026-11-13T11:19:03.0000000Z ##[group]Run make test-py",
		"2026-11-13T11:19:04.0000000Z ##[endgroup]",
		"2026-11-13T11:19:05.0000000Z E       AssertionError: assert 0 == 1",
		"2026-11-13T11:19:06.0000000Z FAILED tests/test_env.py::test_x",
		"2026-11-13T11:19:07.0000000Z ##[error]Process completed with exit code 2.",
		"2026-11-13T11:19:08.0000000Z Post job cleanup.",
	}, "\n")
	got := FailureExcerpt(log, 60)
	for _, want := range []string{"$ make test-py", "AssertionError: assert 0 == 1", "FAILED tests/test_env.py::test_x", "ERROR: Process completed with exit code 2."} {
		if !strings.Contains(got, want) {
			t.Errorf("excerpt lacks %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"lint ok", "Post job cleanup", "2026-11-13T", "##[endgroup]"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("excerpt has %q:\n%s", unwanted, got)
		}
	}
	if short := FailureExcerpt(log, 3); strings.Count(short, "\n") > 3 {
		t.Errorf("max lines not honoured:\n%s", short)
	}
}

func TestGitHubClient(t *testing.T) {
	var logAuth string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logAuth = r.Header.Get("Authorization")
		w.Write([]byte("2026-11-13T00:00:00Z ##[group]Run go test\n2026-11-13T00:00:01Z ##[error]boom\n"))
	}))
	defer storage.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tkn" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.URL.Path == "/repos/o/r/actions/runs" && r.URL.Query().Get("head_sha") == "abc":
			w.Write([]byte(`{"workflow_runs":[{"id":7,"name":"ci","status":"completed","conclusion":"failure","head_sha":"abc"}]}`))
		case r.URL.Path == "/repos/o/r/actions/runs/7/jobs":
			w.Write([]byte(`{"jobs":[{"id":9,"name":"test","status":"completed","conclusion":"failure","steps":[{"name":"Run go test","conclusion":"failure"}]}]}`))
		case r.URL.Path == "/repos/o/r/actions/jobs/9/logs":
			http.Redirect(w, r, storage.URL+"/log", http.StatusFound)
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	defer api.Close()
	g := &GitHub{API: api.URL, Owner: "o", Repo: "r", Token: "tkn", HTTP: http.DefaultClient}
	runs, err := g.RunsForCommit("abc")
	if err != nil || len(runs) != 1 || runs[0].OK() || !runs[0].Done() {
		t.Fatalf("runs %+v err %v", runs, err)
	}
	jobs, err := g.Jobs(7)
	if err != nil || len(jobs) != 1 || !jobs[0].Failed() {
		t.Fatalf("jobs %+v err %v", jobs, err)
	}
	log, err := g.JobLog(9)
	if err != nil || !strings.Contains(log, "boom") {
		t.Fatalf("log %q err %v", log, err)
	}
	if logAuth != "" {
		t.Fatal("the token must not be forwarded to the log storage host")
	}
	if _, err := g.GetRun(404); err == nil || !strings.Contains(err.Error(), "404 on /repos/o/r/actions/runs/404") {
		t.Fatalf("expected a 404 naming the endpoint, got %v", err)
	}
	g.Token = ""
	if _, err := g.JobLog(9); err == nil || !strings.Contains(err.Error(), "needs a GitHub token") {
		t.Fatalf("logs without a token: %v", err)
	}
}

func TestSetSecretEncryptsForTheRepoKey(t *testing.T) {
	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/actions/secrets/public-key":
			fmt.Fprintf(w, `{"key_id":"k1","key":%q}`, base64.StdEncoding.EncodeToString(pub[:]))
		case r.Method == "PUT" && r.URL.Path == "/repos/o/r/actions/secrets/OPENROUTER_API_KEY":
			var body struct {
				Enc string `json:"encrypted_value"`
				Key string `json:"key_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			raw, _ := base64.StdEncoding.DecodeString(body.Enc)
			plain, ok := box.OpenAnonymous(nil, raw, pub, priv)
			if !ok || body.Key != "k1" {
				w.WriteHeader(400)
				return
			}
			got = string(plain)
			w.WriteHeader(201)
		case r.URL.Path == "/repos/o/r/actions/secrets":
			w.Write([]byte(`{"secrets":[{"name":"A"},{"name":"B"}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	g := &GitHub{API: api.URL, Owner: "o", Repo: "r", Token: "t", HTTP: http.DefaultClient}
	if err := g.SetSecret("OPENROUTER_API_KEY", "sk-or-secret"); err != nil {
		t.Fatal(err)
	}
	if got != "sk-or-secret" {
		t.Fatalf("server decrypted %q", got)
	}
	names, err := g.SecretNames()
	if err != nil || !names["A"] || !names["B"] || len(names) != 2 {
		t.Fatalf("names %v err %v", names, err)
	}
	g.Token = ""
	if err := g.SetSecret("X", "y"); err == nil {
		t.Fatal("setting a secret without a token must fail")
	}
}

func TestEnvironmentsAndEnvSecrets(t *testing.T) {
	pub, priv, _ := box.GenerateKey(rand.Reader)
	var put map[string]any
	var policies []string
	var envSecret string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/users/octocat":
			w.Write([]byte(`{"id":42}`))
		case r.Method == "GET" && r.URL.Path == "/orgs/acme/teams/ops":
			w.Write([]byte(`{"id":7}`))
		case r.Method == "PUT" && r.URL.Path == "/repos/o/r/environments/prod":
			json.NewDecoder(r.Body).Decode(&put)
			w.Write([]byte(`{}`))
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/environments/prod/deployment-branch-policies":
			w.Write([]byte(`{"branch_policies":[{"name":"main"}]}`))
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/environments/prod/deployment-branch-policies":
			var b map[string]string
			json.NewDecoder(r.Body).Decode(&b)
			policies = append(policies, b["name"])
			w.WriteHeader(201)
		case r.URL.Path == "/repos/o/r/environments":
			w.Write([]byte(`{"environments":[{"name":"prod"}]}`))
		case r.URL.Path == "/repos/o/r/environments/prod/secrets/public-key":
			fmt.Fprintf(w, `{"key_id":"k2","key":%q}`, base64.StdEncoding.EncodeToString(pub[:]))
		case r.Method == "PUT" && r.URL.Path == "/repos/o/r/environments/prod/secrets/KUBECONFIG":
			var body struct {
				Enc string `json:"encrypted_value"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			raw, _ := base64.StdEncoding.DecodeString(body.Enc)
			plain, _ := box.OpenAnonymous(nil, raw, pub, priv)
			envSecret = string(plain)
			w.WriteHeader(201)
		case r.URL.Path == "/repos/o/r/environments/prod/secrets":
			w.Write([]byte(`{"secrets":[{"name":"KUBECONFIG"}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	g := &GitHub{API: api.URL, Owner: "o", Repo: "r", Token: "t", HTTP: http.DefaultClient}
	err := g.PutEnvironment(Environment{Name: "prod", WaitMinutes: 5, Reviewers: []string{"octocat", "acme/ops"}, Branches: []string{"main", "release/*"}})
	if err != nil {
		t.Fatal(err)
	}
	if put["wait_timer"].(float64) != 5 {
		t.Errorf("wait timer: %v", put)
	}
	revs := put["reviewers"].([]any)
	if len(revs) != 2 || revs[0].(map[string]any)["type"] != "User" || revs[0].(map[string]any)["id"].(float64) != 42 ||
		revs[1].(map[string]any)["type"] != "Team" || revs[1].(map[string]any)["id"].(float64) != 7 {
		t.Errorf("reviewers: %v", revs)
	}
	if bp, _ := put["deployment_branch_policy"].(map[string]any); bp["custom_branch_policies"] != true {
		t.Errorf("branch policy: %v", put)
	}
	if len(policies) != 1 || policies[0] != "release/*" {
		t.Errorf("only the missing branch policy is added: %v", policies)
	}
	names, err := g.EnvironmentNames()
	if err != nil || !names["prod"] {
		t.Fatalf("envs %v %v", names, err)
	}
	if err := g.SetEnvSecret("prod", "KUBECONFIG", "apiVersion: v1"); err != nil || envSecret != "apiVersion: v1" {
		t.Fatalf("env secret %q %v", envSecret, err)
	}
	if s, err := g.EnvSecretNames("prod"); err != nil || !s["KUBECONFIG"] {
		t.Fatalf("env secret names %v %v", s, err)
	}
	if err := g.PutEnvironment(Environment{Name: "prod", Reviewers: []string{"ghost"}}); err == nil {
		t.Error("an unknown reviewer must fail")
	}
}
