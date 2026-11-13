"""Journeys: `env test --list/--lint` (offline) and real runs against a tiny in-process HTTP
server standing in for the Traefik front door (Host-header routing on 127.0.0.1:<port>).
No docker: the cluster's [fabric.traefik] http port is pointed at the test server."""

import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

try:
    from http.server import ThreadingHTTPServer
except ImportError:  # pragma: no cover
    ThreadingHTTPServer = HTTPServer

import pytest

from _util import read, write

DATA = {
    "name": "web",
    "count": 2,
    "ratio": 0.5,
    "ok": True,
    "flag": "true",
    "nothing": None,
    "modules": [
        {"token": "a", "health": "DOWN", "meta": {"v": 1}},
        {"token": "b", "health": "SERVING", "meta": {"v": 2}},
    ],
    "tags": ["x", "y", "z"],
}


class FrontDoor(BaseHTTPRequestHandler):
    """Routes on the Host header, like Traefik would."""

    def log_message(self, *a):
        pass

    def _send(self, code, body, ctype="application/json", headers=None):
        raw = body if isinstance(body, bytes) else (json.dumps(body) if ctype == "application/json" else body).encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("X-Seen-Host", self.headers.get("Host", ""))
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def _route(self, body=None):
        host = self.headers.get("Host", "")
        path = self.path
        if not host.startswith(("web.lvh.me", "web.example.test")):
            return self._send(404, {"error": "unknown host " + host})
        if path == "/healthz":
            return self._send(200, "ok", "text/plain")
        if path == "/api/system":
            return self._send(200, {"modules": [{"token": "web_mod", "health": "SERVING"}]})
        if path == "/data":
            return self._send(200, DATA)
        if path == "/text":
            return self._send(200, "hello journey world", "text/plain")
        if path.startswith("/status/"):
            return self._send(int(path.rsplit("/", 1)[1]), {"status": path})
        if path == "/redirect":
            return self._send(302, "", "text/plain", {"Location": "/data"})
        if path.startswith("/echo"):
            return self._send(200, {
                "method": self.command, "path": path, "host": host,
                "auth": self.headers.get("Authorization", ""),
                "ctype": self.headers.get("Content-Type", ""),
                "body": json.loads(body) if body else None,
            })
        return self._send(404, {"error": "no route " + path})

    def do_GET(self):
        self._route()

    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0") or 0)
        self._route(self.rfile.read(n).decode() if n else None)

    do_PUT = do_POST
    do_DELETE = do_GET


@pytest.fixture(scope="module")
def front_door():
    srv = ThreadingHTTPServer(("127.0.0.1", 0), FrontDoor)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    yield srv.server_address[1]
    srv.shutdown()
    srv.server_close()


@pytest.fixture
def jcluster(axon, web_cluster, front_door):
    """web_cluster whose front door (traefik http port) is the in-process test server."""
    r = axon("cluster", "set", "fabric.traefik.http=%d" % front_door, cwd=web_cluster.ctrl)
    assert r.code == 0, r.err
    return web_cluster


def journey(cluster, name, steps, app="web", meta=""):
    body = '[journey]\nname = "%s"\napp = "%s"\n%s\n' % (name, app, meta)
    for s in steps:
        body += "\n[[step]]\n" + s.strip() + "\n"
    write(cluster.journeys / ("%s.toml" % name), body)


def run(axon, cluster, *names, **kw):
    args = ["env", "test", "dev", "--skip-probe", "--json"]
    for n in names:
        args += ["--journey", n]
    r = axon(*args, cwd=cluster.ctrl, env=kw.get("env"))
    assert r.out, r.err
    return r, json.loads(r.out)


def only(axon, cluster, name, steps, **kw):
    journey(cluster, name, steps)
    r, lr = run(axon, cluster, name, **kw)
    assert len(lr["journeys"]) == 1
    return r, lr["journeys"][0]


# ── offline: --list and --lint ───────────────────────────────────────────────


def test_list_is_offline(axon, web_cluster):
    journey(web_cluster, "checkout", ['name = "a"\npath = "/"', 'name = "b"\npath = "/x"', 'name = "c"\npath = "/y"'])
    r = axon("env", "test", "dev", "--list", cwd=web_cluster.ctrl)
    assert r.code == 0
    lines = r.out.splitlines()
    assert [l.split()[0] for l in lines] == ["checkout", "web_smoke"]
    assert lines[0].split()[1:] == ["app=web", "3", "step(s)"]
    assert lines[1].split()[1:] == ["app=web", "2", "step(s)"]
    r = axon("env", "test", "dev", "--list", "--journey", "checkout.toml", cwd=web_cluster.ctrl)
    assert [l.split()[0] for l in r.out.splitlines()] == ["checkout"]


