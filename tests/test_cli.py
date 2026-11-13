"""CLI contract: grammar, exit codes, messages, help, version, completion, global flags,
moved verbs and stream discipline."""

import json
import re
import shutil
import subprocess

import pytest

from _util import read, write

# The verb registry as documented (`axon --help`, docs/SPEC.md §5).
ALL_VERBS = sorted([
    "version", "completion", "up", "down", "migrate",
    "cluster new", "cluster add app", "cluster add ext", "cluster add fabric", "cluster add product",
    "cluster remove app", "cluster remove ext", "cluster remove fabric", "cluster remove product",
    "cluster set", "cluster check", "cluster explain", "cluster generate", "cluster build", "cluster ports",
    "app new", "app add module", "app remove module", "app set", "app check", "app build", "app run", "app test",
    "env new", "env list", "env show", "env set", "env unset", "env check", "env diagnose", "env deploy",
    "env plan", "env undeploy", "env status", "env logs", "env restart", "env test", "env iam-keys",
    "env rotate-iam-key",
    "release list", "release show", "release check", "release create", "release lock", "release checkout",
    "release cut", "ci status", "ci watch", "ci logs", "ci init", "ci check", "ci secrets", "ci environments",
    "secrets set", "secrets list", "secrets rm", "secrets import", "secrets exec",
])

MOVED = {
    "platform apply": "env deploy", "platform destroy": "env undeploy", "platform init": "env new",
    "platform outputs": "env show", "platform plan": "env plan", "platform status": "env status",
    "platform upgrade": "env set", "add app": "cluster add app", "add ext": "cluster add ext",
    "add-fabric": "cluster add fabric",
}

VERSION_RE = re.compile(
    r"^axon \S+\n"
    r"  commit   \S+\n"
    r"  built    (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} UTC|unknown)\n"
    r"  go       go\S+ \w+/\w+\n$"
)


def verbs_in_root_help(text):
    """Parse the 'all verbs:' section of the root help into verb names."""
    section = text.split("all verbs:", 1)[1].split("global flags:", 1)[0]
    names = []
    for line in section.splitlines():
        if not line.startswith("    axon "):
            continue
        head = line[4:].split("  ", 1)[0].split()
        words = []
        for w in head[1:]:
            if w.startswith("<") or w.startswith("["):
                break
            words.append(w)
        names.append(" ".join(words))
    return sorted(names)


# ── help ─────────────────────────────────────────────────────────────────────


@pytest.mark.parametrize("argv", [[], ["help"], ["--help"], ["-h"]])
def test_root_help_forms(axon, argv):
    r = axon(*argv)
    assert r.code == 0
    assert r.err == ""
    assert "usage: axon <verb> [args] [--flags]" in r.out
    assert "common commands:" in r.out and "all verbs:" in r.out and "global flags:" in r.out


def test_root_help_lists_every_verb_and_moved_name(axon):
    r = axon("--help")
    assert verbs_in_root_help(r.out) == ALL_VERBS
    moved = r.out.split("moved (still work, with a notice):", 1)[1].split("all verbs:", 1)[0]
    for old, new in MOVED.items():
        assert re.search(r"^\s+%s\s+→ %s$" % (re.escape(old), re.escape(new)), moved, re.M), old
    for flag in ("--help, -h", "--version", "--yes, -y", "--quiet, -q", "--verbose, -v", "--no-color", "--compat <mode>"):
        assert flag in r.out


@pytest.mark.parametrize("verb", ALL_VERBS)
def test_every_verb_has_help(axon, verb):
    r = axon(*(verb.split() + ["--help"]))
    assert r.code == 0, r.err
    assert r.out.startswith("usage: axon %s" % verb)
    first = r.out.splitlines()[0]
    if first.endswith("[flags]"):
        assert "\nflags:\n" in r.out
    else:
        assert "flags:" not in r.out
    # `help <verb>` prints the same text
    assert axon(*(["help"] + verb.split())).out == r.out


def test_verb_help_lists_flags_but_not_hidden_ones(axon):
    out = axon("cluster", "add", "app", "--help").out
    for f in ("--name <value>", "--from <value>", "--from-image <value>", "--kind <value>", "--stateless",
              "--env <value> (repeatable)", "--dry-run", "(default module)"):
        assert f in out
    assert "--ext" not in out.replace("--external-url", "")
    assert "--tenant" in axon("cluster", "add", "ext", "--help").out  # the alias is documented


