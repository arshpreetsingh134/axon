"""Scaffolding: `cluster new` and `app new`."""

import base64
import json
import os
import re
import shutil
import stat
import subprocess

import pytest

from _util import Cluster, load_toml, ports_registry, read, read_dotenv, snapshot, tree

GENERATED_SECRETS = ["POSTGRES_PASSWORD", "OIDC_CLIENT_SECRET", "KEYCLOAK_ADMIN_PASSWORD", "SEED_ADMIN_PASSWORD"]


def mode(p):
    return stat.S_IMODE(os.stat(str(p)).st_mode)


# ── cluster new ──────────────────────────────────────────────────────────────


def test_cluster_new_creates_the_controller_layout(cluster):
    files = tree(cluster.workspace)
    for rel in [
        "acme.axon/.axon.infra/axonfile",
        "acme.axon/.axon.infra/.env",
        "acme.axon/.axon.infra/.env.example",
        "acme.axon/.axon.infra/scripts/provision-keycloak.sh",
        "acme.axon/.axon.infra/scripts/seed-iam.sql",
        "acme.axon/.axon.infra/secrets/.gitkeep",
        "acme.axon/.axon.infra/secrets/iam_signing_key.pem",
        "acme.axon/.axon.infra/journeys/.gitkeep",
        "acme.axon/.axon.infra/fabric/docker-compose.yml",
        "acme.axon/.axon.infra/fabric/traefik/dynamic/routes.yml",
        "acme.axon/.axon.infra/fabric/postgres/init/01-databases.sql",
        "acme.axon/RUNBOOK.md",
        "acme.axon/.gitignore",
        "acme.axon/environments/dev.toml",
        "acme.axon/release/policy.toml",
    ]:
        assert rel in files, rel
    # nothing outside the controller dir, and no mycel names
    assert all(f.startswith("acme.axon/") for f in files)
    assert not any("mycel" in f for f in files)
    assert mode(cluster.infra / "scripts" / "provision-keycloak.sh") & 0o111


def test_cluster_new_descriptor(cluster):
    d = cluster.toml()
    assert d["build_type"] == "cluster"
    assert (d["name"], d["network"], d["domain"]) == ("acme", "acme-net", "lvh.me")
    assert "core_image" not in d
    assert set(d["fabric"]) == {"postgres", "temporal", "keycloak", "traefik", "tracing", "iam"}
    assert d["fabric"]["keycloak"]["secret"] == "${OIDC_CLIENT_SECRET}"
    assert d["fabric"]["iam"]["signing_key"] == ".axon.infra/secrets/iam_signing_key.pem"
    assert d["fabric"]["temporal"]["shards"] == 128
    assert "app" not in d
    # the descriptor carries its own explanations
    assert cluster.text().startswith("# axonfile — the CLUSTER descriptor.")


def test_cluster_new_allocates_six_ports_into_registry_and_descriptor(cluster):
    reg = cluster.registry()
    assert list(reg) == ["acme"]
    ports = reg["acme"]
    assert len(ports) == 6 and len(set(ports)) == 6
    pg, traefik, temporal, temporal_ui, grafana, clickhouse = ports
    for p, start in zip(ports, [5545, 8090, 7233, 8233, 3000, 8123]):
        assert start <= p < start + 400
    d = cluster.toml()["fabric"]
    assert d["postgres"]["port"] == pg
    assert d["traefik"]["http"] == traefik
    assert (d["temporal"]["port"], d["temporal"]["ui_port"]) == (temporal, temporal_ui)
    assert d["tracing"]["grafana_port"] == grafana
    assert "# http_port = %d" % clickhouse in cluster.text()  # the commented-out clickhouse table
    registry_file = cluster.xdg / "axon" / "ports.toml"
    text = read(registry_file)
    assert text.startswith("# Host ports allocated per cluster by axon.")
    assert "#   <cluster> = [postgres, traefik, temporal, temporal-ui, grafana, clickhouse]" in text
    assert mode(registry_file) == 0o600