def test_lint_accepts_good_journeys(axon, web_cluster):
    journey(web_cluster, "good", ['name = "post"\nmethod = "post"\npath = "/m/x"', 'name = "del"\nmethod = "DELETE"\npath = "/m/x/1"'])
    r = axon("env", "test", "dev", "--lint", cwd=web_cluster.ctrl)
    assert (r.code, r.out) == (0, "")
    assert "✓ 2 journey(s) valid" in r.err


def test_lint_reports_every_problem(axon, web_cluster):
    journey(web_cluster, "bad", ['name = "s1"\npath = "relative"\nmethod = "FETCH"'], app="nohost")
    write(web_cluster.journeys / "empty.toml", '[journey]\nname = "empty"\n')
    r = axon("env", "test", "dev", "--lint", cwd=web_cluster.ctrl)
    assert r.code == 1
    assert r.out.splitlines() == [
        '✗ bad.toml: app "nohost" is not a routed host of this cluster',
        "✗ bad.toml: step 1 (s1): path must start with /",
        '✗ bad.toml: step 1: unknown method "FETCH"',
        "✗ empty.toml: [journey].app is required",
        "✗ empty.toml: no [[step]]",
    ]


def test_lint_accepts_routed_fabric_hosts(axon, web_cluster):
    assert "keycloak" in json.dumps(read(web_cluster.routes))
    journey(web_cluster, "login_page", ['name = "realm"\npath = "/realms/acme"'], app="keycloak")
    r = axon("env", "test", "dev", "--lint", "--journey", "login_page", cwd=web_cluster.ctrl)
    assert r.code == 0, r.out


def test_unparseable_journey_is_an_error(axon, web_cluster):
    write(web_cluster.journeys / "broken.toml", "[journey\nname = ")
    r = axon("env", "test", "dev", "--list", cwd=web_cluster.ctrl)
    assert r.code == 1 and "broken.toml" in r.err


# ── runs against the test front door ─────────────────────────────────────────


def test_scaffolded_smoke_journey_passes_with_probe_gate(axon, jcluster):
    r = axon("env", "test", "dev", "--json", cwd=jcluster.ctrl)
    assert r.code == 0, r.err
    lr = json.loads(r.out)
    assert (lr["cluster"], lr["env"], lr["pass"]) == ("acme", "dev", True)
    port = jcluster.toml()["fabric"]["traefik"]["http"]
    assert lr["probes"] == [{"app": "web", "kind": "core", "url": "http://web.lvh.me:%d/api/system" % port,
                             "status": 200, "pass": True,
                             "report": {"modules": [{"token": "web_mod", "health": "SERVING"}]}}]
    assert [j["name"] for j in lr["journeys"]] == ["web smoke"]
    assert [s["name"] for s in lr["journeys"][0]["steps"]] == ["core alive", "modules are serving"]
    assert "▶ probe gate" in r.err and "✓ verified acme/dev → verify/last-run.json" in r.err
    saved = json.loads(read(jcluster.ctrl / "verify" / "last-run.json"))
    assert saved == lr


def test_text_output_and_last_run(axon, jcluster):
    r = axon("env", "test", "dev", "--skip-probe", cwd=jcluster.ctrl)
    assert (r.code, r.out) == (0, "")
    assert "✓ web smoke (2 step(s))" in r.err
    assert json.loads(read(jcluster.ctrl / "verify" / "last-run.json"))["probes"] in (None, [])


def test_routing_uses_the_host_header_and_domain(axon, jcluster):
    r, j = only(axon, jcluster, "host", ['name = "h"\npath = "/text"\nexpect = { headers = { "X-Seen-Host" = "web.lvh.me" } }'])
    assert j["pass"], j
    assert axon("cluster", "set", "domain=example.test", cwd=jcluster.ctrl).code == 0
    r, j = only(axon, jcluster, "host2", ['name = "h"\npath = "/text"\nexpect = { headers = { "X-Seen-Host" = "web.example.test" } }'])
    assert j["pass"], j


@pytest.mark.parametrize("step,ok,reason", [
    ('path = "/status/404"\nexpect = { status = 404 }', True, None),
    ('path = "/status/404"', False, "status 404, want 2xx"),
    ('path = "/status/500"\nexpect = { status = 200 }', False, "status 500, want 200"),
    ('path = "/status/204"', True, None),
    ('path = "/redirect"\nexpect = { status = 302 }', True, None),  # redirects are not followed
    ('path = "/text"\nexpect = { contains = "journey" }', True, None),
    ('path = "/text"\nexpect = { contains = "absent" }', False, 'body does not contain "absent"'),
    ('path = "/text"\nexpect = { json = { a = 1 } }', False, "response is not JSON"),
    ('path = "/text"\nexpect = { headers = { "X-Seen-Host" = "nope" } }', False, 'header X-Seen-Host = "web.lvh.me", want "nope"'),
])
def test_status_contains_and_header_expectations(axon, jcluster, step, ok, reason):
    r, j = only(axon, jcluster, "exp", ['name = "s"\n' + step])
    assert j["pass"] is ok, j
    assert r.code == (0 if ok else 1)
    if reason:
        assert j["steps"][0]["reason"] == reason
        assert '✗ exp — step "s": %s' % reason in r.err