def test_help_has_no_side_effects(axon, tmp_path):
    r = axon("cluster", "new", "zzz", "--help", cwd=tmp_path)
    assert r.code == 0
    assert not (tmp_path / "zzz").exists()


# ── version ──────────────────────────────────────────────────────────────────


@pytest.mark.parametrize("argv", [["--version"], ["version"], ["cluster", "check", "--version"]])
def test_version_format(axon, argv):
    r = axon(*argv)
    assert r.code == 0
    assert r.err == ""
    assert VERSION_RE.match(r.out), r.out


def test_version_flag_and_verb_agree(axon):
    assert axon("--version").out == axon("version").out


# ── unknown verbs and the suggestion rule ────────────────────────────────────


@pytest.mark.parametrize("argv,suggestion", [
    (["cluster", "bogus"], "cluster new"),  # cluster new / cluster set tie at 11 chars; sorted order wins
    (["cluster"], "cluster new"),
    (["e"], "env new"),
    (["app"], "app new"),
    (["rel"], "release cut"),
    (["c"], "ci init"),
    (["d"], "down"),
    (["ver"], "version"),
    (["platform"], "platform init"),  # no verb matches; moved names are searched next
    (["platform", "bogus"], "platform init"),
    (["add"], "add app"),
    (["bogus"], None),
    (["doctor"], None),
    (["xyz", "abc"], None),
])
def test_unknown_verb_suggestion(axon, argv, suggestion):
    r = axon(*argv)
    assert r.code == 2
    assert r.out == ""
    msg = 'axon: unknown verb "%s"' % " ".join(argv)
    if suggestion:
        msg += ' (did you mean "%s"?)' % suggestion
    assert r.err == msg + "\n"


def test_unknown_verb_message_echoes_all_args(axon):
    r = axon("cluster", "bogus", "x", "--json", "-q", "--compat", "off")
    assert r.code == 2
    assert r.err == 'axon: unknown verb "cluster bogus x --json -q --compat off" (did you mean "cluster new"?)\n'


def test_help_for_unknown_verb_is_a_usage_error(axon):
    r = axon("help", "nosuch")
    assert (r.code, r.out) == (2, "")
    assert r.err.startswith("axon: unknown verb ") and "nosuch" in r.err


# ── flags ────────────────────────────────────────────────────────────────────


def test_unknown_verb_flag(axon, tmp_path):
    r = axon("cluster", "check", "--bogus", cwd=tmp_path)
    assert (r.code, r.out, r.err) == (2, "", "axon: cluster check: unrecognized flag --bogus\n")
    r = axon("cluster", "check", "--bogus=1", cwd=tmp_path)
    assert r.code == 2 and "unrecognized flag --bogus\n" in r.err


def test_unknown_global_flag_before_verb(axon):
    r = axon("--bogus", "cluster", "check")
    assert r.code == 2
    assert r.err == "axon: unrecognized flag --bogus (run `axon help`)\n"


def test_verb_flag_before_verb_is_rejected(axon):
    r = axon("--json", "cluster", "check")
    assert r.code == 2 and "unrecognized flag --json" in r.err


@pytest.mark.parametrize("argv,verb,missing", [
    (["cluster", "new"], "cluster new", "dir"),
    (["env", "logs"], "env logs", "env, app"),
    (["env", "logs", "dev"], "env logs", "app"),
    (["cluster", "add", "fabric"], "cluster add fabric", "name"),
    (["cluster", "set"], "cluster set", "key=value"),
    (["release", "show"], "release show", "version"),
    (["completion"], "completion", "shell"),
])
def test_missing_required_arguments(axon, tmp_path, argv, verb, missing):
    r = axon(*argv, cwd=tmp_path)
    assert (r.code, r.out) == (2, "")
    assert r.err == "axon: %s: missing required argument(s): %s\n" % (verb, missing)


def test_unexpected_extra_argument(axon, tmp_path):
    r = axon("version", "extra")
    assert (r.code, r.err) == (2, 'axon: version: unexpected argument "extra"\n')
    r = axon("cluster", "check", "a", "b", cwd=tmp_path)
    assert (r.code, r.err) == (2, 'axon: cluster check: unexpected argument "b"\n')


