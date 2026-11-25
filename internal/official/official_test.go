package official

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	dA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	c1 = "1111111111111111111111111111111111111111"
	c2 = "2222222222222222222222222222222222222222"
	c3 = "3333333333333333333333333333333333333333"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture is a small, consistent official layout.
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, WorkflowFile, `schema_version = 2
cluster_repository = "github.com/example/cluster"
source_lock = "github/sources.lock.toml"
runner_version = "0.1.80"
environments = ["qa"]
release_platforms = ["linux/arm64"]
`)
	write(t, root, DefaultLock, `version = 1

[[member]]
app_id = "example.billing.app"
repository = "https://github.com/example/example.billing.app.git"
commit = "`+c1+`"

[[member]]
app_id = "example.policy.app.source"
repository = "https://github.com/example/example.policy.app.git"
commit = "`+c2+`"
`)
	write(t, root, MaterializationsDir+"/example.billing.app.billing/mycelfile", `build_type = "app"
[app]
name = "example.billing.app.billing"
[[compose]]
source = "module"
image = "ghcr.io/example/business-billing@`+dA+`"
token = "billing"
`)
	write(t, root, MaterializationsDir+"/example.policy.app/mycelfile", `build_type = "app"
[app]
name = "example.policy.app"
[[compose]]
source = "modules/policy/module"
`)
	write(t, root, MaterializationsDir+"/example.worker/.mycel.infra/Dockerfile", "FROM ghcr.io/example/worker-build@"+dB+"\n")
	write(t, root, ".github/workflows/business-build.yml", `name: business
jobs:
  build:
    strategy:
      matrix:
        include:
          - token: billing
            repo: example/example.billing.app
            revision: `+c1+`
          - token: policy
            repo: example.policy.app
            revision: `+c2+`
`)
	write(t, root, ".github/workflows/worker-build.yml", `name: worker
jobs:
  build:
    steps:
      - uses: actions/checkout@v4
        with:
          repository: example/example.billing.wfc
          ref: `+c3+`
`)
	write(t, root, ".github/workflows/smoke.yml", `jobs:
  check:
    strategy:
      matrix:
        include:
          - token: billing
            image: ghcr.io/example/business-billing@`+dA+`
# a comment naming ghcr.io/example/business-billing@`+dB+` is not a pin
`)
	return root
}

func TestDetect(t *testing.T) {
	if _, ok, err := Detect(t.TempDir()); ok || err != nil {
		t.Fatalf("empty dir: ok=%v err=%v", ok, err)
	}
	l, ok, err := Detect(fixture(t))
	if !ok || err != nil {
		t.Fatalf("fixture: ok=%v err=%v", ok, err)
	}
	if l.Owner() != "example" || l.Host() != "github.com" || l.Workflow.RunnerVersion != "0.1.80" {
		t.Fatalf("owner %q host %q runner %q", l.Owner(), l.Host(), l.Workflow.RunnerVersion)
	}
}

func TestLock(t *testing.T) {
	l, _, _ := Detect(fixture(t))
	ms, err := l.Lock()
	if err != nil || len(ms) != 2 || ms[1].AppID != "example.policy.app.source" || ms[1].Commit != c2 {
		t.Fatalf("lock %+v err %v", ms, err)
	}
	write(t, l.Root, DefaultLock, "version = 1\n[[member]]\napp_id = \"x\"\nrepository = \"https://github.com/example/x.git\"\ncommit = \"abc\"\n")
	if _, err := l.Lock(); err == nil || !strings.Contains(err.Error(), "40-hex") {
		t.Fatalf("short commit accepted: %v", err)
	}
}

func TestParseRepo(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/example/a.git": "github.com/example/a",
		"github.com/example/a":             "github.com/example/a",
		"example/a":                        "github.com/example/a",
		"a":                                "github.com/owner/a",
		"https://git.example.org/x/y":      "git.example.org/x/y",
	} {
		r, err := ParseRepo(in, "github.com", "owner")
		if err != nil || r.String() != want {
			t.Errorf("%s → %s (%v), want %s", in, r, err, want)
		}
	}
	if _, err := ParseRepo("a", "github.com", ""); err == nil {
		t.Error("bare name without an owner accepted")
	}
}