@pytest.mark.parametrize("pattern,ok,where", [
    ('{ name = "web" }', True, None),
    ('{ name = "web", count = 2 }', True, None),  # subset: extra keys in the response are fine
    ("{ count = 2.0 }", True, None),  # numbers compare numerically
    ("{ ratio = 0.5, ok = true }", True, None),
    ("{ missing = 1 }", False, "$.missing"),
    ('{ name = "other" }', False, "$.name"),
    ("{ count = 3 }", False, "$.count"),
    ('{ modules = [ { health = "SERVING" } ] }', True, None),  # matches the 2nd element
    ('{ modules = [ { health = "SERVING" }, { health = "DOWN" } ] }', True, None),
    ('{ modules = [ { health = "STARTING" } ] }', False, "$.modules[0]"),
    ('{ modules = [ { meta = { v = 2 }, token = "b" } ] }', True, None),  # nested, one element satisfies all
    ('{ modules = [ { meta = { v = 2 }, token = "a" } ] }', False, "$.modules[0]"),  # no single element does
    ('{ tags = [ "z", "x" ] }', True, None),  # order does not matter
    ('{ tags = [ "q" ] }', False, "$.tags[0]"),
    ("{ tags = [] }", True, None),
    ('{ modules = { health = "SERVING" } }', False, "$.modules"),  # an object pattern never matches an array
    ('{ name = [ "web" ] }', False, "$.name"),  # an array pattern never matches a scalar
])
def test_json_subset_matcher(axon, jcluster, pattern, ok, where):
    r, j = only(axon, jcluster, "match", ['name = "m"\npath = "/data"\nexpect = { json = %s }' % pattern])
    assert j["pass"] is ok, (pattern, j)
    if where:
        assert j["steps"][0]["reason"] == "json mismatch at " + where


@pytest.mark.parametrize("pattern", ['{ count = "2" }', "{ flag = true }"])
def test_json_matcher_is_type_strict(axon, jcluster, pattern):
    r, j = only(axon, jcluster, "strict", ['name = "m"\npath = "/data"\nexpect = { json = %s }' % pattern])
    assert j["pass"] is False


def test_a_journey_stops_at_its_first_failing_step(axon, jcluster):
    r, j = only(axon, jcluster, "multi", [
        'name = "one"\npath = "/healthz"',
        'name = "two"\npath = "/status/503"',
        'name = "three"\npath = "/healthz"',
    ])
    assert r.code == 1 and j["pass"] is False
    assert [(s["name"], s["pass"]) for s in j["steps"]] == [("one", True), ("two", False)]
    assert j["steps"][1]["status"] == 503


def test_variables_expand_in_path_headers_and_body(axon, jcluster):
    write(jcluster.dotenv, read(jcluster.dotenv) + "JOURNEY_TOKEN=tok-from-dotenv\nJOURNEY_ID=42\n")
    step = '''name = "echo"
method = "post"
path = "/echo/${JOURNEY_ID}"
headers = { Authorization = "Bearer ${JOURNEY_TOKEN}" }
body = { user = "${JOURNEY_TOKEN}", n = 1 }
expect = { json = { method = "POST", path = "/echo/42", auth = "Bearer tok-from-dotenv", ctype = "application/json", body = { user = "tok-from-dotenv", n = 1 } } }'''
    r, j = only(axon, jcluster, "vars", [step])
    assert j["pass"], j
    # the process environment wins over .env
    r, j = only(axon, jcluster, "vars", [step.replace("tok-from-dotenv", "tok-from-env")],
                env={"JOURNEY_TOKEN": "tok-from-env"})
    assert j["pass"], j


def test_journey_filter_runs_only_the_named_journeys(axon, jcluster):
    journey(jcluster, "fails", ['name = "x"\npath = "/status/500"'])
    r, lr = run(axon, jcluster, "web_smoke")
    assert r.code == 0 and [j["name"] for j in lr["journeys"]] == ["web smoke"]
    r, lr = run(axon, jcluster)
    assert r.code == 1 and lr["pass"] is False
    assert {j["name"]: j["pass"] for j in lr["journeys"]} == {"fails": False, "web smoke": True}


def test_unreachable_front_door_fails_the_journey(axon, web_cluster):
    from _util import first_free_port
    port = first_free_port(41000)
    assert axon("cluster", "set", "fabric.traefik.http=%d" % port, cwd=web_cluster.ctrl).code == 0
    r, lr = run(axon, web_cluster, "web_smoke")
    assert r.code == 1
    step = lr["journeys"][0]["steps"][0]
    assert step["pass"] is False and step["status"] == 0 and step["reason"]
