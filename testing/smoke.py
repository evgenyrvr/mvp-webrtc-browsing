#!/usr/bin/env python3
"""Launch the real gateway with an ephemeral SQLite and check its HTTP path."""
import http.cookiejar
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


def main():
    root = Path(__file__).resolve().parents[1]
    with tempfile.TemporaryDirectory(prefix="mvp1-smoke-") as temporary:
        binary = Path(temporary) / "gateway"
        subprocess.run([str(root / "tools/go.sh"), "build", "-o", str(binary), "./cmd/gateway"], cwd=root, check=True)
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        origin = f"http://127.0.0.1:{port}"
        password = secrets.token_urlsafe(24)
        environment = {**os.environ, "MVP_ADMIN_LOGIN": "smoke-admin", "MVP_ADMIN_PASSWORD": password}
        log_path = Path(temporary) / "gateway.log"
        with log_path.open("w+") as log:
            process = subprocess.Popen([str(binary), "-db", str(Path(temporary) / "smoke.db"), "-addr", f"127.0.0.1:{port}", "-origin", origin], env=environment, stdout=log, stderr=log)
            opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
            try:
                deadline = time.monotonic() + 10
                while True:
                    try:
                        with opener.open(origin + "/healthz", timeout=1) as response:
                            assert response.status == 200
                        break
                    except (urllib.error.URLError, OSError):
                        if process.poll() is not None or time.monotonic() >= deadline:
                            raise RuntimeError("gateway did not become healthy")
                        time.sleep(0.05)
                def request(path, method="GET", data=None, csrf=""):
                    body = json.dumps(data).encode() if data is not None else None
                    headers = {"Origin": origin, "Content-Type": "application/json", "X-CSRF-Token": csrf}
                    with opener.open(urllib.request.Request(origin + path, data=body, headers=headers, method=method), timeout=5) as response:
                        return json.load(response)
                identity = request("/api/v1/login", "POST", {"login": "smoke-admin", "password": password})
                assert identity["user"]["admin"]
                applications = request("/api/v1/applications")
                assert {app["id"] for app in applications} == {"firefox", "terminal"}
                try:
                    request("/api/v1/sessions", "POST", {"application_id": "firefox"}, identity["csrf_token"])
                    raise AssertionError("unconfigured runtime accepted a launch")
                except urllib.error.HTTPError as error:
                    assert error.code == 503
                    assert json.load(error)["error"]["code"] == "runtime_unavailable"
                assert request("/api/v1/sessions") == []
                request("/api/v1/logout", "POST", csrf=identity["csrf_token"])
                with opener.open(origin + "/") as response:
                    assert "Рабочее пространство" in response.read().decode()
                print("Smoke passed: real process, SQLite, login, catalog, honest runtime status, logout, static UI.")
            finally:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
                log.seek(0)
                output = log.read()
                assert password not in output, "bootstrap secret leaked into logs"


if __name__ == "__main__":
    main()
