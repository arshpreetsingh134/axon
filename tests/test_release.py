"""Releases (offline): release check/list/show against hand-written records, release
create's guard rails, and release lock/checkout on git-initialized member apps."""

import json

import pytest

from _util import digest, load_toml, read, release_doc, render_release, snapshot, write


def put(cluster, doc, digest_value=None, name=None):
    p = cluster.ctrl / "releases" / ("%s.toml" % (name or doc["version"]))
    write(p, render_release(doc, digest_value))
    return p


# ── release check ────────────────────────────────────────────────────────────


def test_check_accepts_an_intact_axon_record(axon, cluster):
    doc = release_doc("0.1.0")
    put(cluster, doc)
    r = axon("release", "check", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    assert r.out == "✓ 0.1.0 digest %s\n" % digest(doc)


def test_check_detects_a_tampered_record(axon, cluster):
    doc = release_doc("0.1.0")
    p = put(cluster, doc)
    write(p, read(p).replace("0123456789abcdef", "fedcba9876543210"))
    r = axon("release", "check", "0.1.0", cwd=cluster.ctrl)
    assert r.code == 1
    assert r.out.startswith("✗ 0.1.0 digest mismatch: recorded %s, computed sha256:" % digest(doc))
    assert r.out.rstrip().endswith("— the record was edited")


def test_check_detects_a_tampered_digest(axon, cluster):
    put(cluster, release_doc("0.1.0"), digest_value="sha256:" + "0" * 64)
    assert axon("release", "check", cwd=cluster.ctrl).code == 1


def test_check_record_without_digest_is_a_mismatch(axon, cluster):
    write(cluster.ctrl / "releases" / "0.1.0.toml", 'version = "0.1.0"\ntool_version = "axon/0.1.0"\n')
    r = axon("release", "check", "--json", cwd=cluster.ctrl)
    assert r.code == 1
    assert json.loads(r.out)[0]["status"] == "mismatch"


def test_check_mycel_record_is_unverifiable_not_failing(axon, cluster):
    doc = release_doc("0.0.9", tool="mycel-gh/0.3.152")
    put(cluster, doc, digest_value="sha256:" + "f" * 64)  # mycel's encoding differs; never re-verified
    r = axon("release", "check", cwd=cluster.ctrl)
    assert r.code == 0
    assert r.out == ("· 0.0.9 produced by mycel-gh/0.3.152: its digest uses that tool's encoding and is not "
                     "re-verified (structure checked)\n")


def test_check_json_and_ordering(axon, cluster):
    put(cluster, release_doc("0.1.10"))
    put(cluster, release_doc("0.1.9", tool="mycel-gh/0.3.1"), digest_value="sha256:" + "1" * 64)
    bad = put(cluster, release_doc("0.1.2"))
    write(bad, read(bad).replace('by = "acceptance-tests"', 'by = "mallory"'))
    r = axon("release", "check", "--json", cwd=cluster.ctrl)
    assert r.code == 1
    res = json.loads(r.out)
    assert [x["version"] for x in res] == ["0.1.2", "0.1.9", "0.1.10"]  # numeric, not lexical, order
    assert [x["status"] for x in res] == ["mismatch", "unverifiable", "ok"]
    assert res[2]["want"] == res[2]["got"] == digest(release_doc("0.1.10"))
    assert res[1]["producer"] == "mycel-gh/0.3.1"
    for x in res:
        assert {"version", "status", "want", "got", "producer"} <= set(x)


def test_check_reports_unpinned_images(axon, cluster):
    doc = release_doc("0.2.0", images={"web": "web"})
    put(cluster, doc)
    res = json.loads(axon("release", "check", "--json", cwd=cluster.ctrl).out)
    assert res[0]["status"] == "ok" and res[0]["unpinned"] == ["images.web"]


def test_check_unknown_version(axon, cluster):
    r = axon("release", "check", "9.9.9", cwd=cluster.ctrl)
    assert (r.code, r.err) == (1, 'axon: no release "9.9.9" (releases/9.9.9.toml)\n')


def test_check_with_no_records(axon, cluster):
    r = axon("release", "check", cwd=cluster.ctrl)
    assert (r.code, r.out) == (0, "")


# ── release list / show ──────────────────────────────────────────────────────


def test_list(axon, cluster):
    put(cluster, release_doc("0.1.10"))
    put(cluster, release_doc("0.1.2", tool="mycel-gh/0.3.152"))
    r = axon("release", "list", cwd=cluster.ctrl)
    assert r.code == 0
    rows = [l.split() for l in r.out.splitlines()]
    assert rows[0] == ["VERSION", "CREATED", "BY", "TOOL", "DIGEST"]
    assert [row[0] for row in rows[1:]] == ["0.1.2", "0.1.10"]
    assert rows[2][1:4] == ["2026-01-02T03:04:05Z", "acceptance-tests", "axon/0.1.0-dev"]
    assert rows[2][4] == digest(release_doc("0.1.10"))[:19] + "…"
    d = json.loads(axon("release", "list", "--json", cwd=cluster.ctrl).out)
    assert [x["version"] for x in d] == ["0.1.2", "0.1.10"]
    assert d[1]["digest"] == digest(release_doc("0.1.10"))
    assert d[1]["images"] == release_doc("0.1.10")["images"]


def test_list_empty(axon, cluster):
    r = axon("release", "list", cwd=cluster.ctrl)
    assert (r.code, r.out) == (0, "")
    assert "no releases yet" in r.err


def test_list_json_empty_is_an_empty_list(axon, cluster):
    r = axon("release", "list", "--json", cwd=cluster.ctrl)
    assert r.code == 0
    assert json.loads(r.out) == []


def test_show(axon, cluster):
    p = put(cluster, release_doc("0.1.0"))
    r = axon("release", "show", "0.1.0", cwd=cluster.ctrl)
    assert (r.code, r.out) == (0, read(p))
    d = json.loads(axon("release", "show", "0.1.0", "--json", cwd=cluster.ctrl).out)
    assert d["version"] == "0.1.0" and d["digest"] == digest(release_doc("0.1.0"))
    assert d["sources"] == {"web": "0123456789abcdef0123456789abcdef01234567"}
    r = axon("release", "show", "0.9.9", cwd=cluster.ctrl)
    assert r.code == 1 and 'no release "0.9.9"' in r.err


# ── release create (the parts that need no docker) ───────────────────────────


def test_create_without_any_build(axon, web_cluster):
    before = snapshot(web_cluster.ctrl)
    r = axon("release", "create", cwd=web_cluster.ctrl)
    assert r.code == 1
    assert "no built images recorded yet" in r.err
    assert snapshot(web_cluster.ctrl) == before


VERSION_FLAG_BUG = (
    "BUG: `release create --version <v>` is swallowed by the global --version flag: `--version 1.2.3` prints "
    "the tool version and exits 0 without creating anything, and `--version=1.2.3` is parsed as a global "
    "boolean and silently ignored, so a release can never be given an explicit version")


@pytest.mark.parametrize("form", [["--version", "../1.0"], ["--version=../1.0"]])
def test_create_refuses_an_unsafe_version(axon, cluster, form):
    r = axon(*(["release", "create"] + form), cwd=cluster.ctrl)
    assert (r.code, r.err) == (2, 'axon: release create: version "../1.0" is not safe\n')


def test_create_refuses_to_overwrite(axon, cluster):
    p = put(cluster, release_doc("0.1.0"))
    before = read(p)
    r = axon("release", "create", "--version", "0.1.0", cwd=cluster.ctrl)
    assert read(p) == before
    assert r.code == 1 and "release 0.1.0 already exists; records are immutable" in r.err


def test_create_help_documents_the_version_flag(axon):
    assert "--version <value>" in axon("release", "create", "--help").out


# ── release lock / checkout ──────────────────────────────────────────────────


@pytest.fixture
def git_app(axon, git, web_cluster):
    """web_cluster with its member app dir turned into a git repo with one commit."""
    app = web_cluster.workspace / "web"
    git(app, "init", "-q")
    git(app, "add", "-A")
    git(app, "commit", "-q", "-m", "first")
    return web_cluster, app


def test_lock_pins_source_commits(axon, git, git_app):
    cluster, app = git_app
    head = git(app, "rev-parse", "HEAD")
    r = axon("release", "lock", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    assert "✓ locked 1 source(s) → release/sources.lock.toml" in r.err
    lock = cluster.ctrl / "release" / "sources.lock.toml"
    assert load_toml(lock) == {"sources": {"web": head}}
    assert read(lock).startswith("# Source commits of every member app")
    r = axon("release", "lock", "--check", cwd=cluster.ctrl)
    assert (r.code, r.out) == (0, "up to date\n")


def test_lock_check_reports_drift_without_writing(axon, git, git_app):
    cluster, app = git_app
    old = git(app, "rev-parse", "HEAD")
    assert axon("release", "lock", cwd=cluster.ctrl).code == 0
    write(app / "CHANGELOG.md", "change\n")
    git(app, "add", "-A")
    git(app, "commit", "-q", "-m", "second")
    new = git(app, "rev-parse", "HEAD")
    lock = cluster.ctrl / "release" / "sources.lock.toml"
    before = read(lock)
    r = axon("release", "lock", "--check", cwd=cluster.ctrl)
    assert r.code == 1
    assert r.out == "~ web %s → %s\n" % (old[:12], new[:12])
    assert read(lock) == before


def test_lock_check_without_a_lock_file(axon, git_app):
    cluster, app = git_app
    r = axon("release", "lock", "--check", cwd=cluster.ctrl)
    assert r.code == 1 and r.out.startswith("~ web (none) → ")
    assert not (cluster.ctrl / "release" / "sources.lock.toml").exists()


def test_checkout_goes_back_to_the_locked_commits(axon, git, git_app):
    cluster, app = git_app
    old = git(app, "rev-parse", "HEAD")
    assert axon("release", "lock", cwd=cluster.ctrl).code == 0
    write(app / "CHANGELOG.md", "change\n")
    git(app, "add", "-A")
    git(app, "commit", "-q", "-m", "second")
    r = axon("release", "checkout", "--dry-run", cwd=cluster.ctrl)
    assert r.code == 0 and "would check out web at %s" % old[:12] in r.err
    assert git(app, "rev-parse", "HEAD") != old
    r = axon("release", "checkout", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    assert git(app, "rev-parse", "HEAD") == old
    r = axon("release", "checkout", cwd=cluster.ctrl)
    assert "web already at %s" % old[:12] in r.err


def test_checkout_from_a_release_record(axon, git, git_app):
    cluster, app = git_app
    old = git(app, "rev-parse", "HEAD")
    write(app / "x.txt", "x\n")
    git(app, "add", "-A")
    git(app, "commit", "-q", "-m", "second")
    put(cluster, release_doc("0.1.0", sources={"web": old}))
    assert axon("release", "checkout", "0.1.0", cwd=cluster.ctrl).code == 0
    assert git(app, "rev-parse", "HEAD") == old


def test_checkout_refuses_a_dirty_tree(axon, git, git_app):
    cluster, app = git_app
    assert axon("release", "lock", cwd=cluster.ctrl).code == 0
    write(app / "dirty.txt", "uncommitted\n")
    head = git(app, "rev-parse", "HEAD")
    r = axon("release", "checkout", cwd=cluster.ctrl)
    assert (r.code, r.err) == (1, "axon: uncommitted changes in web; commit or stash them first\n")
    assert git(app, "rev-parse", "HEAD") == head


def test_checkout_without_lock_or_version(axon, cluster):
    r = axon("release", "checkout", cwd=cluster.ctrl)
    assert r.code == 1
    assert "no release/sources.lock.toml (run `axon release lock`) and no version given" in r.err
