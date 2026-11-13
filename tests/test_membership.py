"""Membership mutations: cluster add/remove app|ext|fabric|product, cluster set, and the
comment-preserving descriptor edits behind them."""

import os

import pytest

from _util import load_toml, load_yaml, loads_toml, read, snapshot, tree, write

CUSTOM_COMMENT = "# NOTE(team): keep this line — hand-written comments must survive every edit\n"


def app_names(cluster):
    return [a["name"] for a in cluster.toml().get("app", [])]


# ── cluster add app ──────────────────────────────────────────────────────────


def test_add_app_from_image(axon, cluster):
    r = axon("cluster", "add", "app", "--name", "web", "--from-image", "ghcr.io/example/web-mod:1.4", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    assert r.out == ""
    assert '✓ added app "web" to acme' in r.err
    assert cluster.toml()["app"] == [{"name": "web", "host": "web", "db": "web"}]
    app = cluster.workspace / "web"
    assert sorted(tree(app)) == ["axonfile", "modules/.gitkeep"]
    d = load_toml(app / "axonfile")
    assert d["build_type"] == "app" and d["app"]["name"] == "web"
    assert d["compose"] == [{"source": "modules/web_mod/module", "image": "ghcr.io/example/web-mod:1.4",
                             "token": "web_mod", "db": True}]
    assert cluster.app_compose("web").is_file()
    assert (cluster.journeys / "web_smoke.toml").is_file()
    assert axon("cluster", "check", cwd=cluster.ctrl).code == 0


def test_add_app_options(axon, cluster):
    r = axon("cluster", "add", "app", "--name", "acme.portal", "--from-image", "reg.example/portal-ui@sha256:" + "b" * 64,
             "--host", "portal", "--db", "portal_db", "--token", "portal", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    assert cluster.toml()["app"] == [{"name": "acme.portal", "host": "portal", "db": "portal_db"}]
    compose = load_toml(cluster.workspace / "acme.portal" / "axonfile")["compose"][0]
    assert compose["token"] == "portal"


def test_add_app_defaults_host_and_db_from_a_dotted_name(axon, cluster):
    assert axon("cluster", "add", "app", "--name", "acme.Web_UI", "--from-image", "reg.example/x:1", cwd=cluster.ctrl).code == 0
    assert cluster.toml()["app"] == [{"name": "acme.Web_UI", "host": "web-ui", "db": "acme_web_ui"}]


def test_add_app_stateless(axon, cluster):
    assert axon("cluster", "add", "app", "--name", "web", "--from-image", "reg.example/w:1", "--stateless", cwd=cluster.ctrl).code == 0
    assert cluster.toml()["app"] == [{"name": "web", "host": "web"}]
    assert load_toml(cluster.workspace / "web" / "axonfile")["compose"][0]["db"] is False


def test_add_app_env_values(axon, cluster):
    r = axon("cluster", "add", "app", "--name", "web", "--from-image", "reg.example/w:1",
             "--env", "B=2,A=1", "--env", "TOKEN=${WEB_TOKEN}", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    assert cluster.toml()["app"][0]["env"] == {"A": "1", "B": "2", "TOKEN": "${WEB_TOKEN}"}
    # keys are written sorted under [app.env]
    text = cluster.text()
    assert text.index('A = "1"') < text.index('B = "2"') < text.index('TOKEN = "${WEB_TOKEN}"')


def test_add_app_rejects_malformed_env(axon, cluster):
    before = cluster.text()
    r = axon("cluster", "add", "app", "--name", "web", "--from-image", "reg.example/w:1", "--env", "NOEQUALS", cwd=cluster.ctrl)
    assert r.code == 2
    assert r.err.endswith('axon: cluster add app: --env wants K=V, got "NOEQUALS"\n')
    assert cluster.text() == before


def test_add_app_usage_error_has_no_side_effects(axon, cluster):
    before = snapshot(cluster.workspace)
    r = axon("cluster", "add", "app", "--name", "web", "--from-image", "reg.example/w:1", "--env", "NOEQUALS", cwd=cluster.ctrl)
    assert r.code == 2
    assert not (cluster.workspace / "web").exists()
    assert snapshot(cluster.workspace) == before


def test_add_app_from_dir_links_it_into_the_workspace(axon, cluster, tmp_path):
    src = tmp_path / "elsewhere" / "shop-src"
    assert axon("app", "new", str(src), "--rings", "none", cwd=tmp_path).code == 0
    r = axon("cluster", "add", "app", "--name", "shop", "--from", str(src), "--host", "shop", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    link = cluster.workspace / "shop"
    assert link.is_symlink() and os.path.realpath(str(link)) == os.path.realpath(str(src))
    assert "linked shop → " in r.err
    assert load_yaml(cluster.app_compose("shop"))["services"]["core"]["depends_on"] == {"shop_src": {"condition": "service_healthy"}}


def test_add_app_from_dir_already_in_place(axon, cluster):
    src = cluster.workspace / "shop"
    assert axon("app", "new", str(src), "--rings", "none", cwd=cluster.workspace).code == 0
    r = axon("cluster", "add", "app", "--name", "shop", "--from", "../shop", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    assert not src.is_symlink()


def test_add_app_project_kind_override_rejected_for_a_module_source(axon, cluster, tmp_path):
    src = tmp_path / "mod"
    write(src / "axonfile", 'build_type = "module"\n[module]\ntoken = "pricing"\n')
    r = axon("cluster", "add", "app", "--name", "pricing", "--from", str(src), "--kind", "project", cwd=cluster.ctrl)
    assert r.code == 2
    assert "only iam is valid for a build_type=module source" in r.err


def test_add_app_project_with_temporal_requires_temporal_fabric(axon, cluster):
    assert axon("cluster", "remove", "fabric", "temporal", cwd=cluster.ctrl).code == 0
    before = cluster.text()
    r = axon("cluster", "add", "app", "--name", "w", "--kind", "project", "--from-image", "reg.example/w:1",
             "--temporal", "temporal:7233", cwd=cluster.ctrl)
    assert r.code == 1
    assert "[fabric.temporal] must be present" in r.err
    assert cluster.text() == before


@pytest.mark.parametrize("argv,code,msg", [
    (["--from-image", "reg.example/w:1"], 2, "axon: cluster add app: --name is required\n"),
    (["--name", "web"], 2, "axon: cluster add app: --from <dir> is required unless --from-image is given\n"),
    (["--name", "web", "--kind", "weird", "--from-image", "x:1"], 2, "axon: cluster add app: --kind must be module, iam or project\n"),
])
def test_add_app_usage_errors(axon, cluster, argv, code, msg):
    before = snapshot(cluster.workspace)
    r = axon(*(["cluster", "add", "app"] + argv), cwd=cluster.ctrl)
    assert (r.code, r.err) == (code, msg)
    assert snapshot(cluster.workspace) == before


def test_add_app_missing_from_dir(axon, cluster, tmp_path):
    r = axon("cluster", "add", "app", "--name", "web", "--from", str(tmp_path / "nope"), cwd=cluster.ctrl)
    assert r.code == 1 and r.err.startswith("axon: --from ")
    assert "app" not in cluster.toml()


def test_add_app_duplicate(axon, web_cluster):
    before = web_cluster.text()
    r = axon("cluster", "add", "app", "--name", "web", "--from-image", "reg.example/other:1", cwd=web_cluster.ctrl)
    assert (r.code, r.err) == (1, 'axon: app "web" is already a member\n')
    assert web_cluster.text() == before


def test_add_app_dry_run_changes_nothing(axon, cluster):
    before = snapshot(cluster.workspace)
    r = axon("cluster", "add", "app", "--name", "web", "--from-image", "reg.example/w:1", "--dry-run", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    assert '+ name = "web"' in r.out and "+ [[app]]" in r.out
    assert snapshot(cluster.workspace) == before
    assert not (cluster.workspace / "web").exists()


# ── cluster add ext ──────────────────────────────────────────────────────────


def test_add_ext(axon, web_cluster):
    r = axon("cluster", "add", "ext", "--name", "pricing", "--for", "web", "--module", "pricing", "--tenant", "demo", cwd=web_cluster.ctrl)
    assert r.code == 0, r.err
    assert 'set PRICING_EXT_URL = "http://app-pricing:8090" under [app.env] of web' in r.err
    assert {"name": "pricing", "kind": "project", "port": 8090} in web_cluster.toml()["app"]
    assert load_toml(web_cluster.workspace / "pricing" / "axonfile")["build_type"] == "project"
    assert web_cluster.app_compose("pricing").is_file()


@pytest.mark.parametrize("argv,code", [(["--name", "x"], 2), (["--for", "web"], 2), (["--name", "x", "--for", "nosuch"], 1)])
def test_add_ext_errors(axon, web_cluster, argv, code):
    assert axon(*(["cluster", "add", "ext"] + argv), cwd=web_cluster.ctrl).code == code


# ── cluster remove app ───────────────────────────────────────────────────────


def test_remove_app_keeps_its_dir(axon, web_cluster):
    r = axon("cluster", "remove", "app", "web", cwd=web_cluster.ctrl)
    assert r.code == 0, r.err
    assert '✓ removed app "web"' in r.err and "is untouched (pass --purge to delete it)" in r.err
    assert "app" not in web_cluster.toml()
    assert (web_cluster.workspace / "web" / "axonfile").is_file()
    assert "web" not in load_yaml(web_cluster.routes)["http"]["routers"]
    assert axon("cluster", "generate", "--check", cwd=web_cluster.ctrl).code == 0


def test_remove_unknown_app(axon, cluster):
    r = axon("cluster", "remove", "app", "ghost", cwd=cluster.ctrl)
    assert (r.code, r.err) == (1, 'axon: no app "ghost" in the descriptor\n')


def test_remove_app_purge_with_yes_deletes_the_dir(axon, web_cluster):
    r = axon("--yes", "cluster", "remove", "app", "web", "--purge", cwd=web_cluster.ctrl)
    assert r.code == 0, r.err
    assert not (web_cluster.workspace / "web").exists()


def test_remove_app_purge_of_a_link_removes_only_the_link(axon, cluster, tmp_path):
    src = tmp_path / "src" / "shop"
    assert axon("app", "new", str(src), "--rings", "none", cwd=tmp_path).code == 0
    assert axon("cluster", "add", "app", "--name", "shop", "--from", str(src), cwd=cluster.ctrl).code == 0
    r = axon("cluster", "remove", "app", "shop", "--purge", cwd=cluster.ctrl)  # no TTY, no --yes: a link is not data
    assert r.code == 0, r.err
    assert not os.path.lexists(str(cluster.workspace / "shop"))
    assert (src / "axonfile").is_file()


def test_remove_app_purge_refuses_without_confirmation(axon, web_cluster):
    r = axon("cluster", "remove", "app", "web", "--purge", cwd=web_cluster.ctrl)
    assert r.code == 1
    assert "without confirmation (pass --yes)" in r.err
    assert (web_cluster.workspace / "web" / "axonfile").is_file()


def test_refused_purge_leaves_the_cluster_consistent(axon, web_cluster):
    r = axon("cluster", "remove", "app", "web", "--purge", cwd=web_cluster.ctrl)
    assert r.code == 1
    check = axon("cluster", "generate", "--check", cwd=web_cluster.ctrl)
    assert check.code == 0, check.out


# ── comments and ordering survive edits ──────────────────────────────────────


def test_add_and_remove_preserve_comments_and_order(axon, cluster):
    text = cluster.text().replace("[fabric.traefik]\n", CUSTOM_COMMENT + "[fabric.traefik]\n", 1)
    write(cluster.descriptor, text)
    original = cluster.text()
    for argv in (
        ["cluster", "add", "app", "--name", "one", "--from-image", "reg.example/one:1"],
        ["cluster", "add", "app", "--name", "two", "--from-image", "reg.example/two:1", "--env", "K=V"],
        ["cluster", "add", "fabric", "clickhouse"],
        ["cluster", "add", "product", "clc", "--image", "reg.example/clc:1"],
    ):
        assert axon(*argv, cwd=cluster.ctrl).code == 0, argv
    mid = cluster.text()
    assert mid.startswith(original.split("[fabric.postgres]")[0])  # header + comments untouched
    assert CUSTOM_COMMENT in mid
    # fabric is inserted before the first [[app]], members are appended in order
    assert mid.index("[fabric.clickhouse]") < mid.index("[[app]]")
    assert mid.index('name = "one"') < mid.index('name = "two"') < mid.index("[product.clc]")
    for argv in (
        ["cluster", "remove", "app", "one"],
        ["cluster", "remove", "fabric", "clickhouse"],
        ["cluster", "remove", "product", "clc"],
        ["cluster", "remove", "app", "two"],
    ):
        assert axon(*argv, cwd=cluster.ctrl).code == 0, argv
    assert cluster.text().rstrip() == original.rstrip()


def test_remove_app_takes_its_subtables(axon, cluster):
    assert axon("cluster", "add", "app", "--name", "one", "--from-image", "reg.example/one:1", "--env", "A=1", cwd=cluster.ctrl).code == 0
    assert axon("cluster", "add", "app", "--name", "two", "--from-image", "reg.example/two:1", "--env", "B=2", cwd=cluster.ctrl).code == 0
    assert axon("cluster", "remove", "app", "one", cwd=cluster.ctrl).code == 0
    d = cluster.toml()
    assert d["app"] == [{"name": "two", "host": "two", "db": "two", "env": {"B": "2"}}]


# ── cluster add / remove fabric ──────────────────────────────────────────────


def test_add_fabric_clickhouse_before_apps(axon, web_cluster):
    r = axon("cluster", "add", "fabric", "clickhouse", cwd=web_cluster.ctrl)
    assert r.code == 0, r.err
    assert "✓ added [fabric.clickhouse]" in r.err
    text = web_cluster.text()
    assert text.index("[fabric.clickhouse]") < text.index("[[app]]")
    assert web_cluster.toml()["fabric"]["clickhouse"]["volume"] == {"name": "clickhouse-data", "kind": "durable", "mount": "/var/lib/clickhouse"}


def test_add_fabric_clickhouse_with_port(axon, cluster):
    assert axon("cluster", "add", "fabric", "clickhouse", "--port", "18123", cwd=cluster.ctrl).code == 0
    assert cluster.toml()["fabric"]["clickhouse"]["http_port"] == 18123


def test_add_fabric_duplicate(axon, cluster):
    before = cluster.text()
    r = axon("cluster", "add", "fabric", "temporal", cwd=cluster.ctrl)
    assert (r.code, r.err) == (1, "axon: [fabric.temporal] already exists\n")
    assert cluster.text() == before


def test_add_fabric_unknown(axon, cluster):
    r = axon("cluster", "add", "fabric", "redis", cwd=cluster.ctrl)
    assert r.code == 2
    assert r.err == ('axon: cluster add fabric: unknown fabric "redis" '
                     "(clickhouse, iam, keycloak, kpi, postgres, temporal, tracing, traefik)\n")


def test_add_fabric_port_only_for_clickhouse(axon, cluster):
    r = axon("cluster", "add", "fabric", "kpi", "--port", "1", cwd=cluster.ctrl)
    assert (r.code, r.err) == (2, "axon: cluster add fabric: --port applies only to clickhouse\n")


def test_add_removed_fabric_back(axon, cluster):
    assert axon("cluster", "remove", "fabric", "tracing", cwd=cluster.ctrl).code == 0
    assert "tracing" not in cluster.toml()["fabric"]
    assert axon("cluster", "add", "fabric", "tracing", cwd=cluster.ctrl).code == 0
    assert cluster.toml()["fabric"]["tracing"]["tempo_image"] == "grafana/tempo:2.6.1"


def test_remove_fabric_postgres_is_refused(axon, cluster):
    before = cluster.text()
    r = axon("cluster", "remove", "fabric", "postgres", cwd=cluster.ctrl)
    assert (r.code, r.err) == (1, "axon: [fabric.postgres] is required\n")
    assert cluster.text() == before


def test_remove_unknown_fabric(axon, cluster):
    r = axon("cluster", "remove", "fabric", "clickhouse", cwd=cluster.ctrl)
    assert (r.code, r.err) == (1, 'axon: no fabric "clickhouse" in the descriptor\n')


# ── products ─────────────────────────────────────────────────────────────────


def test_add_and_remove_product(axon, cluster):
    r = axon("cluster", "add", "product", "clc", "--image", "reg.example/clc:1", "--host", "clc", "--mode", "full", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    assert cluster.toml()["product"] == {"clc": {"image": "reg.example/clc:1", "host": "clc", "mode": "full"}}
    r = axon("cluster", "add", "product", "clc", cwd=cluster.ctrl)
    assert (r.code, r.err) == (1, "axon: [product.clc] already exists\n")
    assert axon("cluster", "remove", "product", "clc", cwd=cluster.ctrl).code == 0
    assert "product" not in cluster.toml()
    r = axon("cluster", "remove", "product", "clc", cwd=cluster.ctrl)
    assert (r.code, r.err) == (1, 'axon: no product "clc" in the descriptor\n')


# ── cluster set ──────────────────────────────────────────────────────────────


def test_set_bare_keys_print_values(axon, cluster):
    pg = cluster.toml()["fabric"]["postgres"]["port"]
    r = axon("cluster", "set", "name", "domain", "network", "fabric.postgres.port", "fabric.keycloak.realm", cwd=cluster.ctrl)
    assert (r.code, r.err) == (0, "")
    assert r.out == "acme\nlvh.me\nacme-net\n%d\nacme\n" % pg


def test_set_values_keep_trailing_comments(axon, cluster):
    r = axon("cluster", "set", "domain=example.test", "fabric.postgres.port=16000", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    assert "✓ updated .axon.infra/axonfile" in r.err
    d = cluster.toml()
    assert d["domain"] == "example.test" and d["fabric"]["postgres"]["port"] == 16000
    text = cluster.text()
    assert 'domain  = "example.test"  # apps answer at <host>.<domain>' in text
    assert "port  = 16000  # host port" in text
    # regenerated right away
    assert "127.0.0.1:16000:5432" in read(cluster.fabric_compose)


def test_set_quotes_string_keys_and_encodes_scalars(axon, cluster):
    assert axon("cluster", "set", "core_image=1234", "fabric.temporal.shards=64", "fabric.traefik.image=traefik:v3.2",
                cwd=cluster.ctrl).code == 0
    d = cluster.toml()
    assert d["core_image"] == "1234"  # a string key stays a string even when it looks numeric
    assert d["fabric"]["temporal"]["shards"] == 64
    assert d["fabric"]["traefik"]["image"] == "traefik:v3.2"


def test_set_new_key_in_a_missing_table(axon, cluster):
    assert axon("cluster", "set", "fabric.kpi.backend=kpi:7474", cwd=cluster.ctrl).code == 0
    assert cluster.toml()["fabric"]["kpi"] == {"backend": "kpi:7474"}


def test_set_app_keys(axon, web_cluster):
    r = axon("cluster", "set", "app.web.replicas=3", cwd=web_cluster.ctrl)
    assert r.code == 0, r.err
    assert web_cluster.toml()["app"][0]["replicas"] == 3
    assert axon("cluster", "set", "app.web.host=shop", cwd=web_cluster.ctrl).code == 0
    assert web_cluster.toml()["app"][0]["host"] == "shop"
    r = axon("cluster", "set", "app.web.host", "app.web.db", "app.web.kind", cwd=web_cluster.ctrl)
    assert r.out == "shop\nweb\nmodule\n"
    assert load_yaml(web_cluster.routes)["http"]["routers"]["shop"]["rule"] == "Host(`shop.lvh.me`)"


def test_set_app_env_key_round_trips(axon, cluster):
    assert axon("cluster", "add", "app", "--name", "web", "--from-image", "reg.example/w:1", "--env", "LOG_LEVEL=info",
                cwd=cluster.ctrl).code == 0
    assert axon("cluster", "set", "app.web.LOG_LEVEL", cwd=cluster.ctrl).out == "info\n"
    assert axon("cluster", "set", "app.web.LOG_LEVEL=debug", cwd=cluster.ctrl).code == 0
    assert axon("cluster", "set", "app.web.LOG_LEVEL", cwd=cluster.ctrl).out == "debug\n"
    assert "CONFORMANCE_UNKNOWN_KEY" not in axon("cluster", "check", cwd=cluster.ctrl).out


def test_set_app_key_errors(axon, web_cluster):
    r = axon("cluster", "set", "app.ghost.host=x", cwd=web_cluster.ctrl)
    assert (r.code, r.err) == (1, 'axon: no [[app]] named "ghost"\n')
    r = axon("cluster", "set", "app.bad=1", cwd=web_cluster.ctrl)
    assert (r.code, r.err) == (2, "axon: cluster set: app keys look like app.<name>.<key>\n")


def test_set_check_changes_nothing(axon, cluster):
    before = snapshot(cluster.ctrl)
    r = axon("cluster", "set", "--check", "name=other", "fabric.postgres.port=1", cwd=cluster.ctrl)
    assert r.code == 0
    assert r.out == "would set name = other\nwould set fabric.postgres.port = 1\n"
    assert snapshot(cluster.ctrl) == before


@pytest.mark.parametrize("kv,why", [
    ("fabric.postgres.port=notanumber", "incompatible types"),
    ("build_type=app", 'build_type is "app", want "cluster"'),
])
def test_set_reverts_an_invalid_edit(axon, cluster, kv, why):
    before = snapshot(cluster.ctrl)
    r = axon("cluster", "set", kv, cwd=cluster.ctrl)
    assert r.code == 1
    assert "the edit would make the descriptor invalid (reverted)" in r.err and why in r.err
    assert snapshot(cluster.ctrl) == before  # no temp file left behind either


def test_set_name_renames_the_compose_project(axon, cluster):
    assert axon("cluster", "set", "name=billing", cwd=cluster.ctrl).code == 0
    assert load_yaml(cluster.fabric_compose)["name"] == "billing"
    assert loads_toml(cluster.text())["name"] == "billing"


def test_set_mixed_app_and_cluster_keys_applies_all(axon, web_cluster):
    r = axon("cluster", "set", "domain=first.test", "app.web.replicas=2", "network=after-net", cwd=web_cluster.ctrl)
    assert r.code == 0, r.err
    d = web_cluster.toml()
    assert d["app"][0]["replicas"] == 2
    assert (d["domain"], d["network"]) == ("first.test", "after-net")