def test_cluster_new_prints_the_allocation(axon, tmp_path, xdg):
    r = axon("cluster", "new", "acme", cwd=tmp_path)
    p = ports_registry(xdg / "axon" / "ports.toml")["acme"]
    line = "allocated free host ports — postgres %d · traefik %d · temporal %d/%d · grafana %d · clickhouse %d" % tuple(p)
    assert line in r.err
    assert "acme" in read(tmp_path / "acme" / "acme.axon" / "RUNBOOK.md")


def test_second_cluster_gets_different_ports(new_cluster):
    a = new_cluster("one")
    b = new_cluster("two")
    reg = a.registry()
    assert set(reg) == {"one", "two"}
    assert not set(reg["one"]) & set(reg["two"])
    assert b.toml()["fabric"]["postgres"]["port"] == reg["two"][0]


def test_same_cluster_name_reuses_its_ports(axon, tmp_path, xdg):
    (tmp_path / "x").mkdir()
    (tmp_path / "y").mkdir()
    assert axon("cluster", "new", "acme", cwd=tmp_path / "x").code == 0
    first = ports_registry(xdg / "axon" / "ports.toml")["acme"]
    assert axon("cluster", "new", "acme", cwd=tmp_path / "y").code == 0
    assert ports_registry(xdg / "axon" / "ports.toml") == {"acme": first}
    assert load_toml(tmp_path / "y" / "acme" / "acme.axon" / ".axon.infra" / "axonfile")["fabric"]["postgres"]["port"] == first[0]


def test_cluster_new_env_has_generated_secrets(cluster):
    env = read_dotenv(cluster.dotenv)
    for k in GENERATED_SECRETS:
        assert len(env[k]) >= 24 and re.match(r"^[A-Za-z0-9]+$", env[k]), k
    assert len({env[k] for k in GENERATED_SECRETS}) == 4
    assert env["SEED_ADMIN_EMAIL"] == "admin@acme.local"
    assert mode(cluster.dotenv) == 0o600
    # the example lists the keys without values
    example = read(cluster.infra / ".env.example")
    for k in GENERATED_SECRETS:
        assert "%s=\n" % k in example


def test_secrets_differ_between_clusters(new_cluster):
    a, b = new_cluster("one"), new_cluster("two")
    ea, eb = read_dotenv(a.dotenv), read_dotenv(b.dotenv)
    assert all(ea[k] != eb[k] for k in GENERATED_SECRETS)
    assert read(a.infra / "secrets" / "iam_signing_key.pem") != read(b.infra / "secrets" / "iam_signing_key.pem")


def _der(pem_text):
    body = "".join(l for l in pem_text.splitlines() if l and not l.startswith("-----"))
    return base64.b64decode(body)


def test_signing_key_is_a_p256_pkcs8_key_with_mode_0600(cluster):
    key = cluster.infra / "secrets" / "iam_signing_key.pem"
    assert mode(key) == 0o600
    text = read(key)
    assert text.startswith("-----BEGIN PRIVATE KEY-----\n") and text.rstrip().endswith("-----END PRIVATE KEY-----")
    der = _der(text)
    assert bytes.fromhex("06072a8648ce3d0201") in der  # id-ecPublicKey
    assert bytes.fromhex("06082a8648ce3d030107") in der  # prime256v1 (P-256)
    if shutil.which("openssl"):
        p = subprocess.run(["openssl", "pkey", "-in", str(key), "-noout", "-text"],
                           stdout=subprocess.PIPE, stderr=subprocess.PIPE, universal_newlines=True)
        assert p.returncode == 0, p.stderr
        assert "P-256" in p.stdout or "prime256v1" in p.stdout


def test_dev_environment_and_release_policy(cluster):
    dev = load_toml(cluster.envs / "dev.toml")
    assert dev == {"name": "dev", "tier": "dev", "namespace": "acme-dev", "release": "local", "target": "docker"}
    assert load_toml(cluster.ctrl / "release" / "policy.toml") == {"series": "0.1", "first_patch": 0}


