"""Standalone AI advisory service (stdlib only).

Exposes:
  GET  /healthz
  POST /v1/ai/migration-review   {workload_id, migration_id, target_provider}
  GET  /v1/ai/migration-review?migration_id=...&target_provider=...[&workload_id=...]

The service is stateless and advisory-only: it reads control-plane GET
endpoints, asks the configured model provider for a structured review,
validates the answer, and returns the envelope. It never writes to the
control plane, never calls mutation endpoints, never touches cloud APIs.

Run:  python3 -m skybridge_ai.server   (SKYBRIDGE_AI_PORT, default 18082)
"""
import json
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from . import config as _cfg
from .review import build_review_envelope

MAX_BODY_BYTES = 64 * 1024

# Local console CORS: exact allowlist only (never "*"). No credentials;
# the service uses no cookies/auth headers. Mirrors apps/control-plane/cors.go.
CORS_ALLOWED_ORIGINS = frozenset({
    "http://localhost:3000",
    "http://127.0.0.1:3000",
})
CORS_ALLOW_METHODS = "GET, HEAD, OPTIONS"


class Handler(BaseHTTPRequestHandler):
    server_version = "SkybridgeAI/1.0"

    def log_message(self, fmt, *args):  # quiet; never log bodies/keys
        pass

    def _cors_origin(self):
        origin = self.headers.get("Origin")
        if origin in CORS_ALLOWED_ORIGINS:
            return origin
        return None

    def _send(self, status: int, payload: dict):
        body = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        origin = self._cors_origin()
        if origin is not None:
            self.send_header("Access-Control-Allow-Origin", origin)
            self.send_header("Vary", "Origin")
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _error(self, status: int, code: str, message: str, request_id: str = ""):
        self._send(status, {"error": {"code": code, "message": message,
                                      "request_id": request_id, "details": {}}})

    def do_OPTIONS(self):
        self.send_response(204)
        origin = self._cors_origin()
        if origin is not None:
            self.send_header("Access-Control-Allow-Origin", origin)
            self.send_header("Vary", "Origin")
        self.send_header("Access-Control-Allow-Methods", CORS_ALLOW_METHODS)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_HEAD(self):
        parsed = urllib.parse.urlparse(self.path)
        if parsed.path == "/healthz":
            body = json.dumps({"status": "ok", "service": "skybridge-ai"}).encode("utf-8")
            self.send_response(200)
            origin = self._cors_origin()
            if origin is not None:
                self.send_header("Access-Control-Allow-Origin", origin)
                self.send_header("Vary", "Origin")
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            return
        self.send_response(404)
        origin = self._cors_origin()
        if origin is not None:
            self.send_header("Access-Control-Allow-Origin", origin)
            self.send_header("Vary", "Origin")
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_GET(self):
        parsed = urllib.parse.urlparse(self.path)
        if parsed.path == "/healthz":
            self._send(200, {"status": "ok", "service": "skybridge-ai"})
            return
        if parsed.path == "/v1/ai/migration-review":
            qs = urllib.parse.parse_qs(parsed.query)
            migration_id = (qs.get("migration_id") or [""])[0]
            target_provider = (qs.get("target_provider") or ["azure"])[0]
            workload_id = (qs.get("workload_id") or [""])[0]
            if not migration_id:
                self._error(400, "VALIDATION_FAILED", "migration_id query parameter is required")
                return
            if not workload_id:
                workload_id = _resolve_workload(migration_id)
                if workload_id is None:
                    self._error(502, "EVIDENCE_UNAVAILABLE",
                                "control plane unreachable; cannot resolve workload_id")
                    return
                if workload_id == "":
                    self._error(404, "NOT_FOUND", "migration not found")
                    return
            status, envelope = build_review_envelope(
                workload_id=workload_id, migration_id=migration_id,
                target_provider=target_provider)
            self._send(status, envelope)
            return
        self._error(404, "NOT_FOUND", "not found")

    def do_POST(self):
        parsed = urllib.parse.urlparse(self.path)
        if parsed.path != "/v1/ai/migration-review":
            self._error(404, "NOT_FOUND", "not found")
            return
        length = int(self.headers.get("Content-Length") or 0)
        if length <= 0 or length > MAX_BODY_BYTES:
            self._error(400, "VALIDATION_FAILED", "request body required (max 64KiB)")
            return
        try:
            payload = json.loads(self.rfile.read(length).decode("utf-8"))
        except Exception:
            self._error(400, "VALIDATION_FAILED", "invalid JSON")
            return
        if not isinstance(payload, dict):
            self._error(400, "VALIDATION_FAILED", "object body required")
            return
        workload_id = payload.get("workload_id") or ""
        migration_id = payload.get("migration_id") or ""
        target_provider = payload.get("target_provider") or ""
        if not workload_id or not migration_id or not target_provider:
            self._error(400, "VALIDATION_FAILED",
                        "workload_id, migration_id and target_provider are required")
            return
        status, envelope = build_review_envelope(
            workload_id=workload_id, migration_id=migration_id,
            target_provider=target_provider)
        self._send(status, envelope)


def _resolve_workload(migration_id: str):
    """Resolve workload_id via control-plane GET (read-only)."""
    base = _cfg.control_plane_url()
    try:
        req = urllib.request.Request(base + "/v1/migrations/" + urllib.parse.quote(migration_id),
                                     method="GET", headers={"Accept": "application/json"})
        with urllib.request.urlopen(req, timeout=5) as resp:
            data = json.loads(resp.read().decode("utf-8"))
        return data.get("workload_id") or ""
    except urllib.error.HTTPError as e:
        if e.code == 404:
            return ""
        return None
    except Exception:
        return None


def main():
    port = _cfg.service_port()
    server = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    print(f"skybridge-ai listening on 127.0.0.1:{port} (provider={_cfg.provider_name()})", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
