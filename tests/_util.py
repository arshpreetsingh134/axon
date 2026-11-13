"""Plain helpers shared by the axon acceptance tests (no pytest fixtures here)."""

import hashlib
import json
import os
import pathlib
import re
import socket

try:  # Python 3.11+
    import tomllib as _toml
except ImportError:  # pragma: no cover - Python < 3.11
    import tomli as _toml

import yaml


# ── files ────────────────────────────────────────────────────────────────────


def write(path, text, mode=None):
    path = pathlib.Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    if mode is not None:
        os.chmod(str(path), mode)
    return path


def read(path):
    return pathlib.Path(path).read_text()


def load_toml(path):
    with open(str(path), "rb") as f:
        return _toml.load(f)


def loads_toml(text):
    return _toml.loads(text)


def load_yaml(path):
    with open(str(path)) as f:
        return yaml.safe_load(f)


def read_dotenv(path):
    """Minimal KEY=value reader (the scaffolded .env never quotes its generated values)."""
    out = {}
    for line in read(path).splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, v = line.split("=", 1)
        out[k.strip()] = v.strip().strip('"')
    return out


def tree(root):
    """Sorted relative paths of every file (and symlink) under root."""
    root = pathlib.Path(root)
    out = []
    for dirpath, dirnames, filenames in os.walk(str(root)):
        for n in filenames + [d for d in dirnames if os.path.islink(os.path.join(dirpath, d))]:
            out.append(os.path.relpath(os.path.join(dirpath, n), str(root)))
    return sorted(out)


def snapshot(root):
    """{relpath: bytes} of every regular file under root (to prove nothing changed)."""
    root = pathlib.Path(root)
    snap = {}
    for rel in tree(root):
        p = root / rel
        if p.is_file() and not p.is_symlink():
            snap[rel] = p.read_bytes()
    return snap


# ── a scaffolded cluster ─────────────────────────────────────────────────────


class Cluster(object):
    """Paths of a cluster scaffolded by `axon cluster new <workspace>`."""

    def __init__(self, workspace, name, brand="axon", xdg=None):
        self.workspace = pathlib.Path(workspace)
        self.name = name
        self.brand = brand
        self.xdg = xdg

    @property
    def file_name(self):
        return "mycelfile" if self.brand == "mycel" else "axonfile"

    @property
    def ctrl(self):
        return self.workspace / ("%s.%s" % (self.name, self.brand))

    @property
    def infra(self):
        return self.ctrl / (".%s.infra" % self.brand)

    @property
    def descriptor(self):
        return self.infra / self.file_name

    @property
    def state(self):
        return self.infra / (".%s" % self.brand)

    @property
    def dotenv(self):
        return self.infra / ".env"

    @property
    def journeys(self):
        return self.infra / "journeys"

    @property
    def envs(self):
        return self.ctrl / "environments"

    @property
    def fabric_compose(self):
        return self.infra / "fabric" / "docker-compose.yml"

    @property
    def routes(self):
        return self.infra / "fabric" / "traefik" / "dynamic" / "routes.yml"

    @property
    def init_sql(self):
        return self.infra / "fabric" / "postgres" / "init" / "01-databases.sql"

    def app_compose(self, app):
        return self.state / "apps" / app / "docker-compose.yml"

    def generated_files(self):
        """Every file axon generates (fabric + per-app stacks)."""
        out = []
        for base in (self.infra / "fabric", self.state / "apps"):
            if base.exists():
                out += [base / rel for rel in tree(base)]
        return out

    def registry(self):
        return ports_registry(pathlib.Path(self.xdg) / "axon" / "ports.toml")

    def toml(self):
        return load_toml(self.descriptor)

    def text(self):
        return read(self.descriptor)


def ports_registry(path):
    """Parse a ports.toml registry → {cluster: [6 ints]}."""
    reg = {}
    if not pathlib.Path(path).exists():
        return reg
    for line in read(path).splitlines():
        m = re.match(r'^\s*"?([^"=\s]+)"?\s*=\s*\[([^\]]*)\]', line)
        if m:
            reg[m.group(1)] = [int(x) for x in m.group(2).split(",")]
    return reg


def port_is_free(port):
    """Mirror axon's rule: a port is free when it binds on 127.0.0.1 and on 0.0.0.0."""
    for host in ("127.0.0.1", "0.0.0.0"):
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        try:
            s.bind((host, port))
        except OSError:
            return False
        finally:
            s.close()
    return True


def first_free_port(start, reserved=()):
    for p in range(start, start + 400):
        if p not in reserved and port_is_free(p):
            return p
    raise RuntimeError("no free port near %d" % start)


# ── release records (axon's canonical encoding, re-implemented independently) ──


def _key(k):
    return k if re.match(r"^[A-Za-z0-9_-]+$", k) else json.dumps(k)


def _val(v):
    if isinstance(v, bool):
        return "true" if v else "false"
    if isinstance(v, int):
        return str(v)
    if isinstance(v, str):
        return json.dumps(v, ensure_ascii=False)
    if isinstance(v, list):
        return "[" + ", ".join(_val(x) for x in v) + "]"
    raise TypeError("unsupported value %r" % (v,))


def canonical(doc):
    """Top-level scalars sorted by key, then tables sorted by name with sorted, 2-space indented keys."""
    scal = sorted(k for k, v in doc.items() if k != "digest" and not isinstance(v, dict))
    tabs = sorted(k for k, v in doc.items() if k != "digest" and isinstance(v, dict))
    out = ""
    for k in scal:
        out += "%s = %s\n" % (_key(k), _val(doc[k]))
    for t in tabs:
        out += "\n[%s]\n" % _key(t)
        for k in sorted(doc[t]):
            out += "  %s = %s\n" % (_key(k), _val(doc[t][k]))
    return out


def digest(doc):
    return "sha256:" + hashlib.sha256(canonical(doc).encode("utf-8")).hexdigest()


def render_release(doc, digest_value=None):
    """A record file as `axon release create` lays it out (digest after the scalars)."""
    d = digest_value if digest_value is not None else digest(doc)
    out = "# Release record (hand-made by the acceptance tests).\n"
    inserted = False
    for line in canonical(doc).split("\n"):
        if not inserted and (line == "" or line.startswith("[")):
            out += 'digest = "%s"\n' % d
            inserted = True
        out += line + "\n"
    return out.rstrip("\n") + "\n"


def release_doc(version, tool="axon/0.1.0-dev", **extra):
    doc = {
        "schema_version": 1,
        "version": version,
        "created": "2026-01-02T03:04:05Z",
        "by": "acceptance-tests",
        "commit": "",
        "tool_version": tool,
        "platforms": ["linux/arm64"],
        "images": {"acme_web-core": "acme_web-core:0a1b2c@sha256:" + "a" * 64},
        "modules": {"web/web_mod": "ghcr.io/example/web-mod:1.4"},
        "cores": {},
        "fabric": {"postgres": "postgres:16-alpine", "traefik": "traefik:v3.1"},
        "sources": {"web": "0123456789abcdef0123456789abcdef01234567"},
    }
    doc.update(extra)
    return doc
