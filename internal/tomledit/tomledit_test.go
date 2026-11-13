package tomledit

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

const sample = `# cluster descriptor
build_type = "cluster"
name = "demo"   # the name

[fabric.postgres]
port = 5545

# Member apps
[[app]]
name = "a.iam"
kind = "iam"

# the web app
[[app]]
name = "a.web"
host = "web"
[app.env]
KEY = "v"

[product.clc]
host = "clc"
`

func decodes(t *testing.T, s string) {
	t.Helper()
	var v map[string]any
	if _, err := toml.Decode(s, &v); err != nil {
		t.Fatalf("result does not decode: %v\n%s", err, s)
	}
}

func TestRemoveArrayElemKeepsOthersAndComments(t *testing.T) {
	d := Parse(sample)
	if !d.RemoveArrayElem("app", "name", "a.web") {
		t.Fatal("not removed")
	}
	out := d.String()
	decodes(t, out)
	if strings.Contains(out, "a.web") || strings.Contains(out, "KEY") || strings.Contains(out, "# the web app") {
		t.Fatalf("leftovers:\n%s", out)
	}
	for _, keep := range []string{"# cluster descriptor", "# the name", "a.iam", "[product.clc]", "# Member apps"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("lost %q:\n%s", keep, out)
		}
	}
}

func TestSetKeepsTrailingComment(t *testing.T) {
	d := Parse(sample)
	d.Set("", "name", Quote("other"))
	d.Set("fabric.postgres", "port", "6000")
	d.Set("fabric.postgres", "image", Quote("postgres:17"))
	out := d.String()
	decodes(t, out)
	if !strings.Contains(out, `name = "other"  # the name`) {
		t.Fatalf("comment lost:\n%s", out)
	}
	if v, _ := d.Get("fabric.postgres", "port"); v != "6000" {
		t.Fatalf("port = %q", v)
	}
	if v, _ := d.Get("fabric.postgres", "image"); v != "postgres:17" {
		t.Fatalf("image = %q", v)
	}
}

func TestQuotedTableNames(t *testing.T) {
	d := Parse("[app_env]\n  [app_env.\"x.y\"]\n    A = \"1\"\n")
	d.Set(`app_env."x.y"`, "B", Quote("2"))
	d.Set(`app_env."new.app"`, "C", Quote("3"))
	out := d.String()
	decodes(t, out)
	var v struct {
		AppEnv map[string]map[string]string `toml:"app_env"`
	}
	toml.Decode(out, &v)
	if v.AppEnv["x.y"]["B"] != "2" || v.AppEnv["new.app"]["C"] != "3" || v.AppEnv["x.y"]["A"] != "1" {
		t.Fatalf("got %+v\n%s", v.AppEnv, out)
	}
}

func TestRemoveTableAndAppend(t *testing.T) {
	d := Parse(sample)
	if !d.RemoveTable("product.clc") || d.HasTable("product.clc") {
		t.Fatal("product not removed")
	}
	d.InsertBlockBeforeArray("app", "[fabric.clickhouse]\nhttp_port = 8123")
	out := d.String()
	decodes(t, out)
	if strings.Index(out, "[fabric.clickhouse]") > strings.Index(out, "[[app]]") {
		t.Fatalf("fabric block should precede the apps:\n%s", out)
	}
}

func TestEncodeGuess(t *testing.T) {
	for in, want := range map[string]string{"12": "12", "true": "true", "x": `"x"`, "[a, b]": `["a", "b"]`, "[]": "[]"} {
		if got := EncodeGuess(in); got != want {
			t.Errorf("EncodeGuess(%q) = %s, want %s", in, got, want)
		}
	}
}