func TestParsePin(t *testing.T) {
	img, d, ok := ParsePin("ghcr.io/example/app:1.2@" + dA)
	if !ok || img != "ghcr.io/example/app" || d != dA {
		t.Fatalf("tagged pin: %q %q %v", img, d, ok)
	}
	if _, _, ok := ParsePin("ghcr.io/example/app:1.2"); ok {
		t.Fatal("tag-only reference counted as a pin")
	}
}

func TestPinsAndSourceRefs(t *testing.T) {
	l, _, _ := Detect(fixture(t))
	pins, err := l.Pins()
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 3 { // member file, worker Dockerfile, smoke workflow (the comment is skipped)
		t.Fatalf("pins %+v", pins)
	}
	refs, err := l.SourceRefs()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range refs {
		got[r.Repo.Slug] = r.Commit
	}
	if got["example/example.billing.app"] != c1 || got["example/example.policy.app"] != c2 || got["example/example.billing.wfc"] != c3 {
		t.Fatalf("refs %+v", refs)
	}
}

func TestCheckPinsClean(t *testing.T) {
	l, _, _ := Detect(fixture(t))
	fs, err := CheckPins(l)
	if err != nil || len(fs) != 0 {
		t.Fatalf("findings %+v err %v", fs, err)
	}
}

func TestCheckPinsFindsProblems(t *testing.T) {
	root := fixture(t)
	// the smoke workflow drifts from the member file
	b, _ := os.ReadFile(filepath.Join(root, ".github/workflows/smoke.yml"))
	write(t, root, ".github/workflows/smoke.yml", strings.Replace(string(b), "business-billing@"+dA, "business-billing@"+dB, 1))
	// a build revision drifts from the lock
	b, _ = os.ReadFile(filepath.Join(root, ".github/workflows/business-build.yml"))
	write(t, root, ".github/workflows/business-build.yml", strings.Replace(string(b), "revision: "+c2, "revision: "+c3, 1))
	// a member names an image by tag only
	write(t, root, MaterializationsDir+"/example.claims.app.claims/mycelfile", "[app]\nname = \"example.claims.app.claims\"\n[[compose]]\nsource = \"module\"\nimage = \"ghcr.io/example/business-claims:latest\"\n")
	// a member with nothing to run
	write(t, root, MaterializationsDir+"/example.empty/mycelfile", "[app]\nname = \"example.empty\"\n")

	l, _, _ := Detect(root)
	fs, err := CheckPins(l)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, f := range fs {
		ids[f.ID] = true
		// the pipeline releases over a smoke-only digest drift and an unlocked build revision (SPEC.md C.6)
		want := map[string]string{"DIGEST_DISAGREES": "risk", "BUILD_REVISION_NOT_LOCKED": "risk"}[f.ID]
		if want == "" {
			want = "blocking"
		}
		if f.Severity != want {
			t.Errorf("%s is %s, want %s", f.ID, f.Severity, want)
		}
	}
	for _, want := range []string{"DIGEST_DISAGREES", "BUILD_REVISION_NOT_LOCKED", "IMAGE_NOT_PINNED", "MEMBER_NO_IMAGE"} {
		if !ids[want] {
			t.Errorf("missing %s in %+v", want, fs)
		}
	}
	if len(fs) != 4 {
		t.Errorf("want 4 findings, got %d: %+v", len(fs), fs)
	}
}

func TestCheckPinsBlocksADeployedDigestDrift(t *testing.T) {
	root := fixture(t)
	// a worker Dockerfile deploys another build of the member's image: the release would run two builds of one image
	write(t, root, MaterializationsDir+"/example.worker2/.mycel.infra/Dockerfile", "FROM ghcr.io/example/business-billing@"+dB+"\n")
	l, _, _ := Detect(root)
	fs, err := CheckPins(l)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if f.ID == "DIGEST_DISAGREES" {
			if f.Severity != "blocking" {
				t.Fatalf("deployed drift is %s", f.Severity)
			}
			return
		}
	}
	t.Fatalf("no DIGEST_DISAGREES in %+v", fs)
}
