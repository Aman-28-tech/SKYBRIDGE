"""CORS regression test: local console origins only, no wildcard."""
import threading
import urllib.request
from http.server import ThreadingHTTPServer

from skybridge_ai.server import Handler


def _serve():
    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server


def _get(server, origin=None, method="GET"):
    req = urllib.request.Request(
        f"http://127.0.0.1:{server.server_port}/healthz", method=method)
    if origin is not None:
        req.add_header("Origin", origin)
    try:
        with urllib.request.urlopen(req, timeout=5) as resp:
            return resp.status, dict(resp.headers)
    except Exception as e:
        if hasattr(e, "headers"):
            return e.code, dict(e.headers)
        raise


def test_console_origins_receive_cors_header():
    server = _serve()
    try:
        for origin in ("http://localhost:3000", "http://127.0.0.1:3000"):
            status, headers = _get(server, origin)
            assert status == 200
            assert headers.get("Access-Control-Allow-Origin") == origin
    finally:
        server.shutdown()


def test_unrelated_origin_gets_no_cors_header():
    server = _serve()
    try:
        status, headers = _get(server, "http://evil.example")
        assert status == 200
        assert "Access-Control-Allow-Origin" not in headers
    finally:
        server.shutdown()


def test_options_preflight():
    server = _serve()
    try:
        status, headers = _get(server, "http://localhost:3000", method="OPTIONS")
        assert status in (200, 204)
        assert headers.get("Access-Control-Allow-Origin") == "http://localhost:3000"
        allow = headers.get("Access-Control-Allow-Methods", "")
        for method in ("GET", "HEAD", "OPTIONS"):
            assert method in allow
    finally:
        server.shutdown()