def test_usage_errors_win_over_missing_context(axon, tmp_path):
    # flag parsing happens before the verb looks for a descriptor: 2, not 3
    assert axon("env", "new", "qa", "--tier", cwd=tmp_path).code == 2
    assert axon("cluster", "add", "fabric", "clickhouse", "--port", "abc", cwd=tmp_path).code == 2


def test_flag_value_forms_and_positions(axon, cluster):
    # --flag=value after the positional
    assert axon("env", "new", "qa1", "--tier=qa", cwd=cluster.ctrl).code == 0
    # --flag value before the positional
    assert axon("env", "new", "--tier", "uat", "qa2", cwd=cluster.ctrl).code == 0
    # flag between the verb words' positional and another flag
    assert axon("env", "new", "--namespace", "custom-ns", "qa3", "--tier=prod", cwd=cluster.ctrl).code == 0
    assert 'tier      = "qa"' in read(cluster.envs / "qa1.toml")
    assert 'tier      = "uat"' in read(cluster.envs / "qa2.toml")
    t3 = read(cluster.envs / "qa3.toml")
    assert 'tier      = "prod"' in t3 and 'namespace = "custom-ns"' in t3


def test_flag_between_verb_and_positional(axon, cluster):
    before = cluster.text()
    r = axon("cluster", "add", "fabric", "--dry-run", "clickhouse", cwd=cluster.ctrl)
    assert r.code == 0
    assert "+ [fabric.clickhouse]" in r.out
    assert cluster.text() == before


@pytest.mark.parametrize("value,is_json", [("true", True), ("false", False)])
def test_bool_flag_explicit_value(axon, cluster, value, is_json):
    r = axon("cluster", "check", "--json=" + value, cwd=cluster.ctrl)
    assert r.code == 0
    if is_json:
        assert json.loads(r.out) == {"findings": []}
    else:
        assert r.out.startswith("✓ 0 blocking · 0 risk · 0 note")


def test_bool_flag_rejects_other_values(axon, cluster):
    r = axon("cluster", "check", "--json=maybe", cwd=cluster.ctrl)
    assert r.code == 2
    assert r.err == 'axon: cluster check: flag --json takes no value (or true/false), got "maybe"\n'


def test_int_flag_validation(axon, cluster):
    r = axon("cluster", "add", "fabric", "clickhouse", "--port", "abc", cwd=cluster.ctrl)
    assert r.code == 2
    assert r.err == 'axon: cluster add fabric: flag --port wants a number, got "abc"\n'


def test_string_flag_needs_value(axon, cluster):
    r = axon("env", "new", "qa", "--tier", cwd=cluster.ctrl)
    assert (r.code, r.err) == (2, "axon: env new: flag --tier needs a value\n")
    r = axon("cluster", "check", "--compat", cwd=cluster.ctrl)
    assert (r.code, r.err) == (2, "axon: flag --compat needs a value\n")


def test_double_dash_ends_flags(axon, cluster):
    r = axon("env", "new", "--", "-weird", cwd=cluster.ctrl)
    assert r.code == 2
    assert 'name "-weird" is not safe' in r.err


