"""axon ci / release cut: the offline guards (the GitHub API side is covered by Go tests)."""
import subprocess

import pytest


def git(cwd, *args):
    subprocess.run(["git", "-C", str(cwd), *args], check=True, capture_output=True,
                   env={"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1", "HOME": str(cwd),
                        "PATH": "/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin",
                        "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"})


@pytest.fixture
def repo(tmp_path):
    d = tmp_path / "proj"
    d.mkdir()
    git(d, "init", "-q", "-b", "main")
    (d / "f.txt").write_text("x\n")
    git(d, "add", "f.txt")
    git(d, "commit", "-q", "-m", "init")
    return d


@pytest.mark.parametrize("argv", [["ci", "status"], ["ci", "logs", "--failed"], ["release", "cut", "--dry-run"]])
def test_outside_a_git_repository(axon, tmp_path, argv):
    r = axon(*argv, cwd=tmp_path)
    assert r.code == 1 and "not inside a git repository" in r.err


def test_repository_without_a_remote(axon, repo):
    r = axon("ci", "status", cwd=repo)
    assert r.code == 1 and 'has no "origin" remote' in r.err


def test_non_github_remote(axon, repo):
    git(repo, "remote", "add", "origin", "https://gitlab.com/acme/proj.git")
    r = axon("release", "cut", "--dry-run", cwd=repo)
    assert r.code == 1 and "is not a GitHub repository" in r.err


def test_bad_flags_are_usage_errors(axon, repo):
    r = axon("ci", "watch", "--timeout", "soon", cwd=repo)
    assert r.code == 2
    r = axon("ci", "logs", "--run", "abc", cwd=repo)
    assert r.code in (1, 2)


def test_release_cut_version_flag_is_not_the_global_version(axon, repo):
    # --version belongs to `release cut` here, not the global "print axon's version"
    r = axon("release", "cut", "--version", "1.2", "--dry-run", cwd=repo)
    assert "axon " not in r.out.splitlines()[0] if r.out else True
    assert r.code != 0


def test_ci_verbs_in_help(axon):
    out = axon("--help").out
    for v in ("ci status", "ci watch", "ci logs", "release cut"):
        assert v in out
