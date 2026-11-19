"""`axon up --module`: the fast path that rebuilds and rolls only named modules (offline: plans and errors)."""
import pytest


@pytest.fixture
def shop_cluster(cluster, axon):
    """The "acme" cluster with app "shop" scaffolded from source (one seed module) and core_image set."""
    assert axon("app", "new", str(cluster.workspace / "shop"), "--rings", "none", cwd=cluster.workspace).code == 0
    r = axon("cluster", "add", "app", "--name", "shop", "--from", "../shop", "--host", "shop", "--db", "shop", cwd=cluster.ctrl)
    assert r.code == 0, r.err
    assert axon("cluster", "set", "core_image=registry.example/core:2.0", cwd=cluster.ctrl).code == 0
    return cluster


def seed_token(axon, c):
    """The seed module's token, read from the error that lists every buildable module."""
    r = axon("up", "--module", "no-such-module", "--dry-run", "--target", "docker", cwd=c.ctrl)
    assert r.code == 1 and "no module or worker app of that name" in r.err, r.err
    listed = r.err.split("(modules: ")[1].rstrip(")\n ").split(", ")
    shop = [x for x in listed if x.startswith("shop/")]
    assert len(shop) == 1, listed
    return shop[0].split("/", 1)[1]


def test_module_plan_builds_and_rolls_only_the_module(axon, shop_cluster):
    c = shop_cluster
    tok = seed_token(axon, c)
    r = axon("up", "--module", tok, "--dry-run", "--target", "docker", cwd=c.ctrl)
    assert r.code == 0, r.err
    out = r.out + r.err
    assert "plan for module deploy acme/dev: shop/%s" % tok in out
    assert "would compile shop/%s" % tok in out
    assert "would assemble core image" in out
    assert "would recreate shop: %s, core" % tok in out
    assert "skipped (module deploy): fabric, databases, Keycloak, IAM seed" in out
    assert "fabric up" not in out and "plan complete" in out
    # app/token spelling is the same target
    r2 = axon("up", "--module", "shop/" + tok, "--dry-run", "--target", "docker", cwd=c.ctrl)
    assert r2.code == 0 and "would recreate shop: %s, core" % tok in r2.out + r2.err


def test_module_errors(axon, shop_cluster):
    c = shop_cluster
    # a module token wins over an app of the same name (the scaffold names its seed module after the app)
    tok = seed_token(axon, c)
    if tok == "shop":
        r = axon("up", "--module", "shop", "--dry-run", "--target", "docker", cwd=c.ctrl)
        assert r.code == 0 and "module deploy acme/dev: shop/shop" in r.err
    r = axon("up", "--module", "shop/nope", "--dry-run", "--target", "docker", cwd=c.ctrl)
    assert r.code == 1 and 'has no module "nope"' in r.err
    r = axon("up", "--module", "x", "--app", "shop", cwd=c.ctrl)
    assert r.code == 2 and "--module and --app cannot be combined" in r.err


@pytest.mark.docker
def test_module_needs_a_running_environment(axon, shop_cluster):
    """Without --dry-run, a module deploy refuses an environment that was never brought up."""
    c = shop_cluster
    tok = seed_token(axon, c)
    r = axon("up", "--module", tok, "--target", "docker", cwd=c.ctrl)
    assert r.code == 1 and "run a full `axon up --build` first" in r.err


def test_module_flag_is_documented(axon):
    r = axon("up", "--help")
    assert "--module <[app/]token>" in r.out and "fast path" in r.out