def test_repeatable_flag_appends(axon, cluster):
    r = axon("cluster", "add", "app", "--name", "web", "--from-image", "reg.example/web:1",
             "--env", "A=1", "--env", "B=2,C=3", "--env", "D=x=y", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    env = cluster.toml()["app"][0]["env"]
    assert env == {"A": "1", "B": "2", "C": "3", "D": "x=y"}


# ── global flags ─────────────────────────────────────────────────────────────


@pytest.mark.parametrize("argv", [
    ["-q", "cluster", "set", "name"],
    ["cluster", "-q", "set", "name"],
    ["cluster", "set", "name", "-q"],
    ["cluster", "set", "--quiet", "name"],
])
def test_global_flags_anywhere(axon, cluster, argv):
    r = axon(*argv, cwd=cluster.ctrl)
    assert (r.code, r.out, r.err) == (0, "acme\n", "")


def test_quiet_suppresses_progress_not_results_or_errors(axon, cluster):
    loud = axon("cluster", "add", "app", "--name", "a1", "--from-image", "reg.example/a1:1", cwd=cluster.ctrl)
    assert "✓ added app" in loud.err and "generated" in loud.err
    quiet = axon("-q", "cluster", "add", "app", "--name", "a2", "--from-image", "reg.example/a2:1", cwd=cluster.ctrl)
    assert (quiet.code, quiet.out, quiet.err) == (0, "", "")
    err = axon("-q", "cluster", "add", "app", "--name", "a2", "--from-image", "reg.example/a2:1", cwd=cluster.ctrl)
    assert err.code == 1 and err.err == 'axon: app "a2" is already a member\n'
    assert axon("-q", "cluster", "generate", cwd=cluster.ctrl).out == "up to date\n"


def test_verbose_prints_external_commands(axon, cluster):
    quiet = axon("cluster", "check", cwd=cluster.ctrl)
    loud = axon("-v", "cluster", "check", cwd=cluster.ctrl)
    assert quiet.code == loud.code == 0
    assert "$ git" not in quiet.err
    assert "$ git" in loud.err
    assert loud.out == quiet.out  # diagnostics never leak into results


@pytest.mark.parametrize("extra_env", [{}, {"NO_COLOR": None, "TERM": "xterm-256color"}])
def test_no_color(axon, cluster, extra_env):
    r = axon("--no-color", "cluster", "check", cwd=cluster.ctrl, env=extra_env)
    assert r.code == 0
    assert "\x1b[" not in r.out + r.err
    r = axon("cluster", "add", "app", "--name", "web", "--from-image", "reg.example/web:1", cwd=cluster.ctrl, env=extra_env)
    assert "\x1b[" not in r.out + r.err


def test_compat_flag_rejects_nothing_but_warns_on_unknown_mode(axon, cluster):
    r = axon("--compat", "bogus", "cluster", "set", "name", cwd=cluster.ctrl)
    assert r.code == 0 and r.out == "acme\n"
    assert '! warn: unknown compat mode "bogus" (auto, mycel or off); using auto' in r.err
    assert axon("--compat=off", "cluster", "set", "name", cwd=cluster.ctrl).out == "acme\n"


# ── exit code 3: no descriptor ───────────────────────────────────────────────


@pytest.mark.parametrize("argv", [
    ["cluster", "check"], ["cluster", "generate"], ["cluster", "add", "app", "--name", "x", "--from-image", "i:1"],
    ["env", "list"], ["env", "check", "dev"], ["env", "test", "dev", "--list"], ["release", "list"],
    ["migrate"], ["up"], ["down"], ["cluster", "set", "name"],
])
def test_no_cluster_context_exits_3(axon, tmp_path, argv):
    r = axon(*argv, cwd=tmp_path)
    assert r.code == 3, (argv, r.err)
    assert r.out == ""
    assert r.err.startswith("axon: no ") and str(tmp_path.resolve()) in r.err


@pytest.mark.parametrize("argv", [["app", "check"], ["app", "add", "module", "x"], ["app", "set", "app.name"]])
def test_no_app_context_exits_3(axon, tmp_path, argv):
    r = axon(*argv, cwd=tmp_path)
    assert r.code == 3
    assert "no axonfile (or mycelfile) in" in r.err
    assert "pass --dir <path>" in r.err


def test_dir_flag_and_upward_search(axon, cluster, tmp_path):
    assert axon("cluster", "set", "name", "--dir", str(cluster.ctrl), cwd=tmp_path).out == "acme\n"
    assert axon("cluster", "check", str(cluster.ctrl), cwd=tmp_path).code == 0
    # from a sub directory of the controller
    assert axon("cluster", "set", "name", cwd=cluster.envs).out == "acme\n"


# ── moved verbs ──────────────────────────────────────────────────────────────


def test_moved_verb_forwards_with_notice(axon, cluster):
    r = axon("platform", "init", "qa", "--tier", "qa", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    assert r.err.startswith('axon: "platform init" is now "env new"; the old name will be removed in a later release\n')
    assert (cluster.envs / "qa.toml").exists()
    r = axon("platform", "outputs", "dev", cwd=cluster.ctrl)
    assert r.code == 0 and "namespace  acme-dev" in r.out
    assert '"platform outputs" is now "env show"' in r.err


def test_moved_verb_keeps_exit_codes(axon, tmp_path):
    r = axon("platform", "status", "dev", cwd=tmp_path)
    assert r.code == 3
    assert r.err.splitlines()[0] == 'axon: "platform status" is now "env status"; the old name will be removed in a later release'


def test_moved_add_fabric(axon, cluster):
    r = axon("add-fabric", "clickhouse", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    assert '"add-fabric" is now "cluster add fabric"' in r.err
    assert "clickhouse" in cluster.toml()["fabric"]


# ── completion ───────────────────────────────────────────────────────────────

TOP_WORDS = ["app", "cluster", "completion", "down", "env", "migrate", "release", "up", "version"]


@pytest.mark.parametrize("shell", ["bash", "zsh", "fish"])
def test_completion_scripts_cover_verbs_and_flags(axon, shell):
    r = axon("completion", shell)
    assert r.code == 0 and r.err == ""
    s = r.out
    for w in TOP_WORDS:
        assert re.search(r"\b%s\b" % w, s), w
    for sub in ("generate", "ports", "rotate-iam-key", "checkout", "module"):
        assert sub in s
    names = ["dry-run", "json", "from-image", "compat", "skip-probe", "role-stem", "yes"]
    tokens = set(re.split(r"[\s'\";()]+", s))
    for n in names:
        if shell == "fish":
            assert ("-l %s\n" % n) in s, n
        else:
            assert ("--" + n) in tokens, n
    # hidden flags stay hidden (cluster add app --ext)
    if shell == "fish":
        assert "-l ext\n" not in s
    else:
        assert "--ext" not in tokens and "--external-url" in tokens


def test_completion_bash_is_valid_shell(axon, tmp_path):
    if not shutil.which("bash"):
        pytest.skip("bash not installed")
    script = write(tmp_path / "axon.bash", axon("completion", "bash").out)
    assert subprocess.run(["bash", "-n", str(script)]).returncode == 0
    assert "complete -F _axon axon" in read(script)


def test_completion_zsh_is_valid_shell(axon, tmp_path):
    if not shutil.which("zsh"):
        pytest.skip("zsh not installed")
    script = write(tmp_path / "_axon", axon("completion", "zsh").out)
    assert read(script).startswith("#compdef axon\n")
    assert subprocess.run(["zsh", "-n", str(script)]).returncode == 0


def test_completion_unknown_shell(axon):
    r = axon("completion", "tcsh")
    assert (r.code, r.out) == (2, "")
    assert r.err == 'axon: completion: unsupported shell "tcsh" (bash, zsh or fish)\n'


# ── stream discipline ────────────────────────────────────────────────────────


def test_results_on_stdout_progress_on_stderr(axon, tmp_path, xdg):
    r = axon("cluster", "new", "acme", cwd=tmp_path)
    assert r.code == 0
    assert r.out.startswith("next:\n") and "  axon up --build" in r.out
    assert "allocated free host ports — postgres" in r.err and '✓ cluster "acme" scaffolded' in r.err
    ctrl = tmp_path / "acme" / "acme.axon"
    r = axon("cluster", "generate", cwd=ctrl)
    assert (r.out, r.err) == ("up to date\n", "")
    r = axon("cluster", "add", "app", "--name", "web", "--from-image", "reg.example/web:1", cwd=ctrl)
    assert r.out == "" and "✓ added app" in r.err
    r = axon("cluster", "add", "app", "--name", "web", "--from-image", "reg.example/web:1", cwd=ctrl)
    assert r.code == 1 and r.out == "" and r.err.startswith("axon: ")


@pytest.mark.parametrize("argv", [
    ["cluster", "check", "--json"], ["cluster", "ports", "--json"], ["env", "list", "--json"],
    ["env", "show", "dev", "--json"], ["env", "check", "dev", "--json"],
    ["env", "diagnose", "dev", "--json", "--concern", "conformance,security,devx,business"],
    ["release", "list", "--json"], ["release", "check", "--json"],
])
def test_json_outputs_parse(axon, web_cluster, argv):
    r = axon(*argv, cwd=web_cluster.ctrl)
    assert r.code in (0, 1), r.err
    json.loads(r.out)  # exactly one JSON document on stdout
    assert r.out.startswith(("{", "[", "null"))
    assert r.out.endswith("\n")
    assert "\n  " in r.out or r.out.strip() in ("[]", "{}", "null")  # 2-space pretty printing
