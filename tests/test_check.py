"""`cluster check`: descriptor validation rules, the report format and exit codes."""

import json
import os

import pytest

from _util import read, write


def check(axon, cluster):
    r = axon("cluster", "check", "--json", cwd=cluster.ctrl)
    return r.code, {f["id"]: f for f in json.loads(r.out)["findings"]}


def append(cluster, text):
    write(cluster.descriptor, cluster.text() + "\n" + text.strip() + "\n")


def replace(cluster, old, new):
    text = cluster.text()
    assert old in text, old
    write(cluster.descriptor, text.replace(old, new, 1))


@pytest.mark.parametrize("edit,fid,severity", [
    (lambda c: append(c, '[[app]]\nname = "ghost"\nhost = "ghost"'), "CONFORMANCE_APP_NOT_PRESENT", "blocking"),
    (lambda c: append(c, '[[app]]\nname = "w"\nkind = "weird"\nimage = "x:1"'), "CONFORMANCE_APP_KIND", "blocking"),
    (lambda c: append(c, '[[app]]\nname = "nohost"'), "CONFORMANCE_APP_HOST_MISSING", "blocking"),
    (lambda c: append(c, '[[app]]\nname = "h"\nhost = "Bad_Host"'), "CONFORMANCE_APP_HOST_NOT_DNS_SAFE", "blocking"),
    (lambda c: append(c, '[[app]]\nname = "dup"\nkind = "project"\nimage = "x:1"\n\n[[app]]\nname = "dup"\nkind = "project"\nimage = "x:1"'),
     "CONFORMANCE_DUPLICATE_APP", "blocking"),
    (lambda c: replace(c, 'name    = "acme"', 'name    = "Bad_Name"'), "CONFORMANCE_NAME_NOT_DNS_SAFE", "blocking"),
    (lambda c: replace(c, "[fabric.postgres]", "[fabric.postgres_old]"), "CONFORMANCE_POSTGRES_MISSING", "blocking"),
    (lambda c: replace(c, "[fabric.traefik]", "# [fabric.traefik]\n[fabric.traefik_old]"), "CONFORMANCE_TRAEFIK_MISSING", "risk"),
    (lambda c: replace(c, "shards  = 128", "# shards unset"), "CONFORMANCE_TEMPORAL_SHARDS_DEFAULT", "note"),
    (lambda c: append(c, '[fabric.kpi]\nbackend = "kpi_svc:7373"'), "CONFORMANCE_KPI_BACKEND_NOT_DNS_SAFE", "blocking"),
    (lambda c: replace(c, 'domain  = "lvh.me"', 'domain  = "lvh.me"\ncolour  = "blue"'), "CONFORMANCE_UNKNOWN_KEY", "note"),
    (lambda c: replace(c, "# core_image =", 'core_image = "registry.example/core:2"\n# core_image ='), "SECURITY_UNPINNED_CORE_IMAGE", "note"),
])
def test_validation_rules(axon, cluster, edit, fid, severity):
    edit(cluster)
    code, fs = check(axon, cluster)
    assert fid in fs, sorted(fs)
    assert fs[fid]["severity"] == severity
    assert code == (1 if any(f["severity"] == "blocking" for f in fs.values()) else 0)


def test_temporal_worker_without_temporal_fabric(axon, cluster):
    assert axon("cluster", "remove", "fabric", "temporal", cwd=cluster.ctrl).code == 0
    append(cluster, '[[app]]\nname = "w"\nkind = "project"\nimage = "x:1"\ntemporal = "temporal:7233"')
    code, fs = check(axon, cluster)
    assert code == 1 and fs["CONFORMANCE_TEMPORAL_MISSING"]["subject"] == "app w"


def test_iam_app_without_iam_contract(axon, cluster):
    assert axon("cluster", "remove", "fabric", "iam", cwd=cluster.ctrl).code == 0
    assert axon("cluster", "add", "app", "--name", "iam", "--kind", "iam", "--from-image", "reg.example/iam:1",
                "--token", "mycel_iam", cwd=cluster.ctrl).code == 0
    code, fs = check(axon, cluster)
    assert fs["CONFORMANCE_IAM_CONTRACT_MISSING"]["severity"] == "risk"


def test_duplicate_hosts_and_tokens(axon, cluster):
    assert axon("cluster", "add", "app", "--name", "a", "--from-image", "reg.example/shared:1", "--host", "same", cwd=cluster.ctrl).code == 0
    assert axon("cluster", "add", "app", "--name", "b", "--from-image", "reg.example/shared:2", "--host", "same", cwd=cluster.ctrl).code == 0
    code, fs = check(axon, cluster)
    assert code == 1
    assert fs["CONFORMANCE_DUPLICATE_HOST"]["cause"] == 'host "same" is also used by a'
    assert "composed by both a and b" in fs["CONFORMANCE_DUPLICATE_TOKEN"]["cause"]


