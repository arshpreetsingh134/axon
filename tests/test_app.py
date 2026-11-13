"""App verbs: app add/remove module, app set, app check (--json, --fix), app test."""

import json

import pytest

from _util import load_toml, read, snapshot, tree, write


@pytest.fixture
def app(axon, tmp_path):
    """A scaffolded Go app "shop" (seed module "shop", no rings)."""
    r = axon("app", "new", "shop", "--rings", "none", cwd=tmp_path)
    assert r.code == 0, r.err
    return tmp_path / "shop"


def composed(app_dir):
    return [c["source"] for c in load_toml(app_dir / "axonfile").get("compose", [])]


# ── app add module / app remove module ───────────────────────────────────────


def test_add_module_composes_and_scaffolds(axon, app):
    r = axon("app", "add", "module", "Billing-Engine", "--lang", "rust", "--rings", "none", cwd=app)
    assert r.code == 0, r.err
    assert 'module "billing_engine" added' in r.err
    assert composed(app) == ["modules/shop/module", "modules/billing_engine/module"]
    files = tree(app / "modules" / "billing_engine")
    assert "module/service/Cargo.toml" in files and "module/axonfile" in files
    m = load_toml(app / "modules" / "billing_engine" / "module" / "axonfile")["module"]
    assert (m["token"], m["language"]) == ("billing_engine", "rust")


def test_add_module_preserves_the_rest_of_the_descriptor(axon, app):
    write(app / "axonfile", read(app / "axonfile").replace("[home]\n", "# my home page\n[home]\n"))
    before = read(app / "axonfile")
    assert axon("app", "add", "module", "billing", "--rings", "none", cwd=app).code == 0
    after = read(app / "axonfile")
    assert after.startswith(before.rstrip("\n"))
    assert after[len(before.rstrip("\n")):] == '\n\n[[compose]]\nsource = "modules/billing/module"\n'


def test_add_module_with_dir_flag(axon, app, tmp_path):
    assert axon("app", "add", "module", "billing", "--rings", "none", "--dir", str(app), cwd=tmp_path).code == 0
    assert "modules/billing/module" in composed(app)


def test_add_module_duplicate(axon, app):
    r = axon("app", "add", "module", "shop", cwd=app)
    assert (r.code, r.err) == (1, 'axon: module "shop" is already composed\n')


def test_add_module_needs_an_app_descriptor(axon, app):
    mod = app / "modules" / "shop" / "module"
    r = axon("app", "add", "module", "x", cwd=mod)
    assert r.code == 1
    assert 'build_type="module"; modules belong to an app' in r.err


def test_remove_module_keeps_files(axon, app):
    assert axon("app", "add", "module", "billing", "--rings", "none", cwd=app).code == 0
    r = axon("app", "remove", "module", "billing", cwd=app)
    assert r.code == 0, r.err
    assert composed(app) == ["modules/shop/module"]
    assert (app / "modules" / "billing" / "module" / "axonfile").is_file()
    assert "its files remain; delete with: rm -rf " in r.err


def test_remove_module_round_trip_restores_the_descriptor(axon, app):
    before = read(app / "axonfile")
    assert axon("app", "add", "module", "billing", "--rings", "none", cwd=app).code == 0
    assert axon("app", "remove", "module", "billing", cwd=app).code == 0
    assert read(app / "axonfile").rstrip() == before.rstrip()


def test_remove_module_not_composed(axon, app):
    r = axon("app", "remove", "module", "nope", cwd=app)
    assert (r.code, r.err) == (1, 'axon: module "nope" is not composed\n')


def test_remove_module_purge_with_yes(axon, app):
    assert axon("app", "add", "module", "billing", "--rings", "none", cwd=app).code == 0
    r = axon("app", "remove", "module", "billing", "--purge", "-y", cwd=app)
    assert r.code == 0, r.err
    assert not (app / "modules" / "billing").exists()


def test_remove_module_purge_refuses_without_confirmation(axon, app):
    assert axon("app", "add", "module", "billing", "--rings", "none", cwd=app).code == 0
    r = axon("app", "remove", "module", "billing", "--purge", cwd=app)
    assert r.code == 1
    assert "without confirmation (pass --yes)" in r.err
    assert (app / "modules" / "billing" / "module" / "axonfile").is_file()


# ── app set ──────────────────────────────────────────────────────────────────


def test_app_set_prints_bare_keys(axon, app):
    r = axon("app", "set", "build-type", "app.name", "app.title", "home.title", cwd=app)
    assert (r.code, r.out) == (0, "app\nshop_app\nShop\nWelcome to Shop\n")


def test_app_set_updates_keys(axon, app):
    r = axon("app", "set", "app.title=Shop Front", "app.search_placeholder=Find…", cwd=app)
    assert r.code == 0, r.err
    d = load_toml(app / "axonfile")
    assert d["app"]["title"] == "Shop Front" and d["app"]["search_placeholder"] == "Find…"


