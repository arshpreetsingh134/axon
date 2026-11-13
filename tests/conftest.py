"""Shared fixtures for the axon black-box acceptance suite.

Every test drives the compiled binary through ``subprocess``. Hermeticity rules:

* each test gets its own ``XDG_CONFIG_HOME`` (so the port registry and user config live
  in ``tmp_path``, never in the real ``~/.config``);
* ``NO_COLOR=1``; every ``AXON_*`` / ``MYCEL_*`` variable of the caller is dropped;
* unmarked tests point ``DOCKER_HOST`` and ``KUBECONFIG`` at nothing, so even an
  accidental docker/kubectl call cannot reach (let alone mutate) a real daemon or cluster;
* ``GOPROXY=off`` keeps ``app new`` / ``app add module`` (which run ``go mod tidy``) offline.
"""

import json
import os
import pathlib
import shutil
import subprocess
import sys
from collections import namedtuple

import pytest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from _util import Cluster, write  # noqa: E402

REPO = pathlib.Path(__file__).resolve().parent.parent

Result = namedtuple("Result", ["code", "out", "err"])


# ── the binary ───────────────────────────────────────────────────────────────


@pytest.fixture(scope="session")
def axon_bin(tmp_path_factory):
    """Path to the axon binary: $AXON_BIN, else built once per session from the repo."""
    env_bin = os.environ.get("AXON_BIN")
    if env_bin:
        p = pathlib.Path(env_bin).resolve()
        if not p.is_file():
            pytest.exit("AXON_BIN=%s does not exist" % env_bin, returncode=4)
        return str(p)
    go = shutil.which("go")
    if not go:
        pytest.exit("`go` is not on PATH and AXON_BIN is unset; cannot build axon", returncode=4)
    out = tmp_path_factory.mktemp("axon-bin") / "axon"
    r = subprocess.run([go, "build", "-o", str(out), "./cmd/axon"], cwd=str(REPO),
                       stdout=subprocess.PIPE, stderr=subprocess.STDOUT, universal_newlines=True)
    if r.returncode != 0:
        pytest.exit("building axon failed:\n" + r.stdout, returncode=4)
    return str(out)


# ── environment ──────────────────────────────────────────────────────────────


def _scrubbed_environ():
    env = {}
    for k, v in os.environ.items():
        if k.startswith(("AXON_", "MYCEL_")):
            continue
        if k in ("NO_COLOR", "TERM", "XDG_CONFIG_HOME", "GIT_DIR", "GIT_WORK_TREE", "GOFLAGS"):
            continue
        env[k] = v
    return env


@pytest.fixture
def xdg(tmp_path):
    """The per-test XDG_CONFIG_HOME (axon's registry lands in <xdg>/axon/ports.toml)."""
    p = tmp_path / "xdg"
    p.mkdir()
    return p


@pytest.fixture
def base_env(request, xdg, tmp_path):
    """The subprocess environment for this test."""
    env = _scrubbed_environ()
    env.update({
        "XDG_CONFIG_HOME": str(xdg),
        "NO_COLOR": "1",
        "TERM": "dumb",
        "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull,
        "GIT_AUTHOR_NAME": "axon-tests", "GIT_AUTHOR_EMAIL": "tests@example.invalid",
        "GIT_COMMITTER_NAME": "axon-tests", "GIT_COMMITTER_EMAIL": "tests@example.invalid",
    })
    marked_runtime = request.node.get_closest_marker("docker") or request.node.get_closest_marker("k8s")
    if not marked_runtime:
        env.update({"GOPROXY": "off", "GOSUMDB": "off"})
        env.pop("DOCKER_CONTEXT", None)
        env["DOCKER_HOST"] = "unix://" + str(tmp_path / "no-docker.sock")
        env["KUBECONFIG"] = str(tmp_path / "no-kubeconfig")
    return env


@pytest.fixture
def axon(axon_bin, base_env, tmp_path):
    """Run axon: ``axon(*args, cwd=None, env=None, input=None, timeout=120)`` → Result(code, out, err).

    ``env`` entries override the hermetic base environment (a ``None`` value removes a key).
    Without ``input`` stdin is /dev/null, i.e. never a TTY.
    """

    def run(*args, cwd=None, env=None, input=None, timeout=120):
        e = dict(base_env)
        for k, v in (env or {}).items():
            if v is None:
                e.pop(k, None)
            else:
                e[k] = v
        p = subprocess.run(
            [axon_bin] + [str(a) for a in args],
            cwd=str(cwd or tmp_path), env=e,
            input=input, stdin=None if input is not None else subprocess.DEVNULL,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            universal_newlines=True, timeout=timeout,
        )
        return Result(p.returncode, p.stdout, p.stderr)

    return run


