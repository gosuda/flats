#!/usr/bin/env python3
"""Exercise the checked-in Compose service on disposable data and free ports.

Usage: python3 scripts/compose-smoke.py IMAGE
Requires Docker Compose, a running Docker engine, and Python 3. Uses only a
synthetic private document (no approval decisions or external providers).
FLATS_SMOKE_PORT/FLATS_SMOKE_LOCAL_PORT default to 17878/17879.
"""

import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parent.parent


def run(*args):
    return subprocess.check_output(args, text=True, stderr=subprocess.STDOUT).strip()


def main():
    if len(sys.argv) != 2:
        raise SystemExit(f"usage: {sys.argv[0]} IMAGE")
    port = int(os.environ.get("FLATS_SMOKE_PORT", "17878"))
    local_port = int(os.environ.get("FLATS_SMOKE_LOCAL_PORT", "17879"))
    project = "flats-compose-smoke-" + uuid.uuid4().hex[:12]
    env = {**os.environ, "FLATS_IMAGE": sys.argv[1]}
    config = json.loads(subprocess.check_output(
        ["docker", "compose", "-f", str(ROOT / "compose.yaml"), "config", "--format", "json"],
        env=env, text=True,
    ))
    # Render the real service, changing only its project identity and ports so
    # the smoke test can coexist with a host's normal Flats installation.
    config["name"] = project
    for group in ("volumes", "networks"):
        for key, value in config[group].items():
            value["name"] = project + "_" + key
    service = config["services"]["flats"]
    service["command"] = ["serve", "--listen", f"0.0.0.0:{port}", "--local-addr", f"0.0.0.0:{local_port}"]
    for mapping in service["ports"]:
        new_port = port if mapping["target"] == 7878 else local_port
        mapping.update(target=new_port, published=str(new_port))
    service.setdefault("environment", {})["FLATS_URL"] = f"http://127.0.0.1:{port}"
    url = f"http://127.0.0.1:{port}"
    # Do not send loopback checks through the operator's HTTP proxy.
    http = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def get(address, host=None):
        request = urllib.request.Request(address, headers={"Host": host} if host else {})
        with http.open(request, timeout=5) as response:
            return response.read().decode()

    def document():
        return get(f"http://127.0.0.1:{local_port}/", f"compose-check.localhost:{local_port}")

    with tempfile.TemporaryDirectory(prefix=project) as work:
        path = Path(work)
        (path / "compose.json").write_text(json.dumps(config))
        command = ["docker", "compose", "-p", project, "-f", str(path / "compose.json")]

        def compose(*args):
            return run(*command, *args)

        def read_config():
            compose("cp", "flats:/data/config.json", str(path / "config.json"))
            return json.loads((path / "config.json").read_text())

        try:
            compose("up", "-d", "--wait", "--wait-timeout", "90")
            # Healthy inside the container is insufficient: both published
            # host ports must work before claiming the deployment works.
            status = json.loads(get(url + "/api/status"))["system"]
            assert status["console_url"] == url, "console URL is not the published loopback address"
            assert status["local_url"] == url, "local URL is not the published loopback address"
            assert status["mcp_url"] == url + "/mcp", "MCP URL is not the published loopback address"
            assert get(status["console_url"]), "advertised console URL is unreachable"
            assert get(url + "/"), "console is empty"
            before = read_config()
            archive = io.BytesIO()
            with tarfile.open(fileobj=archive, mode="w:gz") as bundle:
                for name, content in {
                    "index.md": b"# Compose persistence check\n",
                    "flats.json": b'{"type":"docs"}\n',
                }.items():
                    info = tarfile.TarInfo(name)
                    info.size = len(content)
                    info.mode = 0o644
                    bundle.addfile(info, io.BytesIO(content))
            request = urllib.request.Request(
                url + "/api/flats/compose-check/document", data=archive.getvalue(),
                headers={"Content-Type": "application/gzip"}, method="POST",
            )
            with http.open(request, timeout=30) as response:
                saved = json.load(response)
            assert saved["flat"]["type"] == "docs" and saved["flat"]["running"], "document did not start"
            assert saved["flat"]["visibility"] == "private", "test document is not private"
            assert "Compose persistence check" in document(), "document is not served"
            # Request, but never approve, deletion to verify the exact URL an
            # agent gives the operator. The document must remain available.
            request = urllib.request.Request(
                url + "/api/flats/compose-check", method="DELETE",
                headers={"Content-Type": "application/json"},
            )
            with http.open(request, timeout=5) as response:
                approval = json.load(response)
            assert approval["status"] == "pending_approval", "delete bypassed approval"
            assert approval["approval_url"] == url + "/approvals/" + approval["approval"]["id"]
            assert get(approval["approval_url"]), "generated approval URL is unreachable"
            container = compose("ps", "-q", "flats")
            compose("stop")
            assert run("docker", "inspect", "-f", "{{.State.ExitCode}}", container) == "0", "unclean SIGTERM exit"
            backup = path / "flats-data.tar.gz"
            # Stream into a caller-owned private file. A root helper writing
            # 0600 into a bind mount would be unreadable to Linux CI users.
            with backup.open("xb") as output:
                backup.chmod(0o600)
                subprocess.run(
                    ["docker", "run", "--rm", "--volumes-from", container + ":ro",
                     "alpine:3.22", "tar", "-czf", "-", "-C", "/data", "."],
                    stdout=output, stderr=subprocess.PIPE, text=True, check=True,
                )
            with tarfile.open(backup, "r:gz") as bundle:
                assert json.load(bundle.extractfile("./config.json")) == before, "backup has wrong host config"
                assert bundle.getmember("./flats.db").size > 0, "backup database is empty"
            compose("run", "--rm", "--no-deps", "flats", "config", "set", "system.version_quota", "23")
            compose("down")
            compose("up", "-d", "--wait", "--wait-timeout", "90")
            after = read_config()
            expected = json.loads(json.dumps(before))
            expected.setdefault("system", {})["version_quota"] = 23
            assert after == expected, "configuration/identity did not survive recreation"
            assert "Compose persistence check" in document(), "document did not survive recreation"
            print("compose-smoke: PASS (console/API/approval URLs, document, backup, offline config, recreation, SIGTERM)")
        except Exception:
            print(compose("logs", "--no-color"), file=sys.stderr)
            raise
        finally:
            # Only the unique project and synthetic volume owned by this run.
            compose("down", "--volumes", "--remove-orphans")


if __name__ == "__main__":
    try:
        main()
    except subprocess.CalledProcessError as error:
        if error.output:
            print(error.output, file=sys.stderr)
        if error.stderr:
            print(error.stderr, file=sys.stderr)
        raise