@pytest.mark.parametrize("value,why", [
    ("project", "build_type=project requires at least one [[project.build]] step"),
    ("module", "[module].token is required (build_type=module)"),
])
def test_app_set_build_type_reverts_when_invalid(axon, app, value, why):
    before = snapshot(app)
    r = axon("app", "set", "build-type=" + value, cwd=app)
    assert r.code == 1
    assert r.err == "axon: reverted: %s\n" % why
    assert snapshot(app) == before  # unchanged, and no temp file left behind


def test_app_set_build_type_project_when_valid(axon, app):
    write(app / "axonfile", read(app / "axonfile") + '\n[project]\ndockerfile = "Dockerfile"\n\n[[project.build]]\nname = "image"\ncommand = "docker build ."\n')
    r = axon("app", "set", "build-type=project", cwd=app)
    assert r.code == 0, r.err
    assert load_toml(app / "axonfile")["build_type"] == "project"


def test_app_set_rejects_unknown_build_type(axon, app):
    before = read(app / "axonfile")
    r = axon("app", "set", "build-type=bogus", cwd=app)
    assert (r.code, r.err) == (2, "axon: app set: build-type must be app, module or project\n")
    assert read(app / "axonfile") == before


# ── app check ────────────────────────────────────────────────────────────────


def test_app_check_table_and_report(axon, app):
    r = axon("app", "check", cwd=app)
    assert r.code == 0
    lines = r.out.splitlines()
    assert lines[0].split() == ["TOOL", "STATUS", "VERSION", "PATH"]
    tools = [l.split()[0] for l in lines[1:10]]
    assert tools == ["go", "cargo", "node", "npm", "docker", "kubectl", "git", "k6", "config"]
    assert "✓ 0 blocking · 0 risk · 0 note" in r.out


def test_app_check_json_shape(axon, app):
    write(app / "axonfile", read(app / "axonfile") + '\n[[compose]]\nsource = "modules/ghost/module"\n')
    r = axon("app", "check", "--json", cwd=app)
    d = json.loads(r.out)
    assert set(d) == {"toolchain", "findings"}
    assert [t["name"] for t in d["toolchain"]] == ["go", "cargo", "node", "npm", "docker", "kubectl", "git", "k6"]
    for t in d["toolchain"]:
        assert set(t) == {"name", "ok", "version", "path"}
    assert d["findings"] == [{"id": "APP_MODULE_MISSING", "concern": "conformance", "severity": "blocking",
                              "subject": "modules/ghost/module", "cause": "composed but missing on disk"}]


def test_app_check_json_clean_has_empty_findings_list(axon, app):
    d = json.loads(axon("app", "check", "--json", cwd=app).out)
    assert d["findings"] == []


def test_app_check_blocking_finding_fails(axon, app):
    write(app / "axonfile", read(app / "axonfile") + '\n[[compose]]\nsource = "modules/ghost/module"\n')
    r = axon("app", "check", cwd=app)
    assert r.code == 1
    assert "✗ 1 blocking" in r.out and "APP_MODULE_MISSING  modules/ghost/module — composed but missing on disk" in r.out


def test_app_check_json_blocking_finding_fails(axon, app):
    write(app / "axonfile", read(app / "axonfile") + '\n[[compose]]\nsource = "modules/ghost/module"\n')
    r = axon("app", "check", "--json", cwd=app)
    assert json.loads(r.out)["findings"][0]["severity"] == "blocking"
    assert r.code == 1


def test_app_check_flags_and_fixes_uncomposed_modules(axon, app):
    assert axon("app", "add", "module", "billing", "--rings", "none", cwd=app).code == 0
    assert axon("app", "remove", "module", "billing", cwd=app).code == 0
    r = axon("app", "check", cwd=app)
    assert r.code == 0  # a risk, not blocking
    assert "APP_MODULE_NOT_COMPOSED  modules/billing/module — module dir exists but is not composed" in r.out
    assert "repair: axon app check --fix" in r.out
    r = axon("app", "check", "--fix", cwd=app)
    assert r.code == 0
    assert "composed modules/billing/module" in r.err
    assert composed(app) == ["modules/shop/module", "modules/billing/module"]
    assert "0 blocking · 0 risk · 0 note" in axon("app", "check", cwd=app).out


def test_app_check_validates_the_descriptor(axon, tmp_path):
    write(tmp_path / "mod" / "axonfile", 'build_type = "module"\n[module]\ntoken = "x"\nlanguage = "java"\n')
    r = axon("app", "check", str(tmp_path / "mod"), cwd=tmp_path)
    assert r.code == 1
    assert 'MODULE_LANGUAGE  [module] — language "java" (go or rust)' in r.out


def test_app_check_finds_descriptors_in_module_subdir(axon, tmp_path):
    write(tmp_path / "pkg" / "module" / "axonfile", 'build_type = "module"\n[module]\ntoken = "x"\nlanguage = "go"\n')
    r = axon("app", "check", cwd=tmp_path / "pkg")
    assert r.code == 0, r.err


def test_app_test_without_suites(axon, cluster):
    assert axon("cluster", "add", "app", "--name", "web", "--from-image", "reg.example/w:1", cwd=cluster.ctrl).code == 0
    r = axon("app", "test", cwd=cluster.workspace / "web")
    assert r.code == 0
    assert "no test suites found" in r.err
