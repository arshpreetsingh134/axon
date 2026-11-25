package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"axon/internal/official"
)

func TestEvalGHExpressions(t *testing.T) {
	m := map[string]string{"token": "quote", "workflow_worker": "1"}
	cases := map[string]string{
		"business-${{ matrix.token }}:${{ github.sha }}":                          "business-quote:",
		"${{ matrix.workflow_worker == '1' && 'workflow-worker' || 'standard' }}": "workflow-worker",
		"${{ matrix.missing == '1' && 'workflow-worker' || 'standard' }}":         "standard",
		"TOKEN=${{ matrix.token }}\nSECRET=${{ secrets.X }}":                      "TOKEN=quote\nSECRET=",
	}
	for in, want := range cases {
		if got := evalGH(in, m); got != want {
			t.Errorf("evalGH(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatrixRows(t *testing.T) {
	if rows := matrixRows(map[string]any{"include": []any{map[string]any{"token": "a"}, map[string]any{"token": "b", "x": 1}}}); len(rows) != 2 || rows[1]["x"] != "1" {
		t.Fatalf("include: %v", rows)
	}
	if rows := matrixRows(map[string]any{"app": []any{"portal", "workspace"}}); len(rows) != 2 || rows[0]["app"] != "portal" {
		t.Fatalf("list: %v", rows)
	}
	if rows := matrixRows(nil); len(rows) != 1 {
		t.Fatalf("no matrix: %v", rows)
	}
}

func TestNames(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io/o/r/business-quote@sha256:abc": "ghcr.io/o/r/business-quote",
		"ghcr.io/o/r/business-quote:1234":       "ghcr.io/o/r/business-quote",
		"localhost:5000/x":                      "localhost:5000/x",
	} {
		if got := imageRepo(in); got != want {
			t.Errorf("imageRepo(%q) = %q", in, got)
		}
	}
	for in, want := range map[string]string{
		"https://github.com/Flarre-AI/repo.git": "flarre-ai/repo",
		"git@github.com:flarre-ai/repo.git":     "flarre-ai/repo",
		"flarre-ai/repo":                        "flarre-ai/repo",
	} {
		if got := repoSlug(in); got != want {
			t.Errorf("repoSlug(%q) = %q", in, got)
		}
	}
}

func TestDockerfileWatchFollowsCopyFromContexts(t *testing.T) {
	dir := t.TempDir()
	df := filepath.Join(dir, "Dockerfile")
	os.WriteFile(df, []byte("FROM x\nCOPY --from=descriptor /runtime/compat/ /src/\nCOPY --from=descriptor /drafts/${TOKEN}.toml /m\nCOPY --from=source /a /b\n"), 0o644)
	w := dockerfileWatch(df, map[string]string{"descriptor": "/c"}, map[string]string{"TOKEN": "quote"})
	got := strings.Join(w, ",")
	if got != dir+",/c/runtime/compat,/c/drafts/quote.toml" {
		t.Fatalf("watch = %s", got)
	}
}

func TestDevIdentitiesFromWorkflowClientEnv(t *testing.T) {
	d := &official.Descriptor{}
	d.App = append(d.App, struct {
		Name     string            `toml:"name"`
		Kind     string            `toml:"kind"`
		Host     string            `toml:"host"`
		DB       string            `toml:"db"`
		Temporal string            `toml:"temporal"`
		Port     int               `toml:"port"`
		Env      map[string]string `toml:"env"`
	}{Name: "shop.wfc", Kind: "project", Env: map[string]string{"WFC_SERVICE_CLIENT_ID": "shop.worker", "WFC_SERVICE_KEY_FILE": "/run/key/key.pem",
		"WFC_SERVICE_AUDIENCES": `{"http://a:9080":"shop.app.a","http://b:9080":"shop.app.b","http://a2:9080":"shop.app.a"}`}})
	d.App = append(d.App, d.App[0])
	d.App[1].Name, d.App[1].Env = "other.wfc", map[string]string{"WFC_SERVICE_CLIENT_ID": "${FROM_SECRET}", "WFC_SERVICE_KEY_FILE": "/k"}
	ids := devIdentities(d, map[string]map[string]string{"shop.wfc": {"WFC_IAM_ISSUER": "http://iam.localhost:8095"}})
	if len(ids) != 1 || ids[0].ClientID != "shop.worker" || strings.Join(ids[0].Sites, ",") != "shop.app.a,shop.app.b" ||
		ids[0].Issuer != "http://iam.localhost:8095" || ids[0].Secret != "app-shop-wfc-service-key" {
		t.Fatalf("identities %+v", ids)
	}
}
