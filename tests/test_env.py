"""Environment verbs (offline): env new/list/show/set/unset/check/diagnose, the docker-free
paths of env plan / up / down, the run lock, and IAM key handling."""

import json
import os
import stat

import pytest

from _util import load_toml, read, snapshot, write


def env_file(cluster, name):
    return cluster.envs / ("%s.toml" % name)


def overrides(cluster, name):
    return cluster.envs / ("%s.overrides.toml" % name)


# ── env new ──────────────────────────────────────────────────────────────────


def test_env_new_defaults(axon, cluster):
    r = axon("env", "new", "qa", "--tier", "qa", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    assert '✓ environment "qa" → environments/qa.toml' in r.err
    d = load_toml(env_file(cluster, "qa"))
    assert (d["name"], d["tier"], d["namespace"], d["release"]) == ("qa", "qa", "acme-qa", "")
    assert d["storage"]["durable"] == {"backend": "pvc", "size": "20Gi", "reclaim": "retain"}
    assert "target" not in d


def test_env_new_dev_tier_defaults_to_the_local_release(axon, cluster):
    assert axon("env", "new", "sandbox", cwd=cluster.ctrl).code == 0
    d = load_toml(env_file(cluster, "sandbox"))
    assert (d["tier"], d["release"]) == ("dev", "local")


def test_env_new_all_flags(axon, cluster):
    r = axon("env", "new", "prod", "--tier", "prod", "--namespace", "acme", "--context", "kind-prod", "--release", "1.0.0",
             "--postgres", "external:db.internal:5433", "--http-port", "443", "--target", "k8s", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    d = load_toml(env_file(cluster, "prod"))
    assert d["namespace"] == "acme" and d["context"] == "kind-prod" and d["release"] == "1.0.0"
    assert d["postgres"] == "external:db.internal:5433" and d["http_port"] == 443 and d["target"] == "k8s"


@pytest.mark.parametrize("argv,code,msg", [
    (["Bad"], 2, 'axon: env new: name "Bad" is not safe ([a-z0-9][a-z0-9.-]*)\n'),
    (["../x"], 2, 'axon: env new: name "../x" is not safe ([a-z0-9][a-z0-9.-]*)\n'),
    (["qa", "--tier", "staging"], 2, "axon: env new: --tier must be dev, qa, uat or prod\n"),
])
def test_env_new_validation(axon, cluster, argv, code, msg):
    before = snapshot(cluster.ctrl)
    r = axon(*(["env", "new"] + argv), cwd=cluster.ctrl)
    assert (r.code, r.err) == (code, msg)
    assert snapshot(cluster.ctrl) == before


def test_env_new_existing(axon, cluster):
    before = read(env_file(cluster, "dev"))
    r = axon("env", "new", "dev", cwd=cluster.ctrl)
    assert r.code == 1 and 'environment "dev" already exists' in r.err
    assert read(env_file(cluster, "dev")) == before


def test_env_new_check_writes_nothing(axon, cluster):
    r = axon("env", "new", "qa", "--check", cwd=cluster.ctrl)
    assert r.code == 0
    assert r.out.startswith("would write /") and r.out.endswith("/environments/qa.toml\n")
    assert not env_file(cluster, "qa").exists()


# ── env list / show ──────────────────────────────────────────────────────────


def test_env_list(axon, cluster):
    assert axon("env", "new", "qa", "--tier", "qa", "--release", "0.1.0", cwd=cluster.ctrl).code == 0
    r = axon("env", "list", cwd=cluster.ctrl)
    assert r.code == 0
    rows = [l.split() for l in r.out.splitlines()]
    assert rows[0] == ["ENV", "TIER", "TARGET", "NAMESPACE", "RELEASE"]
    assert rows[1:] == [["dev", "dev", "docker", "acme-dev", "local"], ["qa", "qa", "auto", "acme-qa", "0.1.0"]]
    d = json.loads(axon("env", "list", "--json", cwd=cluster.ctrl).out)
    assert [(e["name"], e["tier"], e["release"]) for e in d] == [("dev", "dev", "local"), ("qa", "qa", "0.1.0")]


def test_env_list_skips_overrides_files(axon, cluster):
    assert axon("env", "set", "dev", "app.web.K=v", cwd=cluster.ctrl).code == 0
    d = json.loads(axon("env", "list", "--json", cwd=cluster.ctrl).out)
    assert [e["name"] for e in d] == ["dev"]


def test_env_list_empty(axon, cluster):
    os.remove(str(env_file(cluster, "dev")))
    r = axon("env", "list", cwd=cluster.ctrl)
    assert (r.code, r.out) == (0, "")
    assert "no environments yet" in r.err


def test_env_show(axon, cluster):
    assert axon("env", "set", "dev", "app.web.LOG_LEVEL=debug", "image.reg.example/a:1=mirror.local/a:1", cwd=cluster.ctrl).code == 0
    r = axon("env", "show", "dev", cwd=cluster.ctrl)
    assert r.code == 0
    rows = dict(l.split(None, 1) for l in r.out.split("\n\n")[0].splitlines())
    assert rows["name"] == "dev" and rows["namespace"] == "acme-dev" and rows["target"] == "docker"
    assert rows["file"] == "environments/dev.toml"
    assert "  app.web.LOG_LEVEL = debug" in r.out
    assert "  image reg.example/a:1 → mirror.local/a:1" in r.out
    d = json.loads(axon("env", "show", "dev", "--json", cwd=cluster.ctrl).out)
    assert d["env"]["name"] == "dev" and d["env"]["release"] == "local"
    assert d["overrides"]["AppEnv"] == {"web": {"LOG_LEVEL": "debug"}}


def test_env_show_missing(axon, cluster):
    r = axon("env", "show", "nope", cwd=cluster.ctrl)
    assert r.code == 1
    assert 'environment "nope" not found' in r.err and "axon env new nope" in r.err


# ── env set / unset ──────────────────────────────────────────────────────────


def test_env_set_keys_and_bare_reads(axon, cluster):
    assert axon("env", "new", "qa", "--tier", "qa", cwd=cluster.ctrl).code == 0
    r = axon("env", "set", "qa", "release=0.2.1", "domain=qa.example.test", "http-port=8443", "replicas=2",
             "tls=true", "gate.l2=required", "context=kind-qa", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    assert "✓ environments/qa.toml: release = 0.2.1" in r.err
    d = load_toml(env_file(cluster, "qa"))
    assert d["release"] == "0.2.1" and d["domain"] == "qa.example.test"
    assert d["http_port"] == 8443 and d["replicas"] == 2 and d["tls"] is True
    assert d["gates"] == {"l2": "required"} and d["context"] == "kind-qa"
    r = axon("env", "set", "qa", "release", "domain", "tier", cwd=cluster.ctrl)
    assert r.out == "0.2.1\nqa.example.test\nqa\n"


def test_env_set_keeps_comments(axon, cluster):
    assert axon("env", "set", "dev", "target=k8s", cwd=cluster.ctrl).code == 0
    text = read(env_file(cluster, "dev"))
    assert text.startswith("# environments/dev.toml — local development")
    assert 'target    = "k8s"  # docker | k8s' in text


def test_env_set_app_override_goes_to_the_overrides_file(axon, cluster):
    before = read(env_file(cluster, "dev"))
    r = axon("env", "set", "dev", "app.web.LOG_LEVEL=debug", "app.web.MODE=strict", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    assert read(env_file(cluster, "dev")) == before
    assert "✓ environments/dev.overrides.toml: app.web.LOG_LEVEL = debug" in r.err
    d = load_toml(overrides(cluster, "dev"))
    assert d["app_env"] == {"web": {"LOG_LEVEL": "debug", "MODE": "strict"}}
    assert read(overrides(cluster, "dev")).startswith("# environments/dev.overrides.toml — per-environment overrides.")


def test_env_set_override_for_a_dotted_app_name(axon, cluster):
    assert axon("cluster", "add", "app", "--name", "acme.portal", "--from-image", "reg.example/p:1", cwd=cluster.ctrl).code == 0
    assert axon("env", "set", "dev", "app.acme.portal.MODE=strict", cwd=cluster.ctrl).code == 0
    assert load_toml(overrides(cluster, "dev"))["app_env"] == {"acme.portal": {"MODE": "strict"}}


def test_env_set_image_override(axon, cluster):
    assert axon("env", "set", "dev", "image.ghcr.io/x/y:1=mirror.local/y:1", cwd=cluster.ctrl).code == 0
    assert load_toml(overrides(cluster, "dev"))["images"] == {"ghcr.io/x/y:1": "mirror.local/y:1"}


def test_override_reaches_the_generated_stack(axon, web_cluster):
    assert axon("env", "set", "dev", "app.web.LOG_LEVEL=debug", cwd=web_cluster.ctrl).code == 0
    assert axon("cluster", "generate", cwd=web_cluster.ctrl).code == 0
    from _util import load_yaml
    env = load_yaml(web_cluster.app_compose("web"))["services"]["web_mod"]["environment"]
    assert env["LOG_LEVEL"] == "debug"


@pytest.mark.parametrize("kv,msg", [
    ("release=../../etc", "environment release assignment requires safe environment and version names"),
    ("release=Bad Version", "environment release assignment requires safe environment and version names"),
    ("tier=gold", "tier must be dev, qa, uat or prod"),
    ("target=nomad", "target must be docker or k8s"),
    ("http-port=abc", "http-port wants a number"),
    ("tls=maybe", "tls wants true or false"),
    ("nonsense=1", 'unknown env key "nonsense"'),
    ("app.web=1", "app keys look like app.<app>.<KEY>=value"),
])
def test_env_set_rejects_bad_values(axon, cluster, kv, msg):
    before = snapshot(cluster.ctrl)
    r = axon("env", "set", "dev", kv, cwd=cluster.ctrl)
    assert (r.code, r.err) == (1, "axon: %s\n" % msg)
    assert snapshot(cluster.ctrl) == before


def test_env_set_check_changes_nothing(axon, cluster):
    before = snapshot(cluster.ctrl)
    r = axon("env", "set", "dev", "--check", "release=9.9.9", "app.web.X=1", cwd=cluster.ctrl)
    assert (r.code, r.out) == (0, "would set release = 9.9.9\nwould set app.web.X = 1\n")
    assert snapshot(cluster.ctrl) == before


def test_env_set_needs_a_pair(axon, cluster):
    r = axon("env", "set", "dev", cwd=cluster.ctrl)
    assert (r.code, r.err) == (2, "axon: env set: missing required argument(s): key=value\n")


def test_env_unset(axon, cluster):
    assert axon("env", "set", "dev", "http_port=9000", "gate.l1=required", "app.web.A=1", "app.web.B=2", cwd=cluster.ctrl).code == 0
    r = axon("env", "unset", "dev", "http_port", "gate.l1", "app.web.A", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    d = load_toml(env_file(cluster, "dev"))
    assert "http_port" not in d and d.get("gates", {}) == {}
    assert load_toml(overrides(cluster, "dev"))["app_env"] == {"web": {"B": "2"}}
    r = axon("env", "unset", "dev", "http_port", cwd=cluster.ctrl)
    assert (r.code, r.err) == (1, 'axon: key "http_port" is not set\n')


def test_env_unset_check(axon, cluster):
    before = snapshot(cluster.ctrl)
    r = axon("env", "unset", "dev", "--check", "target", cwd=cluster.ctrl)
    assert (r.code, r.out) == (0, "would unset target\n")
    assert snapshot(cluster.ctrl) == before


# ── env check ────────────────────────────────────────────────────────────────


def findings(axon, cluster, env="dev"):
    r = axon("env", "check", env, "--json", cwd=cluster.ctrl)
    return r.code, json.loads(r.out)["findings"] or []


def ids(fs):
    return [f["id"] for f in fs]


def test_env_check_clean(axon, web_cluster):
    r = axon("env", "check", "dev", cwd=web_cluster.ctrl)
    assert (r.code, r.out) == (0, "✓ 0 blocking · 0 risk · 0 note\n")


def test_env_check_json_clean_is_an_empty_list(axon, web_cluster):
    r = axon("env", "check", "dev", "--json", cwd=web_cluster.ctrl)
    assert r.code == 0
    assert json.loads(r.out) == {"findings": []}


def test_env_check_reports_missing_env_values(axon, web_cluster):
    assert axon("cluster", "add", "app", "--name", "api", "--kind", "project", "--from-image", "reg.example/api:1",
                "--env", "API_TOKEN=${WEB_API_TOKEN}", cwd=web_cluster.ctrl).code == 0
    code, fs = findings(axon, web_cluster)
    assert code == 1
    assert {"id": "ENV_VALUE_MISSING", "concern": "conformance", "severity": "blocking", "subject": "${WEB_API_TOKEN}",
            "cause": "referenced by the descriptor but not set in axon secrets or .axon.infra/.env",
            "fix": "axon secrets set WEB_API_TOKEN  (or add WEB_API_TOKEN=… to .axon.infra/.env)"} in fs
    r = axon("env", "check", "dev", cwd=web_cluster.ctrl)
    assert r.code == 1 and "✗ ENV_VALUE_MISSING  ${WEB_API_TOKEN}" in r.out
    write(web_cluster.dotenv, read(web_cluster.dotenv) + "WEB_API_TOKEN=s3cret\n")
    code, fs = findings(axon, web_cluster)
    assert code == 0 and "ENV_VALUE_MISSING" not in ids(fs)


def test_env_check_reports_missing_values_from_overrides(axon, web_cluster):
    assert axon("env", "set", "dev", "app.web.PAYMENT_KEY=${PAYMENT_KEY}", cwd=web_cluster.ctrl).code == 0
    code, fs = findings(axon, web_cluster)
    assert code == 1 and [f["subject"] for f in fs if f["id"] == "ENV_VALUE_MISSING"] == ["${PAYMENT_KEY}"]
    # another environment without that override is unaffected
    assert axon("env", "new", "dev2", cwd=web_cluster.ctrl).code == 0
    assert findings(axon, web_cluster, "dev2")[0] == 0


def test_env_check_unknown_override_app(axon, web_cluster):
    assert axon("env", "set", "dev", "app.ghost.X=1", cwd=web_cluster.ctrl).code == 0
    code, fs = findings(axon, web_cluster)
    assert code == 0
    f = [f for f in fs if f["id"] == "CONFORMANCE_ENV_UNKNOWN_SERVICE"]
    assert f and f[0]["severity"] == "risk"


def test_env_check_release_rules(axon, web_cluster):
    assert axon("env", "new", "qa", "--tier", "qa", cwd=web_cluster.ctrl).code == 0
    code, fs = findings(axon, web_cluster, "qa")
    assert code == 1 and "ENV_RELEASE_REQUIRED" in ids(fs)
    assert axon("env", "set", "qa", "release=0.3.0", cwd=web_cluster.ctrl).code == 0
    code, fs = findings(axon, web_cluster, "qa")
    assert code == 1 and "ENV_RELEASE_MISSING" in ids(fs) and "ENV_RELEASE_REQUIRED" not in ids(fs)
    write(web_cluster.ctrl / "releases" / "0.3.0.toml", 'version = "0.3.0"\n')
    code, fs = findings(axon, web_cluster, "qa")
    assert code == 0 and not {"ENV_RELEASE_MISSING", "ENV_RELEASE_REQUIRED"} & set(ids(fs))


def test_env_check_core_image_required_for_cored_apps(axon, cluster):
    assert axon("cluster", "add", "app", "--name", "web", "--from-image", "reg.example/w:1", cwd=cluster.ctrl).code == 0
    code, fs = findings(axon, cluster)
    assert code == 1 and "CONFORMANCE_CORE_IMAGE_MISSING" in ids(fs)


def test_env_check_missing_dev_file_uses_defaults(axon, web_cluster):
    os.remove(str(env_file(web_cluster, "dev")))
    code, fs = findings(axon, web_cluster)
    assert code == 0
    assert [f for f in fs if f["id"] == "ENV_FILE_MISSING"][0]["severity"] == "note"


def test_env_check_unknown_env(axon, cluster):
    r = axon("env", "check", "qa", cwd=cluster.ctrl)
    assert r.code == 1 and 'environment "qa" not found' in r.err


def test_env_check_account_id_is_ignored(axon, web_cluster):
    write(env_file(web_cluster, "dev"), read(env_file(web_cluster, "dev")) + 'account_id = "123456"\n')
    code, fs = findings(axon, web_cluster)
    assert code == 0 and "ENV_ACCOUNT_IGNORED" in ids(fs)


# ── env diagnose (offline concerns only) ─────────────────────────────────────


def test_env_diagnose_offline_concerns(axon, cluster):
    assert axon("cluster", "add", "app", "--name", "web", "--from-image", "reg.example/w:1", cwd=cluster.ctrl).code == 0
    r = axon("env", "diagnose", "dev", "--json", "--concern", "conformance,security", "--concern", "business,devx", cwd=cluster.ctrl)
    d = json.loads(r.out)
    assert set(d) == {"findings", "notChecked"}
    assert d["notChecked"] == []
    got = ids(d["findings"])
    assert "CONFORMANCE_CORE_IMAGE_MISSING" in got and "SECURITY_UNPINNED_IMAGE" in got and "BUSINESS_NEVER_VERIFIED" in got
    assert {f["concern"] for f in d["findings"]} <= {"conformance", "security", "business", "devx"}
    assert r.code == 1  # CONFORMANCE_CORE_IMAGE_MISSING is blocking


def test_env_diagnose_unknown_concern(axon, cluster):
    r = axon("env", "diagnose", "dev", "--concern", "bogus", cwd=cluster.ctrl)
    assert r.code == 2
    assert r.err == ('axon: env diagnose: unknown concern "bogus" '
                     "(toolchain, conformance, function, performance, business, security, devx)\n")


def test_env_diagnose_reports_literal_secrets(axon, web_cluster):
    assert axon("cluster", "add", "app", "--name", "api", "--kind", "project", "--from-image", "reg.example/api:1",
                "--env", "DB_PASSWORD=hunter2", cwd=web_cluster.ctrl).code == 0
    d = json.loads(axon("env", "diagnose", "dev", "--json", "--concern", "security", cwd=web_cluster.ctrl).out)
    f = [f for f in d["findings"] if f["id"] == "SECURITY_SECRETS_IN_ENV"]
    assert f and "DB_PASSWORD" in f[0]["cause"] and f[0]["fix"] == 'set DB_PASSWORD = "${DB_PASSWORD}" and store the value with `axon secrets set DB_PASSWORD`'


# ── env plan / up / down without docker ──────────────────────────────────────


def test_env_plan_rejects_unknown_target(axon, cluster):
    r = axon("env", "plan", "dev", "--target", "nomad", cwd=cluster.ctrl)
    assert (r.code, r.err) == (1, 'axon: unknown target "nomad" (docker or k8s)\n')


def test_env_plan_rejects_bad_timeout(axon, cluster):
    r = axon("env", "plan", "dev", "--timeout", "soon", cwd=cluster.ctrl)
    assert (r.code, r.err) == (2, 'axon: env plan: --timeout wants a duration like 10m, got "soon"\n')


def test_env_plan_unknown_env(axon, cluster):
    r = axon("env", "plan", "staging", cwd=cluster.ctrl)
    assert r.code == 1 and 'environment "staging" not found' in r.err


def test_build_is_refused_for_a_non_dev_tier(axon, cluster):
    assert axon("env", "new", "qa", "--tier", "qa", "--release", "1.2.3", "--target", "docker", cwd=cluster.ctrl).code == 0
    r = axon("up", "--env", "qa", "--build", "--dry-run", cwd=cluster.ctrl)
    assert r.code == 1
    assert "--build deploys the working tree, which only a dev-tier environment may do (qa is qa, release 1.2.3)" in r.err


def test_plan_stops_on_blocking_descriptor_problems(axon, cluster):
    write(cluster.descriptor, cluster.text() + '\n[[app]]\nname = "ghost"\nhost = "ghost"\n')
    write(cluster.infra / "axon.toml", '[hooks]\npre_generate = ["touch hook-ran"]\n')
    before = snapshot(cluster.ctrl)
    r = axon("env", "plan", "dev", cwd=cluster.ctrl)
    assert r.code == 1
    assert "▶ plan for acme/dev → docker" in r.err
    assert "would run pre_generate hook: touch hook-ran" in r.err
    assert "✗ app ghost: app dir " in r.err
    assert "the descriptor has 1 blocking problem(s)" in r.err
    assert not (cluster.ctrl / "hook-ran").exists()  # a plan never runs hooks
    assert snapshot(cluster.ctrl) == before  # nor writes anything (no lock, no state)


def test_plan_without_a_daemon_fails_cleanly(axon, cluster):
    # DOCKER_HOST points at nothing in unmarked tests
    r = axon("up", "--dry-run", cwd=cluster.ctrl)
    assert r.code == 1
    assert "▶ plan for acme/dev → docker" in r.err
    assert "docker" in r.err.splitlines()[-1]


def test_down_dry_run_prints_the_teardown_plan(axon, web_cluster):
    before = snapshot(web_cluster.ctrl)
    r = axon("down", "--dry-run", cwd=web_cluster.ctrl)
    assert r.code == 0, r.err
    assert r.out == ("· remove every container of compose project acme and its 1 app project(s)\n"
                     "· remove network acme-net\n"
                     "· keep the shared Postgres data volume\n")
    assert "would remove compose project acme_web" in r.err
    assert "would remove fabric project acme (volumes kept)" in r.err
    assert snapshot(web_cluster.ctrl) == before
    r = axon("env", "undeploy", "dev", "--volumes", "--dry-run", cwd=web_cluster.ctrl)
    assert r.code == 0
    assert "· DELETE the data volumes (Postgres, ClickHouse, Grafana, Tempo)" in r.out
    assert "would remove fabric project acme and its volumes" in r.err


@pytest.mark.parametrize("argv", [["down", "--volumes"], ["env", "undeploy", "dev", "--volumes"]])
def test_volume_deletion_needs_confirmation(axon, cluster, argv):
    before = snapshot(cluster.ctrl)
    r = axon(*argv, cwd=cluster.ctrl)
    assert (r.code, r.err) == (1, "axon: --volumes deletes data: confirm in a terminal or pass --yes\n")
    assert snapshot(cluster.ctrl) == before


def test_run_lock_held_by_another_host_blocks(axon, cluster):
    lock = cluster.state / "run.lock"
    rec = {"verb": "up", "target": "docker", "pid": 424242, "startedAt": "2026-01-01T00:00:00Z", "host": "other-host.invalid"}
    write(lock, json.dumps(rec))
    r = axon("down", cwd=cluster.ctrl)
    assert r.code == 1
    assert "lock held by pid 424242 on other-host.invalid (up docker since 2026-01-01T00:00:00Z); pass --force to take it over" in r.err
    assert json.loads(read(lock)) == rec


# ── IAM signing key ──────────────────────────────────────────────────────────


def key_path(cluster):
    return cluster.infra / "secrets" / "iam_signing_key.pem"


def test_iam_keys(axon, cluster):
    r = axon("env", "iam-keys", "dev", cwd=cluster.ctrl)
    assert r.code == 0
    rows = dict(l.split(None, 1) for l in r.out.splitlines())
    assert rows["key"] == ".axon.infra/secrets/iam_signing_key.pem"
    assert rows["mode"] == "600"
    assert rows["fingerprint"].startswith("SHA256:") and len(rows["fingerprint"]) == len("SHA256:") + 32
    assert rows["gitignored"] == "unknown (not a git repo)"
    assert axon("env", "iam-keys", "dev", "--check", cwd=cluster.ctrl).code == 0


def test_iam_keys_check_flags_a_loose_mode(axon, cluster):
    os.chmod(str(key_path(cluster)), 0o644)
    r = axon("env", "iam-keys", "dev", "--check", cwd=cluster.ctrl)
    assert r.code == 1 and "mode         644" in r.out
    r = axon("cluster", "check", "--json", cwd=cluster.ctrl)
    assert r.code == 1
    assert "SECURITY_SIGNING_KEY_PERMISSIONS" in ids(json.loads(r.out)["findings"])


def test_iam_keys_missing_key(axon, cluster):
    os.remove(str(key_path(cluster)))
    r = axon("env", "iam-keys", "dev", cwd=cluster.ctrl)
    assert r.code == 0 and "no signing key at .axon.infra/secrets/iam_signing_key.pem" in r.out
    assert axon("env", "iam-keys", "dev", "--check", cwd=cluster.ctrl).code == 1


def test_key_rotation_lifecycle(axon, cluster):
    old = read(key_path(cluster))
    fp_old = axon("env", "iam-keys", "dev", cwd=cluster.ctrl).out
    r = axon("env", "rotate-iam-key", "dev", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    assert "next: axon env deploy dev" in r.out and "then: axon env rotate-iam-key dev --finish" in r.out
    prev = cluster.infra / "secrets" / "iam_signing_key.prev.pem"
    assert read(prev) == old and read(key_path(cluster)) != old
    assert stat.S_IMODE(os.stat(str(key_path(cluster))).st_mode) == 0o600
    shown = axon("env", "iam-keys", "dev", cwd=cluster.ctrl).out
    assert "(rotation in progress)" in shown and shown.split("fingerprint")[1].split()[0] != fp_old.split("fingerprint")[1].split()[0]
    # a second rotation is refused while one is in progress
    r = axon("env", "rotate-iam-key", "dev", cwd=cluster.ctrl)
    assert r.code == 1 and "a rotation is already in progress" in r.err
    # reset restores the previous key
    assert axon("env", "rotate-iam-key", "dev", "--reset", cwd=cluster.ctrl).code == 0
    assert read(key_path(cluster)) == old and not prev.exists()
    # rotate + finish deletes the previous key
    assert axon("env", "rotate-iam-key", "dev", cwd=cluster.ctrl).code == 0
    assert axon("env", "rotate-iam-key", "dev", "--finish", cwd=cluster.ctrl).code == 0
    assert not prev.exists()
    r = axon("env", "rotate-iam-key", "dev", "--finish", cwd=cluster.ctrl)
    assert (r.code, r.err) == (1, "axon: no rotation in progress\n")
    r = axon("env", "rotate-iam-key", "dev", "--reset", cwd=cluster.ctrl)
    assert (r.code, r.err) == (1, "axon: no previous key to restore\n")


def test_rotated_previous_key_stays_out_of_git(axon, cluster, git):
    git(cluster.ctrl, "init", "-q")
    assert axon("env", "rotate-iam-key", "dev", cwd=cluster.ctrl).code == 0
    status = git(cluster.ctrl, "status", "--porcelain", "--untracked-files=all")
    assert "iam_signing_key.prev.pem" not in status, status


def test_env_list_json_empty_is_an_empty_list(axon, cluster):
    os.remove(str(env_file(cluster, "dev")))
    r = axon("env", "list", "--json", cwd=cluster.ctrl)
    assert r.code == 0
    assert json.loads(r.out) == []