def test_gitignore_keeps_secrets_and_generated_files_out_of_git(cluster, git, axon):
    git(cluster.ctrl, "init", "-q")
    status = git(cluster.ctrl, "status", "--porcelain", "--untracked-files=all")
    assert ".env\n" not in status + "\n" and "iam_signing_key.pem" not in status
    assert "fabric/" not in status
    assert ".axon.infra/.env.example" in status  # the example is meant to be committed
    r = axon("cluster", "check", "--json", cwd=cluster.ctrl)
    assert r.code == 0 and json.loads(r.out) == {"findings": []}


def test_fresh_cluster_checks_clean(axon, cluster):
    r = axon("cluster", "check", cwd=cluster.ctrl)
    assert (r.code, r.out) == (0, "✓ 0 blocking · 0 risk · 0 note\n")


def test_cluster_new_next_hints(axon, tmp_path):
    r = axon("cluster", "new", "acme", cwd=tmp_path)
    assert r.out.splitlines() == [
        "next:",
        "  cd acme/acme.axon",
        "  axon cluster set core_image=<registry>/<core image>:<tag>",
        "  axon cluster add app --name <app> --from <path> --host <host> --db <db>",
        "  axon up --build",
    ]


def test_cluster_new_core_image(axon, tmp_path):
    r = axon("cluster", "new", "acme", "--core-image", "registry.example/core:1.2", cwd=tmp_path)
    assert r.code == 0
    assert "core_image=" not in r.out
    assert load_toml(tmp_path / "acme" / "acme.axon" / ".axon.infra" / "axonfile")["core_image"] == "registry.example/core:1.2"


def test_cluster_new_name_is_derived_from_the_dir(axon, tmp_path, xdg):
    r = axon("cluster", "new", "My.Cluster_X", cwd=tmp_path)
    assert r.code == 0, r.err
    c = Cluster(tmp_path / "My.Cluster_X", "my-cluster-x", xdg=xdg)
    assert c.toml()["name"] == "my-cluster-x"
    assert "my-cluster-x" in c.registry()


def test_cluster_new_explicit_name(axon, tmp_path, xdg):
    assert axon("cluster", "new", "ws", "--name", "billing", cwd=tmp_path).code == 0
    c = Cluster(tmp_path / "ws", "billing", xdg=xdg)
    assert c.descriptor.is_file() and c.toml()["name"] == "billing"


@pytest.mark.parametrize("argv,name", [
    (["bad name"], "bad name"),
    (["ok", "--name", "Bad_Name"], "Bad_Name"),
    (["ok", "--name", "UPPER"], "UPPER"),
    (["ok", "--name=-lead"], "-lead"),
])
def test_cluster_new_rejects_invalid_names(axon, tmp_path, xdg, argv, name):
    r = axon(*(["cluster", "new"] + argv), cwd=tmp_path)
    assert r.code == 2
    assert r.err == 'axon: cluster new: name "%s" is not a DNS label (use --name)\n' % name
    assert os.listdir(str(tmp_path)) == ["xdg"]  # nothing written, not even the registry
    assert not (xdg / "axon").exists()


def test_cluster_new_dry_run_writes_nothing(axon, tmp_path, xdg):
    r = axon("cluster", "new", "acme", "--dry-run", cwd=tmp_path)
    assert r.code == 0
    assert "would create acme.axon/.axon.infra/axonfile" in r.out
    assert "would create acme.axon/environments/dev.toml" in r.out
    assert not (tmp_path / "acme").exists()
    assert not (xdg / "axon" / "ports.toml").exists()


def test_cluster_new_refuses_an_existing_cluster(axon, cluster, xdg):
    before = snapshot(cluster.ctrl)
    reg = cluster.registry()
    r = axon("cluster", "new", "acme", cwd=cluster.workspace.parent)
    assert r.code == 1
    assert r.err.startswith("axon: ") and "already exists" in r.err
    assert snapshot(cluster.ctrl) == before
    assert cluster.registry() == reg


