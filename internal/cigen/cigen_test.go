package cigen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"axon/internal/descriptor"
	"axon/internal/layout"
)

func fixture(t *testing.T, ci string) *descriptor.Model {
	t.Helper()
	root := t.TempDir()
	w := func(p, s string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(s), 0o644)
	}
	w(filepath.Join(root, "shop.axon", ".axon.infra", "axonfile"), `build_type = "cluster"
name = "shop"
core_image = "ghcr.io/example/core:1"
[fabric.postgres]
port = 5545
[fabric.traefik]
http = 8090
[[app]]
name = "shop.web"
host = "web"
db = "web"
[app.env]
STRIPE_KEY = "${STRIPE_KEY}"
BASE = "https://${API_HOST}/v1"
[[app]]
name = "forms.app"
host = "forms"
`+ci)
	w(filepath.Join(root, "shop.web", "axonfile"), "build_type = \"app\"\n[app]\nname = \"shop.web\"\n[[compose]]\nsource = \"modules/web/module\"\n")
	w(filepath.Join(root, "shop.web", "modules", "web", "module", "axonfile"), "build_type = \"module\"\n[module]\ntoken = \"web\"\nlanguage = \"go\"\n")
	w(filepath.Join(root, "shop.web", "modules", "web", "module", "ui", "package.json"), "{}")
	w(filepath.Join(root, "forms.app", "axonfile"), "build_type = \"app\"\n[app]\nname = \"forms.app\"\n[[compose]]\nsource = \"modules/f/module\"\nimage = \"ghcr.io/example/f:1\"\ntoken = \"f\"\n")
	f, err := layout.FindCluster(filepath.Join(root, "shop.axon"), layout.ModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	m, err := descriptor.Load(f)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

const repos = `
[ci.checkouts]
"shop.web" = "example/shop.web@main"
".axon-ci/example__forms" = "example/forms@develop"
[ci.links]
"forms.app" = ".axon-ci/example__forms/apps/forms.app"
`

func TestParseCheckout(t *testing.T) {
	c, err := ParseCheckout("x/y/", "o/r@dev")
	if err != nil || c.Repo != "o/r" || c.Ref != "dev" || c.Path != "x/y" || c.Spec() != "o/r@dev" {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []string{"repo", "/o/r", "o/r/x", ""} {
		if _, err := ParseCheckout("a", bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, bad := range []string{"", ".", "../up"} {
		if _, err := ParseCheckout(bad, "o/r"); err == nil {
			t.Errorf("path %q accepted", bad)
		}
	}
}

func TestGenerateWorkflow(t *testing.T) {
	m := fixture(t, repos)
	lay := Layout{RepoBase: "shop.axon", CtrlRel: "."}
	res, err := Generate(m, lay)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := Generate(m, lay)
	if again.Files[WorkflowPath] != res.Files[WorkflowPath] {
		t.Fatal("generation must be deterministic")
	}
	text := res.Files[WorkflowPath]
	var wf map[string]any
	if err := yaml.Unmarshal([]byte(text), &wf); err != nil {
		t.Fatalf("invalid YAML: %v\n%s", err, text)
	}
	jobs := wf["jobs"].(map[string]any)
	if jobs["check"] == nil || jobs["e2e"] == nil {
		t.Fatalf("jobs: %v", jobs)
	}
	if got := strings.Join(res.Secrets, ","); got != "API_HOST,STRIPE_KEY" {
		t.Fatalf("secrets = %s", got)
	}
	for _, want := range []string{
		"STRIPE_KEY: ${{ secrets.STRIPE_KEY }}",
		"repository: example/shop.web", "path: shop.web", "ref: main",
		"repository: example/forms", "path: .axon-ci/example__forms", "ref: develop",
		`ln -sfn "$GITHUB_WORKSPACE/.axon-ci/example__forms/apps/forms.app" "$GITHUB_WORKSPACE/forms.app"`,
		"working-directory: shop.axon", "run: axon ci check", "run: axon cluster check", "run: axon env check ci",
		"axon up --build --env ci --target docker --timeout 20m", "run: axon env test ci",
		"if: always()", "axon down --env ci --volumes --yes",
		"uses: actions/setup-go@v6", "uses: actions/setup-node@v5",
		"registry: ghcr.io", "secrets.AXON_GHCR_TOKEN || github.token",
		"name: 'Unit tests: shop.web'",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("workflow lacks %q", want)
		}
	}
	if strings.Contains(text, "POSTGRES_PASSWORD") {
		t.Error("derived secrets must not be required from GitHub")
	}
}

func TestGenerateRespectsSettings(t *testing.T) {
	m := fixture(t, `
[ci]
branches = ["develop", "release/*"]
runner = "ubuntu-24.04"
e2e = false
app_tests = false
env = "test"
axon_version = "0.1.1"
`)
	res, err := Generate(m, Layout{RepoBase: "mono", CtrlRel: "deploy/shop.axon"})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Files[WorkflowPath]
	if strings.Contains(text, "e2e:") || strings.Contains(text, "Unit tests") {
		t.Error("e2e and app tests were disabled")
	}
	for _, want := range []string{"- develop", "- release/*", "runs-on: ubuntu-24.04", "AXON_VERSION: 0.1.1", "axon env check test", "working-directory: mono/deploy/shop.axon"} {
		if !strings.Contains(text, want) {
			t.Errorf("workflow lacks %q", want)
		}
	}
	m.C.CI.Apps = []string{"shop.web"}
	m.C.CI.E2E = nil
	f := false
	m.C.CI.Hooks = &f
	res, _ = Generate(m, Layout{RepoBase: "mono", CtrlRel: "."})
	if !strings.Contains(res.Files[WorkflowPath], "--app shop.web --no-hooks") {
		t.Error("app subset / hooks flag not rendered")
	}
}

func TestReleaseWorkflow(t *testing.T) {
	m := fixture(t, repos+"\n[release]\nregistry = \"ghcr.io/example/\"\n")
	res, err := Generate(m, Layout{RepoBase: "shop.axon", CtrlRel: "."})
	if err != nil {
		t.Fatal(err)
	}
	text, ok := res.Files[ReleaseWorkflowPath]
	if !ok {
		t.Fatal("no release workflow although a registry is set")
	}
	var wf map[string]any
	if err := yaml.Unmarshal([]byte(text), &wf); err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}
	for _, want := range []string{
		"- v*", "contents: write", "packages: write",
		"registry: ghcr.io", "secrets.AXON_GHCR_TOKEN || github.token",
		`axon release create --build --push --registry ghcr.io/example --version "${GITHUB_REF_NAME#v}"`,
		`gh release upload "$GITHUB_REF_NAME" "releases/$v.toml"`, "repository: example/shop.web",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("release workflow lacks %q", want)
		}
	}
	if _, ok := Generate(fixture(t, ""), Layout{RepoBase: "x", CtrlRel: "."}); ok != nil {
		t.Fatal(ok)
	}
	plain, _ := Generate(fixture(t, ""), Layout{RepoBase: "x", CtrlRel: "."})
	if _, has := plain.Files[ReleaseWorkflowPath]; has {
		t.Fatal("no registry, no release workflow")
	}
	other := fixture(t, "\n[release]\nregistry = \"registry.example.com/team\"\n")
	r2, _ := Generate(other, Layout{RepoBase: "x", CtrlRel: "."})
	if !strings.Contains(r2.Files[ReleaseWorkflowPath], "secrets.AXON_REGISTRY_PASSWORD") || !strings.Contains(strings.Join(r2.Secrets, ","), "AXON_REGISTRY_USERNAME") {
		t.Fatal("a non-GHCR registry needs username/password secrets")
	}
}

func TestMultiArchRelease(t *testing.T) {
	m := fixture(t, "\n[release]\nregistry = \"ghcr.io/example\"\nplatforms = [\"linux/amd64\", \"linux/arm64\"]\n")
	res, err := Generate(m, Layout{RepoBase: "x", CtrlRel: "."})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Files[ReleaseWorkflowPath]
	for _, want := range []string{"docker/setup-qemu-action@v3", "docker/setup-buildx-action@v3"} {
		if !strings.Contains(text, want) {
			t.Errorf("multi-arch release lacks %q", want)
		}
	}
	one := fixture(t, "\n[release]\nregistry = \"ghcr.io/example\"\nplatforms = [\"linux/arm64\"]\n")
	r1, _ := Generate(one, Layout{RepoBase: "x", CtrlRel: "."})
	if strings.Contains(r1.Files[ReleaseWorkflowPath], "setup-qemu") {
		t.Error("one platform needs no QEMU")
	}
}

func writeEnv(t *testing.T, m *descriptor.Model, name, body string) {
	t.Helper()
	p := filepath.Join(m.Dir, "environments", name+".toml")
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDeployWorkflow(t *testing.T) {
	m := fixture(t, repos+"\n[release]\nregistry = \"ghcr.io/example\"\n")
	writeEnv(t, m, "ci", "name = \"ci\"\ntarget = \"docker\"\n")
	writeEnv(t, m, "staging", `name = "staging"
tier = "staging"
[cloud]
provider = "eks"
cluster = "shop-staging"
region = "eu-west-1"
role_arn = "arn:aws:iam::123456789012:role/deploy"
[ci]
deploy = true
auto = true
url = "https://staging.shop.example"
`)
	writeEnv(t, m, "prod", `name = "prod"
tier = "prod"
domain = "shop.example"
promotes_from = "staging"
[cloud]
provider = "kubeconfig"
[ci]
deploy = true
reviewers = ["octocat"]
wait_minutes = 10
`)
	res, err := Generate(m, Layout{RepoBase: "shop.axon", CtrlRel: "."})
	if err != nil {
		t.Fatal(err)
	}
	text, ok := res.Files[DeployWorkflowPath]
	if !ok {
		t.Fatal("no deploy workflow")
	}
	var wf map[string]any
	if err := yaml.Unmarshal([]byte(text), &wf); err != nil {
		t.Fatalf("invalid YAML: %v\n%s", err, text)
	}
	jobs := wf["jobs"].(map[string]any)
	if len(jobs) != 2 || jobs["deploy-staging"] == nil || jobs["deploy-prod"] == nil {
		t.Fatalf("jobs: %v", jobs)
	}
	prod := jobs["deploy-prod"].(map[string]any)
	if prod["needs"] != "deploy-staging" || prod["environment"] != "prod" {
		t.Fatalf("prod job: %v", prod)
	}
	if !strings.Contains(prod["if"].(string), "needs.deploy-staging.result == 'success'") {
		t.Errorf("prod must promote from staging: %v", prod["if"])
	}
	stg := jobs["deploy-staging"].(map[string]any)
	if env := stg["environment"].(map[string]any); env["url"] != "https://staging.shop.example" {
		t.Errorf("staging environment: %v", env)
	}
	for _, want := range []string{
		"workflows:", "- axon-release", "id-token: write",
		"aws-actions/configure-aws-credentials@v4", "role-to-assume: arn:aws:iam::123456789012:role/deploy",
		"aws eks update-kubeconfig --name shop-staging --region eu-west-1",
		"secrets.KUBECONFIG", `axon release checkout "${AXON_TAG#v}"`,
		`axon env deploy prod "${AXON_TAG#v}" --target k8s`, "AXON_PULL_REGISTRY: ghcr.io",
		"STRIPE_KEY: ${{ secrets.STRIPE_KEY }}", "fetch-depth: 0", "ref: ${{ env.AXON_TAG }}",
		"port-forward svc/traefik",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("deploy workflow lacks %q", want)
		}
	}
	if strings.Contains(text, "setup-go") {
		t.Error("deploys pull released images; no toolchain needed")
	}
	if got := res.EnvSecrets["prod"]; len(got) != 1 || got[0] != "KUBECONFIG" {
		t.Errorf("env secrets: %v", res.EnvSecrets)
	}
	if len(res.Envs) != 2 || res.Envs[0].Name != "prod" || res.Envs[0].Reviewers[0] != "octocat" || res.Envs[0].WaitMinutes != 10 {
		t.Errorf("envs: %+v", res.Envs)
	}

	// no registry → refused; bad cloud → refused
	m2 := fixture(t, "")
	writeEnv(t, m2, "prod", "name = \"prod\"\n[cloud]\nprovider = \"eks\"\n[ci]\ndeploy = true\n")
	if _, err := Generate(m2, Layout{RepoBase: "x", CtrlRel: "."}); err == nil {
		t.Error("an incomplete [cloud] must be rejected")
	}
	writeEnv(t, m2, "prod", "name = \"prod\"\n[cloud]\nprovider = \"kubeconfig\"\n[ci]\ndeploy = true\n")
	if _, err := Generate(m2, Layout{RepoBase: "x", CtrlRel: "."}); err == nil || !strings.Contains(err.Error(), "registry") {
		t.Errorf("deploying without a registry: %v", err)
	}
}
