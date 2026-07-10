from fastapi.testclient import TestClient

from orpheus_workers.control_plane import create_app


def test_health() -> None:
    client = TestClient(create_app())
    r = client.get("/health")
    assert r.status_code == 200
    body = r.json()
    assert body["status"] == "ok"
    assert "version" in body


def test_ready() -> None:
    client = TestClient(create_app())
    r = client.get("/ready")
    assert r.status_code == 200
    assert r.json() == {"status": "ready"}


def test_metrics() -> None:
    client = TestClient(create_app())
    r = client.get("/metrics")
    assert r.status_code == 200
    assert "text/plain" in r.headers["content-type"]
    assert b"# HELP" in r.content