def test_cluster_new_compat_mycel(axon, new_cluster):
    c = new_cluster("myc", compat="mycel")
    files = tree(c.workspace)
    assert "myc.mycel/.mycel.infra/mycelfile" in files
    assert "myc.mycel/.mycel.infra/secrets/iam_signing_key.pem" in files
    assert "myc.mycel/.mycel.infra/fabric/docker-compose.yml" in files
    assert not any(".axon" in f or "axonfile" in f for f in files)
    gi = read(c.ctrl / ".gitignore")
    assert ".mycel.infra/.env" in gi and ".mycel.infra/.mycel/" in gi
    assert c.toml()["fabric"]["iam"]["signing_key"] == ".mycel.infra/secrets/iam_signing_key.pem"
    # axon reads what it scaffolded (in auto mode), reporting the compat layout
    r = axon("cluster", "check", "--json", cwd=c.ctrl)
    assert r.code == 0
    assert "COMPAT_MYCEL_LAYOUT" in [f["id"] for f in json.loads(r.out)["findings"]]


# ── app new ──────────────────────────────────────────────────────────────────

GO_MODULE_FILES = [
    "module/axonfile", "module/Dockerfile", "module/.dockerignore", "module/README.md",
    "module/database/schema.sql", "module/database/migrations/0001_init.sql",
    "module/service/go.mod", "module/service/main.go", "module/service/logic/logic.go",
    "module/service/logic/logic_test.go", "module/service/logic/openapi.json",
    "module/ui/package.json", "module/ui/vite.config.ts", "module/ui/src/App.svelte", "module/ui/src/entry.ts",
]


def test_app_new_go_without_rings(axon, tmp_path):
    r = axon("app", "new", "acme.policy-app", "--rings", "none", cwd=tmp_path)
    assert r.code == 0, r.err
    app = tmp_path / "acme.policy-app"
    files = [f for f in tree(app) if not f.endswith("go.sum")]
    expected = ["README.md", ".gitignore", "axonfile"] + ["modules/acme_policy_app/" + f for f in GO_MODULE_FILES]
    assert sorted(files) == sorted(expected)
    d = load_toml(app / "axonfile")
    assert d["build_type"] == "app"
    assert d["app"] == {"name": "acme_policy_app_app", "title": "Acme Policy App", "search_placeholder": "Search Acme Policy App…"}
    assert d["compose"] == [{"source": "modules/acme_policy_app/module"}]
    m = load_toml(app / "modules" / "acme_policy_app" / "module" / "axonfile")
    assert m["build_type"] == "module"
    assert m["module"] == {
        "token": "acme_policy_app", "language": "go", "element": "acme-policy-app-app", "ui_entry": "ui/dist/entry.js",
        "label": "Acme Policy App (Go)", "backend": "unix:///tmp/mycel_app_core/acme_policy_app.sock",
    }
    assert 'app "acme_policy_app_app" scaffolded' in r.err
    assert r.out.startswith("next:\n")
    assert "axon cluster add app --name acme.policy-app --from %s --host acme-policy-app" % app.resolve() in r.out \
        or "axon cluster add app --name acme.policy-app --from %s --host acme-policy-app" % app in r.out


def test_app_new_generated_json_files_are_valid(axon, tmp_path):
    assert axon("app", "new", "shop", "--rings", "none", cwd=tmp_path).code == 0
    mod = tmp_path / "shop" / "modules" / "shop" / "module"
    api = json.loads(read(mod / "service" / "logic" / "openapi.json"))
    assert "/m/shop/hello" in api["paths"]
    pkg = json.loads(read(mod / "ui" / "package.json"))
    assert pkg["name"] == "@app-mod/shop" and pkg["scripts"]["build"] == "vite build"
    assert "module shop_service" in read(mod / "service" / "go.mod")
    assert "CREATE SCHEMA IF NOT EXISTS shop;" in read(mod / "database" / "schema.sql")