@pytest.fixture
def axon_json(axon):
    """Run a --json verb and return (code, parsed stdout)."""

    def run(*args, **kw):
        r = axon(*args, **kw)
        assert r.out.strip(), "no JSON on stdout (exit %d): %s" % (r.code, r.err)
        return r.code, json.loads(r.out)

    return run


@pytest.fixture
def git(base_env):
    """Run git hermetically: git(cwd, *args) → stdout (raises on failure)."""

    def run(cwd, *args):
        cmd = ["git", "-c", "user.name=axon-tests", "-c", "user.email=tests@example.invalid",
               "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main", "-C", str(cwd)] + list(args)
        p = subprocess.run(cmd, env=base_env, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                           universal_newlines=True)
        if p.returncode != 0:
            raise AssertionError("git %s failed: %s" % (" ".join(args), p.stderr))
        return p.stdout.strip()

    if not shutil.which("git"):
        pytest.skip("git is not installed")
    return run


# ── cluster fixtures ─────────────────────────────────────────────────────────


@pytest.fixture
def new_cluster(axon, tmp_path, xdg):
    """Factory: scaffold a fresh cluster with `axon cluster new` and return a Cluster."""

    def make(name="acme", *flags, parent=None, compat=None):
        parent = pathlib.Path(parent or (tmp_path / "work"))
        parent.mkdir(parents=True, exist_ok=True)
        args = []
        if compat:
            args += ["--compat", compat]
        r = axon(*(args + ["cluster", "new", name] + list(flags)), cwd=parent)
        assert r.code == 0, "cluster new failed: %s%s" % (r.out, r.err)
        brand = "mycel" if compat == "mycel" else "axon"
        return Cluster(workspace=parent / name, name=name, brand=brand, xdg=xdg)

    return make


@pytest.fixture
def cluster(new_cluster):
    """A freshly scaffolded axon cluster named "acme" (no apps)."""
    return new_cluster("acme")


@pytest.fixture
def web_cluster(cluster, axon):
    """The "acme" cluster with one image-composed app "web" (module web_mod) and core_image set."""
    r = axon("cluster", "add", "app", "--name", "web", "--from-image", "ghcr.io/example/web-mod:1.4", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    r = axon("cluster", "set", "core_image=registry.example/core:2.0", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    return cluster


# ── a hand-built mycel (compat) repo ─────────────────────────────────────────

MYCEL_CLUSTER = """\
# A mycel cluster descriptor, written by hand for the compat tests.
build_type = "cluster"
name       = "x"
domain     = "lvh.me"

[fabric.postgres]
image = "postgres:16-alpine"
port  = 15432

[fabric.traefik]
http = 18090

# Tracing exists in the mycelfile but the axon overlay skips it locally.
[fabric.tracing]
tempo_image   = "grafana/tempo:2.6.1"
grafana_image = "grafana/grafana:11.3.0"
grafana_port  = 13000

[[app]]
name = "shop"
host = "shop"
db   = "shop"

[[app]]
name  = "worker"
kind  = "project"
image = "reg.example/worker:2"
"""

MYCEL_APP = """\
build_type = "app"

[app]
name  = "shop"
title = "Shop"

[[compose]]
source = "modules/catalog/module"
image  = "ghcr.io/acme/catalog:1.2"
token  = "catalog"
"""

MYCEL_OVERLAY = """\
# axon-only settings next to an unchanged mycelfile.
[deploy]
skip        = ["worker"]
skip_fabric = ["tracing"]

[images]
"ghcr.io/acme/catalog:1.2" = "mirror.local/catalog:1.2"
"postgres:16-alpine"       = "mirror.local/postgres:16"

[hooks]
pre_generate = ["echo hook-ran > hook-marker.txt"]
"""


@pytest.fixture
def mycel_repo(tmp_path):
    """A mycel workspace built by hand: x.mycel/.mycel.infra/mycelfile + sibling app "shop".

    Returns a dict of paths. The overlay (.mycel.infra/axon.toml) is written by the
    ``mycel_overlay`` fixture, not here.
    """
    ws = tmp_path / "mws"
    ctrl = ws / "x.mycel"
    write(ctrl / ".mycel.infra" / "mycelfile", MYCEL_CLUSTER)
    write(ctrl / "environments" / "dev.toml", 'name = "dev"\ntier = "dev"\nrelease = "local"\ntarget = "docker"\n')
    write(ws / "shop" / "mycelfile", MYCEL_APP)
    return {"ws": ws, "ctrl": ctrl, "infra": ctrl / ".mycel.infra", "app": ws / "shop"}


@pytest.fixture
def mycel_overlay(mycel_repo):
    write(mycel_repo["infra"] / "axon.toml", MYCEL_OVERLAY)
    return mycel_repo
