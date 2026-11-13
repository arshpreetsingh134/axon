"""axon secrets: the offline contract (the store itself needs a Kubernetes cluster)."""
import pytest

from _util import read, write


def test_value_on_the_command_line_is_refused(axon, cluster):
    r = axon("secrets", "set", "OPENROUTER_API_KEY=sk-should-never-be-accepted", cwd=cluster.ctrl)
    assert r.code == 2
    assert "never from the command line" in r.err
    assert "sk-should-never" not in r.out + r.err.split("never from")[0]


@pytest.mark.parametrize("name", ["lower_case", "1STARTS_WITH_DIGIT", "HAS-DASH"])
def test_invalid_names_are_refused(axon, cluster, name):
    r = axon("secrets", "set", name, cwd=cluster.ctrl)
    assert r.code == 2 and "not a valid name" in r.err


def test_unreachable_store_fails_clearly(axon, cluster):
    # unmarked tests point KUBECONFIG at nothing
    r = axon("secrets", "list", cwd=cluster.ctrl)
    assert r.code == 1
    assert "secret store lives in Kubernetes" in r.err


def test_exec_needs_a_command(axon, cluster):
    r = axon("secrets", "exec", cwd=cluster.ctrl)
    assert r.code == 2 and "missing required argument" in r.err


def test_env_check_points_at_axon_secrets(axon, cluster):
    desc = cluster.infra / "axonfile"
    write(desc, read(desc) + '\n[[app]]\nname = "acme.ai"\nkind = "project"\nimage = "alpine:3"\n[app.env]\nOPENROUTER_API_KEY = "${OPENROUTER_API_KEY}"\n')
    r = axon("env", "check", "dev", cwd=cluster.ctrl)
    assert r.code == 1
    assert "ENV_VALUE_MISSING" in r.out and "axon secrets set OPENROUTER_API_KEY" in r.out


def test_secrets_verbs_are_listed_in_help(axon):
    out = axon("--help").out
    for v in ("secrets set", "secrets list", "secrets rm", "secrets import", "secrets exec"):
        assert v in out