def test_app_new_rust_with_all_rings(axon, tmp_path):
    r = axon("app", "new", "ledger", "--lang", "rust", cwd=tmp_path)
    assert r.code == 0, r.err
    base = tmp_path / "ledger" / "modules" / "ledger"
    files = tree(base)
    for rel in ["module/service/Cargo.toml", "module/service/src/main.rs", "module/Dockerfile",
                "module_test/README.md", "docs/README.md", "docs/DESIGN.md", "docs/overview.md"]:
        assert rel in files, rel
    assert not any(f.endswith(".go") or f.endswith("go.mod") for f in files)
    m = load_toml(base / "module" / "axonfile")["module"]
    assert (m["language"], m["label"]) == ("rust", "Ledger (Rust)")
    assert 'name = "ledger"' in read(base / "module" / "service" / "Cargo.toml")


def test_app_new_go_test_ring_only(axon, tmp_path):
    assert axon("app", "new", "shop", "--rings", "test", cwd=tmp_path).code == 0
    files = tree(tmp_path / "shop" / "modules" / "shop")
    assert "module_test/go.mod" in files and "module_test/service/api_test.go" in files
    assert not any(f.startswith("docs/") for f in files)


def test_app_new_overrides(axon, tmp_path):
    r = axon("app", "new", "ws-app", "--name", "custom_app", "--title", "Custom Title", "--token", "billing",
             "--label", "Billing Engine", "--rings", "none", cwd=tmp_path)
    assert r.code == 0, r.err
    d = load_toml(tmp_path / "ws-app" / "axonfile")
    assert d["app"]["name"] == "custom_app" and d["app"]["title"] == "Custom Title"
    assert d["compose"] == [{"source": "modules/billing/module"}]
    m = load_toml(tmp_path / "ws-app" / "modules" / "billing" / "module" / "axonfile")["module"]
    assert (m["token"], m["label"], m["element"]) == ("billing", "Billing Engine", "billing-app")


def test_app_new_module_flag_sets_the_token(axon, tmp_path):
    assert axon("app", "new", "store", "--module", "Price-List", "--rings", "none", cwd=tmp_path).code == 0
    d = load_toml(tmp_path / "store" / "axonfile")
    assert d["compose"] == [{"source": "modules/price_list/module"}]
    assert d["app"]["name"] == "store_app"


def test_app_new_rejects_unknown_language(axon, tmp_path):
    r = axon("app", "new", "shop", "--lang", "java", cwd=tmp_path)
    assert (r.code, r.err) == (2, "axon: app new: --lang must be go or rust\n")
    assert not (tmp_path / "shop").exists()


def test_app_new_project(axon, tmp_path):
    r = axon("app", "new", "jobs", "--project", cwd=tmp_path)
    assert r.code == 0, r.err
    assert sorted(tree(tmp_path / "jobs")) == ["Dockerfile", "axonfile"]
    d = load_toml(tmp_path / "jobs" / "axonfile")
    assert d["build_type"] == "project"
    assert d["project"]["dockerfile"] == "Dockerfile" and len(d["project"]["build"]) >= 1
    r = axon("app", "check", cwd=tmp_path / "jobs")
    assert r.code == 0 and "0 blocking" in r.out


def test_app_new_refuses_existing_descriptor(axon, tmp_path):
    assert axon("app", "new", "shop", "--rings", "none", cwd=tmp_path).code == 0
    before = snapshot(tmp_path / "shop")
    r = axon("app", "new", "shop", cwd=tmp_path)
    assert r.code == 1 and "already holds an app descriptor" in r.err
    assert snapshot(tmp_path / "shop") == before


def test_app_new_compat_mycel_names(axon, tmp_path):
    assert axon("--compat", "mycel", "app", "new", "shop", "--rings", "none", cwd=tmp_path).code == 0
    files = tree(tmp_path / "shop")
    assert "mycelfile" in files and "modules/shop/module/mycelfile" in files
    assert not any("axonfile" in f for f in files)
    assert ".mycel.infra/.mycel/" in read(tmp_path / "shop" / ".gitignore")


def test_new_app_checks_clean(axon, tmp_path):
    assert axon("app", "new", "shop", "--rings", "none", cwd=tmp_path).code == 0
    r = axon("app", "check", cwd=tmp_path / "shop")
    assert r.code == 0
    assert "✓ 0 blocking · 0 risk · 0 note" in r.out