def test_app_without_modules(axon, cluster):
    assert axon("cluster", "add", "app", "--name", "web", "--from-image", "reg.example/w:1", cwd=cluster.ctrl).code == 0
    write(cluster.workspace / "web" / "axonfile", 'build_type = "app"\n[app]\nname = "web"\n')
    code, fs = check(axon, cluster)
    assert fs["CONFORMANCE_APP_EMPTY"]["severity"] == "risk"


def test_project_app_needs_a_build_step(axon, cluster):
    write(cluster.workspace / "job" / "axonfile", 'build_type = "project"\n[app]\nname = "job"\n')
    append(cluster, '[[app]]\nname = "job"\nkind = "project"')
    code, fs = check(axon, cluster)
    assert code == 1 and fs["CONFORMANCE_PROJECT_BUILD_MISSING"]["subject"] == "app job"
    assert fs["PROJECT_BUILD_MISSING"]["subject"] == "job [project]"  # the member descriptor is validated too


def test_missing_source_module(axon, cluster, tmp_path):
    assert axon("app", "new", str(cluster.workspace / "shop"), "--rings", "none", cwd=tmp_path).code == 0
    assert axon("cluster", "add", "app", "--name", "shop", "--from", "../shop", cwd=cluster.ctrl).code == 0
    import shutil
    shutil.rmtree(str(cluster.workspace / "shop" / "modules" / "shop" / "module"))
    code, fs = check(axon, cluster)
    assert code == 1 and fs["CONFORMANCE_MODULE_MISSING"]["subject"] == "module shop"


def test_literal_secret_in_app_env(axon, cluster):
    assert axon("cluster", "add", "app", "--name", "web", "--from-image", "reg.example/w:1",
                "--env", "API_TOKEN=plaintext,SAFE_TOKEN=${SAFE_TOKEN},LOG_LEVEL=debug", cwd=cluster.ctrl).code == 0
    code, fs = check(axon, cluster)
    f = fs["SECURITY_SECRETS_IN_ENV"]
    assert f["severity"] == "risk" and "API_TOKEN" in f["cause"] and f["subject"] == "app web"


def test_keycloak_admin_password_default(axon, cluster):
    env = read(cluster.dotenv)
    write(cluster.dotenv, "\n".join(l for l in env.splitlines() if not l.startswith("KEYCLOAK_ADMIN_PASSWORD")) + "\n")
    code, fs = check(axon, cluster)
    assert code == 0 and fs["SECURITY_ADMIN_PASSWORD"]["severity"] == "note"


def test_dotenv_not_ignored_by_git(axon, cluster, git):
    write(cluster.ctrl / ".gitignore", "# emptied\n")
    git(cluster.ctrl, "init", "-q")
    code, fs = check(axon, cluster)
    assert code == 1
    assert fs["SECURITY_SIGNING_KEY_NOT_IGNORED"]["severity"] == "blocking"
    assert fs["SECURITY_SECRETS_IN_ENV"]["subject"] == ".axon.infra/.env"


def test_journey_coverage_note(axon, web_cluster):
    os.remove(str(web_cluster.journeys / "web_smoke.toml"))
    code, fs = check(axon, web_cluster)
    assert code == 0
    assert fs["DEVX_NO_JOURNEY_COVERAGE"]["fix"] == "axon cluster generate (scaffolds journeys/web_smoke.toml)"


def test_report_format(axon, cluster):
    append(cluster, '[[app]]\nname = "ghost"\nhost = "ghost"')
    replace(cluster, "shards  = 128", "# shards unset")
    r = axon("cluster", "check", cwd=cluster.ctrl)
    assert r.code == 1 and r.err == ""
    lines = r.out.splitlines()
    # ghost app (blocking), drift from the shards edit (risk), shards default + no journey (notes)
    assert lines[0] == "✗ 1 blocking · 1 risk · 2 note"
    assert "CONFORMANCE" in lines and "DEVX" in lines
    assert lines.index("CONFORMANCE") < lines.index("DEVX")  # concerns in report order
    assert any(l.startswith("  ✗ CONFORMANCE_APP_NOT_PRESENT  app ghost — app dir ") for l in lines)
    assert "      repair: clone or link the app next to the cluster dir" in lines
    assert any(l.startswith("  · CONFORMANCE_TEMPORAL_SHARDS_DEFAULT  fabric.temporal — ") for l in lines)
    # within a concern: blocking, then risk, then notes
    assert r.out.index("✗ CONFORMANCE_APP_NOT_PRESENT") < r.out.index("! CONFORMANCE_CONFIG_DRIFT") \
        < r.out.index("· CONFORMANCE_TEMPORAL_SHARDS_DEFAULT")
