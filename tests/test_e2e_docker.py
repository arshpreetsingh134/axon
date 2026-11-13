"""End-to-end runs against a real Docker daemon. Marked `docker`, so deselected by default:

    AXON_BIN=$PWD/bin/axon .venv/bin/pytest -m docker

They pull small public images (postgres, traefik, traefik/whoami, alpine), start containers
on free host ports under a throw-away cluster name, and always tear everything down
(`down --volumes --yes`) at the end.
"""

import json
import os
import shutil
import subprocess
import uuid

import pytest

from _util import load_toml, write

pytestmark = pytest.mark.docker

UP_TIMEOUT = 1200


def docker_ok():
    if not shutil.which("docker"):
        return False
    return subprocess.run(["docker", "info"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0


@pytest.fixture
def lean_cluster(axon, new_cluster):
    """A uniquely named cluster with only postgres + traefik fabric (fast to start)."""
    if not docker_ok():
        pytest.skip("docker daemon not reachable")
    name = "e2e" + uuid.uuid4().hex[:6]
    c = new_cluster(name)
    for f in ("temporal", "keycloak", "tracing"):
        r = axon("cluster", "remove", "fabric", f, cwd=c.ctrl)
        assert r.code == 0, r.err
    yield c
    # always clean up, even after a failure (a second `down` is harmless)
    axon("down", "--volumes", "--yes", cwd=c.ctrl, timeout=600)


def test_e2e_image_app_up_status_test_down(axon, lean_cluster):
    c = lean_cluster
    http = c.toml()["fabric"]["traefik"]["http"]
    r = axon("cluster", "add", "app", "--name", "whoami", "--kind", "project", "--from-image", "traefik/whoami:v1.10",
             "--port", "80", "--db", "whoami", "--external-url", "http://whoami.lvh.me:%d" % http, cwd=c.ctrl)
    assert r.code == 0, r.err
    write(c.journeys / "whoami_smoke.toml",
          '[journey]\nname = "whoami smoke"\napp = "whoami"\n\n[[step]]\nname = "root"\npath = "/"\n'
          'expect = { status = 200, contains = "Hostname" }\n')
    assert axon("env", "test", "dev", "--lint", cwd=c.ctrl).code == 0

    r = axon("up", "--build", cwd=c.ctrl, timeout=UP_TIMEOUT)
    assert r.code == 0, r.out + r.err
    assert "✓ deployed %s/dev → docker" % c.name in r.err
    assert "front doors:" in r.out

    r = axon("env", "status", "dev", "--json", cwd=c.ctrl)
    assert r.code == 0, r.err
    st = json.loads(r.out)
    assert (st["cluster"], st["env"], st["target"], st["verdict"]) == (c.name, "dev", "docker", "healthy")
    apps = {w["app"] for w in st["workloads"]}
    assert {"fabric", "whoami"} <= apps
    assert all(w["healthy"] for w in st["workloads"])
    doors = {d["name"]: d for d in st["frontDoors"]}
    assert doors["whoami"]["ok"] and doors["whoami"]["url"] == "http://whoami.lvh.me:%d" % http
    assert st["lastRun"]["completed"] is True and st["lastRun"]["outcome"] == "ok"

    r = axon("env", "test", "dev", "--json", cwd=c.ctrl)
    assert r.code == 0, r.err
    lr = json.loads(r.out)
    assert lr["pass"] is True and [j["name"] for j in lr["journeys"]] == ["whoami smoke"]

    r = axon("env", "logs", "dev", "whoami", "--tail", "5", cwd=c.ctrl)
    assert r.code == 0, r.err

    # a second `up` with nothing changed is a no-op rollout and stays healthy
    r = axon("up", cwd=c.ctrl, timeout=UP_TIMEOUT)
    assert r.code == 0, r.err

    r = axon("down", "--volumes", "--yes", cwd=c.ctrl, timeout=600)
    assert r.code == 0, r.err
    assert "· DELETE the data volumes" in r.out
    st = json.loads(axon("env", "status", "dev", "--json", cwd=c.ctrl).out)
    assert st["verdict"] == "not deployed"


def test_e2e_plan_with_a_daemon_changes_nothing(axon, lean_cluster):
    c = lean_cluster
    r = axon("env", "plan", "dev", cwd=c.ctrl)
    assert r.code == 0, r.err
    assert "plan complete — nothing was changed" in r.err
    assert "would create docker network %s-net" % c.name in r.err
    assert not (c.state / "run.lock").exists()


def _builder_app(axon, c):
    r = axon("cluster", "add", "app", "--name", "job", "--kind", "project", "--from-image", "alpine:3.20", "--builder", cwd=c.ctrl)
    assert r.code == 0, r.err
    write(c.workspace / "job" / "Dockerfile", 'FROM alpine:3.20\nCMD ["sleep", "infinity"]\n')


def test_e2e_release_create_and_check(axon, lean_cluster):
    c = lean_cluster
    _builder_app(axon, c)
    r = axon("up", "--build", cwd=c.ctrl, timeout=UP_TIMEOUT)
    assert r.code == 0, r.out + r.err
    r = axon("release", "create", "--dry-run", cwd=c.ctrl)
    assert r.code == 0, r.err
    assert 'version = "0.1.0"' in r.out and 'tool_version = "axon/' in r.out
    assert not (c.ctrl / "releases").exists()
    r = axon("release", "create", cwd=c.ctrl)
    assert r.code == 0, r.err
    rec = c.ctrl / "releases" / "0.1.0.toml"
    assert oct(os.stat(str(rec)).st_mode & 0o777) == oct(0o444)  # written read-only
    d = load_toml(rec)
    assert d["version"] == "0.1.0" and d["digest"].startswith("sha256:")
    assert any(k.endswith("_job") for k in d["images"])
    assert r.out.strip() == d["digest"]
    assert axon("release", "check", cwd=c.ctrl).code == 0
    # the next one gets the next patch number
    assert axon("release", "create", cwd=c.ctrl).code == 0
    assert (c.ctrl / "releases" / "0.1.1.toml").exists()
    # a pinned environment can be planned against the recorded release
    assert axon("env", "new", "qa", "--tier", "qa", "--release", "0.1.0", "--target", "docker", cwd=c.ctrl).code == 0
    assert axon("env", "check", "qa", cwd=c.ctrl).code == 0


def test_e2e_release_create_after_cluster_build(axon, lean_cluster):
    """`release create` tells you to run `axon cluster build <app>` first; that must be enough."""
    c = lean_cluster
    _builder_app(axon, c)
    r = axon("cluster", "build", "job", cwd=c.ctrl, timeout=UP_TIMEOUT)
    assert r.code == 0, r.err
    r = axon("release", "create", cwd=c.ctrl)
    assert r.code == 0, r.err
