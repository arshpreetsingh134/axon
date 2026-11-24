package official

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const descriptorFixture = `name = "example"
# "${NOT_A_KEY}" in a comment is not required

[[app]]
name = "example.workspace"
[app.env]
PLAIN = "1"
FORMS_CLIENT_EMAIL = "${FORMS_CLIENT_EMAIL}"   # trailing comment ${ALSO_NOT}
EMBEDDED = "https://${HOST}/x"
QUOTED_HASH = "a#${NOT_EXACT}"

[app.ports]
PORT = "${PORT_IS_NOT_ENV}"

[[app]]
name = "example.worker"
env = { HELIX_WRITER_KEY = "${HELIX_WRITER_KEY}" }

[[app]]
name = "example.iam"
[app.env]
FORMS_CLIENT_EMAIL = "${FORMS_CLIENT_EMAIL}"

[fabric.postgres]
PASSWORD = "${POSTGRES_PASSWORD}"
`

func TestRequiredKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mycelfile")
	if err := os.WriteFile(p, []byte(descriptorFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	refs, comments, err := RequiredKeys(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(KeyNames(refs), ","); got != "FORMS_CLIENT_EMAIL,HELIX_WRITER_KEY" {
		t.Fatalf("required %s; refs %+v", got, refs)
	}
	lines := map[string]int{}
	for _, r := range refs {
		lines[r.App+"/"+r.Name] = r.Line
	}
	if lines["example.workspace/FORMS_CLIENT_EMAIL"] != 8 || lines["example.iam/FORMS_CLIENT_EMAIL"] != 22 {
		t.Errorf("lines %v", lines)
	}
	if l, ok := lines["example.worker/HELIX_WRITER_KEY"]; !ok || l != 0 {
		t.Errorf("inline-table key should be found by the decoder without a line: %v", lines)
	}
	var cn []string
	for _, c := range comments {
		cn = append(cn, c.Name)
	}
	if strings.Join(cn, ",") != "NOT_A_KEY,ALSO_NOT" {
		t.Errorf("comment refs %v", cn)
	}
}

func TestInventoryRoundTripAndPermissions(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	inv, ok, err := LoadInventory("example cluster", "qa")
	if err != nil || ok {
		t.Fatalf("fresh inventory: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(inv.Path, filepath.Join("axon", "inventory", "example_cluster", "qa.keys")) {
		t.Fatalf("path %s", inv.Path)
	}
	inv.Keys = []string{"B_KEY", "A_KEY", "B_KEY"}
	inv.Source = "manual"
	if err := inv.Save(); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(inv.Path)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	got, ok, err := LoadInventory("example cluster", "qa")
	if err != nil || !ok || strings.Join(got.Keys, ",") != "A_KEY,B_KEY" || got.Source != "manual" || time.Since(got.Updated) > time.Minute {
		t.Fatalf("reloaded %+v ok=%v err=%v", got, ok, err)
	}
	if !got.Has("A_KEY") || got.Has("C_KEY") {
		t.Error("Has")
	}
	got.Keys = append(got.Keys, "not a key=value")
	if err := got.Save(); err == nil {
		t.Error("a value-like entry was saved")
	}
	if err := os.WriteFile(inv.Path, []byte("A_KEY\nSECRET=hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadInventory("example cluster", "qa"); err == nil {
		t.Error("an inventory holding a value was accepted")
	}
}
