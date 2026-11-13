"""The per-user host-port registry: `cluster ports` and allocation by `cluster new`."""

import json
import socket

import pytest

from _util import first_free_port, ports_registry, read, write

SERVICES = ["postgres", "traefik", "temporal", "temporal-ui", "grafana", "clickhouse"]


def listen(port):
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.bind(("127.0.0.1", port))
    s.listen(1)
    return s


def test_ports_json_lists_both_registries(axon, new_cluster, xdg, tmp_path):
    new_cluster("one")
    new_cluster("two")
    write(xdg / "mycel" / "ports.toml", "# mycel's registry\nlegacy = [6545, 9090, 8233, 9233, 4000, 9123]\n")
    r = axon("cluster", "ports", "--json", cwd=tmp_path)  # works outside any cluster
    assert r.code == 0, r.err
    rows = json.loads(r.out)
    assert [(x["cluster"], x["tool"]) for x in rows] == [("legacy", "mycel"), ("one", "axon"), ("two", "axon")]
    reg = ports_registry(xdg / "axon" / "ports.toml")
    assert rows[1]["ports"] == reg["one"] and rows[2]["ports"] == reg["two"]
    assert rows[0]["ports"] == [6545, 9090, 8233, 9233, 4000, 9123]


def test_ports_table_marks_ports_in_use(axon, xdg, tmp_path):
    busy = first_free_port(42000)
    free = first_free_port(busy + 1)
    write(xdg / "axon" / "ports.toml", "demo = [%d, %d, 0, 0, 0, 0]\n" % (busy, free))
    s = listen(busy)
    try:
        r = axon("cluster", "ports", cwd=tmp_path)
    finally:
        s.close()
    assert r.code == 0
    lines = r.out.splitlines()
    assert lines[0].split() == ["CLUSTER", "TOOL"] + SERVICES
    row = lines[1].split()
    assert row[:4] == ["demo", "axon", "%d*" % busy, str(free)]
    assert lines[-1] == "(* = in use right now)"


def test_ports_json_empty_registry_is_an_empty_list(axon, tmp_path):
    r = axon("cluster", "ports", "--json", cwd=tmp_path)
    assert r.code == 0
    assert json.loads(r.out) == []


def test_allocation_skips_a_port_in_use(axon, tmp_path, xdg):
    taken = first_free_port(5545)
    s = listen(taken)
    try:
        r = axon("cluster", "new", "acme", cwd=tmp_path)
    finally:
        s.close()
    assert r.code == 0, r.err
    pg = ports_registry(xdg / "axon" / "ports.toml")["acme"][0]
    assert pg != taken and pg > taken


def test_allocation_skips_ports_reserved_by_other_clusters(axon, tmp_path, xdg):
    write(xdg / "axon" / "ports.toml", "other = [5545, 8090, 7233, 8233, 3000, 8123]\n")
    assert axon("cluster", "new", "acme", cwd=tmp_path).code == 0
    reg = ports_registry(xdg / "axon" / "ports.toml")
    assert reg["other"] == [5545, 8090, 7233, 8233, 3000, 8123]  # untouched
    assert not set(reg["acme"]) & set(reg["other"])


def test_allocation_reuses_an_existing_entry(axon, tmp_path, xdg):
    write(xdg / "axon" / "ports.toml", "acme = [15545, 18090, 17233, 18233, 13000, 18123]\n")
    assert axon("cluster", "new", "acme", cwd=tmp_path).code == 0
    assert ports_registry(xdg / "axon" / "ports.toml") == {"acme": [15545, 18090, 17233, 18233, 13000, 18123]}
    from _util import load_toml
    d = load_toml(tmp_path / "acme" / "acme.axon" / ".axon.infra" / "axonfile")
    assert d["fabric"]["postgres"]["port"] == 15545 and d["fabric"]["traefik"]["http"] == 18090


def test_registry_survives_hand_comments_and_quoted_names(axon, tmp_path, xdg):
    write(xdg / "axon" / "ports.toml", '# hand edit\n"my.cluster" = [25545, 28090, 27233, 28233, 23000, 28123]\n')
    assert axon("cluster", "new", "acme", cwd=tmp_path).code == 0
    text = read(xdg / "axon" / "ports.toml")
    assert '"my.cluster" = [25545, 28090, 27233, 28233, 23000, 28123]' in text
    assert set(ports_registry(xdg / "axon" / "ports.toml")) == {"my.cluster", "acme"}


def test_registry_lock_file_lives_next_to_the_registry(axon, tmp_path, xdg):
    assert axon("cluster", "new", "acme", cwd=tmp_path).code == 0
    assert (xdg / "axon" / "ports.toml.lock").exists()
