#!/usr/bin/env python3
"""Black-box acceptance runner: real archive validation, HTTP, CLI and MCP.

Only disposable, loopback servers are started. No provider credentials are read.
JSON evidence is retained locally; do not commit raw server logs.
"""
import argparse
import concurrent.futures
import hashlib
import io
import json
import os
import re
from pathlib import Path
import socket
import sqlite3
import subprocess
import tarfile
import threading
import time
import traceback
import urllib.error
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
LEGACY_REVISION = "29edc2a6e13e27867603a23ff5b1b5a8d1b84df0"
REQUIRED_LANES = {"actual-binary-local", "production-provider-manager"}


def failed(row, category):
    codes = {
        "draft-drift": "stale_approval", "policy-drift": "stale_approval",
        "live-drift": "stale_approval", "visibility-drift": "stale_approval",
        "not-permitted": "provider_not_permitted", "unavailable": "provider_unavailable",
        "health": "health_check_failed", "module-init": "runtime_start_failed",
        "teardown": "public_stop_unconfirmed", "serve": "provider_not_ready",
    }
    require(row.get("status") == "failed", f"{category}: approval did not fail: {row}")
    dto = execution(row)
    require(dto.get('status') == 'failed' and dto.get('failure_code'), f"missing typed failure DTO: {row}")
    require(dto['failure_code'] == codes[category], f"{category}: unrelated failure code: {row}")
    return row


def data_impact(row, changed=False):
    dto = execution(row)
    require(dto.get('data_impact') in {'none', 'runtime_start', 'restore_data', 'unknown'}, f"invalid data-impact enum: {row}")
    require(dto.get('health_data') in {'not_run', 'isolated_copy'} and
            dto.get('live_data') in {'untouched', 'runtime_may_write', 'restored', 'unknown'}, f"invalid data scope DTO: {row}")
    require(not changed or dto["data_impact"] != "none", f"live data changed but impact is none: {row}")
    return dto["data_impact"]


def execution(row):
    require(isinstance(row, dict), f"expected typed approval DTO: {row}")
    dto = row.get('result_data', row)
    if isinstance(dto, str):
        dto = json.loads(dto)
    require(isinstance(dto, dict), f"expected object result_data: {row}")
    return dto


def provider_ids(flat):
    ids = flat.get("providers")
    require(isinstance(ids, list) and all(isinstance(p, str) for p in ids),
            f"provider DTO must be a list of canonical IDs: {ids}")
    return set(ids)


def source_identity():
    def git(*args):
        return subprocess.check_output(["git", "-C", str(ROOT), *args], text=True).strip()
    paths = ["scripts/lifecycle-gate.sh", *sorted(str(p.relative_to(ROOT)) for p in
             (ROOT / "internal/lifecyclecheck").rglob("*") if p.is_file() and p.suffix in (".py", ".go"))]
    return {"root": str(ROOT), "head": git("rev-parse", "HEAD"),
            "tree": git("rev-parse", "HEAD^{tree}"), "dirty": git("status", "--porcelain"),
            "legacy_revision": LEGACY_REVISION, "legacy_tree": git("rev-parse", LEGACY_REVISION + "^{tree}"),
            "go_version": subprocess.check_output(["go", "version"], text=True).strip(),
            "harness_sha256": {p: hashlib.sha256((ROOT / p).read_bytes()).hexdigest() for p in paths}}


def binary_identity(path):
    if not path:
        return None
    build = subprocess.check_output(["go", "version", "-m", str(path)], text=True)
    settings = dict(re.findall(r"^\s*build\s+([^=\s]+)=(.*)$", build, re.M))
    return {"path": str(Path(path).resolve()), "sha256": hashlib.sha256(Path(path).read_bytes()).hexdigest(),
            "build_info": build, "revision": settings.get("vcs.revision"), "modified": settings.get("vcs.modified")}


def acceptance_identity(source, binaries):
    return (not source["dirty"] and all(binaries.get(k) and binaries[k]["revision"] == source["head"]
            and binaries[k]["modified"] == "false" for k in ("candidate", "provider_adapter", "tagged_crash_adapter")))


def accepted(report):
    return (report["full_suite_selected"] and report["exit"] == 0 and
            report["lane_complete"] and report["identity_bound"] and
            report["human_approval"] == "proven")


def require(condition, detail):
    if not condition:
        raise AssertionError(detail)


def archive(files):
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w:gz") as tar:
        for name, text in sorted(files.items()):
            data = text.encode() if isinstance(text, str) else text
            member = tarfile.TarInfo(name)
            member.size, member.mode, member.mtime = len(data), 0o644, 0
            tar.addfile(member, io.BytesIO(data))
    return output.getvalue()


def static(marker, health="/"):
    return {"flats.json": json.dumps({"kind": "static", "health": health}),
            "index.html": "<!doctype html><h1>" + marker + "</h1>"}


def server(version, broken=False, healthy_write=False):
    # /health writes a sentinel before failure: live DB must not see this write.
    return {"flats.json": '{"kind":"server","health":"/health"}', "server.js": """
export default { async fetch(request, env) {
  env.DB.exec("CREATE TABLE IF NOT EXISTS hits (n INTEGER)");
  const path = new URL(request.url).pathname;
  if (path === "/hit") env.DB.exec("INSERT INTO hits VALUES (1)");
  if (path === "/health-file") return Response.json(env.FILES.get("health-sentinel"));
  if (path === "/health" && (BROKEN || HEALTHY_WRITE)) {
    env.DB.exec("INSERT INTO hits VALUES (999)");
    env.FILES.put("health-sentinel", "HEALTH-COPY-ONLY");
    return new Response(BROKEN ? "not healthy" : "healthy", {status:BROKEN ? 503 : 200});
  }
  const [{c}] = env.DB.query("SELECT count(*) AS c FROM hits");
  return Response.json({version: VERSION, hits:c});
}};
""".replace("VERSION", str(version)).replace("BROKEN", str(broken).lower()).replace("HEALTHY_WRITE", str(healthy_write).lower())}


def reserve_ports(previous=None):
    """Reserve only owned loopback ports, allowing their TIME_WAIT sockets."""
    sockets = []
    try:
        for port in previous or (0, 0):
            sock = socket.socket()
            sockets.append(sock)
            # Go listeners already use SO_REUSEADDR. Repeat it for the
            # reservation too; never SO_REUSEPORT, which could admit a live
            # listener belonging to another process.
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            sock.bind(("127.0.0.1", port))
        return sockets, [sock.getsockname()[1] for sock in sockets]
    except Exception:
        for sock in sockets:
            sock.close()
        raise


def mcp_payload(result):
    """Read a structured object or the tool formatter's complete JSON line."""
    structured = result.get('structuredContent')
    candidates = [structured] if isinstance(structured, dict) else []
    for item in reversed(result.get('content', [])):
        text = item.get('text', '').strip()
        # Error text ends with one complete JSON object after the human hint.
        # Never find an arbitrary brace inside prose or accept trailing junk.
        for raw in (text, text.rsplit('\n', 1)[-1]):
            try:
                value = json.loads(raw)
            except (ValueError, TypeError):
                continue
            if isinstance(value, dict):
                candidates.append(value)
    for value in candidates:
        if result.get('isError'):
            if not isinstance(value.get('category'), str) or not value['category']:
                continue
        return value
    raise AssertionError(f"no typed MCP object: {result}")


class Host:
    def __init__(self, binary, work, adapter=False, legacy=False, without_tailnet=False):
        self.binary, self.work, self.adapter = str(binary), Path(work), adapter
        self.legacy = legacy
        self.without_tailnet = without_tailnet
        self.addresses = None
        self.work.mkdir(parents=True, exist_ok=True)
        self.data = self.work / "data"
        self.proc = None
        self.serial = 0
        self.mcp_session = None
        self.phase = None
        self.phase_marker = None
        self.trace = []
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def start(self):
        # Reserve both ports until immediately before spawn. Bind-race failures
        # fail the run; never kill another process or reuse the operator's port.
        reservations, ports = reserve_ports(self.addresses)
        try:
            # A restart must repeat the configured listener addresses; changing
            # legacy flags against config.json correctly fails with EX_CONFIG.
            self.addresses = ports
            self.base = f"http://127.0.0.1:{ports[0]}"
            self.local = f"http://127.0.0.1:{ports[1]}"
            argv = [self.binary, "serve", "--data", str(self.data), "--listen", f"127.0.0.1:{ports[0]}",
                    "--network", "local", "--local-addr", f"127.0.0.1:{ports[1]}", "--portal=false"]
            if self.without_tailnet:
                require(self.adapter and not self.legacy, "missing-backend flag belongs only to the test adapter")
                argv.append("--without-tailnet")
        finally:
            for sock in reservations:
                sock.close()
        env = os.environ.copy()
        # Explicitly remove inherited provider credentials; not even their values
        # are inspected or recorded. Worker child keeps this disposable binary.
        for name in ("TS_AUTHKEY", "TS_AUTH_KEY", "FLATS_URL", "FLATS_CONFIG",
                     "FLATS_TAILSCALE_AUTHKEY", "FLATS_TAILSCALE_AUTHKEY_FILE",
                     "FLATS_ZROK_TOKEN", "FLATS_ZROK_TOKEN_FILE",
                     "FLATS_NETBIRD_SETUP_KEY", "FLATS_NETBIRD_SETUP_KEY_FILE", "HTTP_PROXY", "HTTPS_PROXY",
                     "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy", "FLATS_TEST_PHASE", "FLATS_TEST_PHASE_MARKER"):
            env.pop(name, None)
        if self.phase:
            env['FLATS_TEST_PHASE'] = self.phase
            env['FLATS_TEST_PHASE_MARKER'] = str(self.phase_marker)
        self.log = open(self.work / "serve.log", "ab")
        self.proc = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=self.log, stderr=self.log, env=env)
        self.trace.append({"argv": argv, "kind": "start"})
        until = time.monotonic() + 30
        while time.monotonic() < until:
            if self.proc.poll() is not None:
                raise AssertionError(f"server exited {self.proc.returncode}; inspect disposable serve.log")
            try:
                if self.request("GET", "/api/status")[0] == 200:
                    return
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(.1)
        raise AssertionError("server not ready in 30 seconds")

    def stop(self):
        if self.proc is not None:
            if self.proc.poll() is None:
                self.proc.terminate()
            try:
                self.proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=5)
            self.trace.append({"kind": "stop", "exit": self.proc.returncode})
            self.proc = None
            self.log.close()
            require(self.trace[-1]["exit"] in (0, -15), f"server did not exit gracefully: {self.trace[-1]}")
            require(not re.search(rb"panic:|fatal error:", (self.work / "serve.log").read_bytes()),
                    "server log contains a panic/fatal marker")

    def request(self, method, path, body=None, console=False, headers=None, local_host=None):
        base = self.local if local_host else self.base
        hdr = {"Content-Type": "application/json"}
        if console:
            hdr.update({"X-Flats-Console": "1", "Origin": self.base, "Sec-Fetch-Site": "same-origin"})
        else:
            hdr["X-Flats-Client"] = "lifecyclecheck"
        if local_host:
            hdr["Host"] = local_host + ".localhost:" + urllib.parse.urlparse(self.local).netloc.split(":")[-1]
        hdr.update(headers or {})
        if path == "/mcp" and self.mcp_session:
            hdr["Mcp-Session-Id"] = self.mcp_session
        raw = body if isinstance(body, bytes) else (None if body is None else json.dumps(body).encode())
        req = urllib.request.Request(base + path, data=raw, headers=hdr, method=method)
        try:
            response = self.opener.open(req, timeout=25)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            status, raw = response.code, response.read()
            if path == "/mcp" and response.headers.get("Mcp-Session-Id"):
                self.mcp_session = response.headers["Mcp-Session-Id"]
        try:
            value = json.loads(raw)
        except (ValueError, UnicodeDecodeError):
            value = raw.decode(errors="replace")
        self.trace.append({"method": method, "path": path, "console": console,
                           "host": local_host, "status": status, "response": value})
        return status, value

    def set_host_permissions(self, permitted):
        # The loopback adapter owns this current host-registry interface.
        # Production provider toggles could construct real clients and are
        # deliberately outside this gate's authority.
        require(self.adapter and not self.legacy, "host permission changes require the loopback adapter")
        self.ok('POST', '/__gate/host-permission', {'permitted': permitted})
        self.trace.append({'kind': 'disposable-host-permission', 'permitted': permitted})

    def set_flat_permission(self, slug, provider, permitted=True, allow_not_ready=False):
        code, out = self.request('POST', f'/console/api/flats/{slug}/providers',
                                 {'provider': provider, 'permitted': permitted}, console=True)
        require(code == 200 or (allow_not_ready and permitted and code == 409 and out.get('category') == 'provider_not_ready'),
                f"unexpected permission outcome: {code}: {out}")
        saved = provider in provider_ids(self.flat(slug))
        require(saved == permitted, f"permission was not saved despite readiness response: {provider}: {out}")
        return out

    def ok(self, method, path, body=None, status=200, **kwargs):
        code, value = self.request(method, path, body, **kwargs)
        require(code == status, f"{method} {path}: wanted {status}, got {code}: {value}")
        return value

    def save(self, slug, files, deploy=False):
        require(not deploy or self.legacy, "Draft saves never request version creation or deployment")
        route = "versions" if self.legacy else "draft"
        return self.ok("POST", f"/api/flats/{slug}/{route}" + ("?deploy=1" if deploy else ""),
                       archive(files), status=202 if deploy else 201,
                       headers={"Content-Type": "application/gzip"})

    def flat(self, slug):
        return self.ok("GET", f"/api/flats/{slug}")

    def versions(self, slug):
        return self.ok("GET", f"/api/flats/{slug}/versions")["versions"] or []

    def approval(self, response):
        if "approval" not in response and "deploy" in response:
            response = response["deploy"]
        require(response.get("status") == "pending_approval" and response.get("approval", {}).get("id"),
                f"expected pending approval: {response}")
        return response["approval"]["id"]

    def create_version(self, slug):
        draft = self.ok("GET", f"/api/flats/{slug}/draft")
        out = self.ok("POST", f"/api/flats/{slug}/versions",
                      {"revision": draft["revision"], "hash": draft["hash"]}, status=202)
        require(out.get("approval", {}).get("action") == "create_version", "wrong creation approval action")
        return self.approval(out)

    def deploy_version(self, slug, version):
        return self.approval(self.ok("POST", f"/api/flats/{slug}/deploy", {"version": version}, status=202))

    def decide(self, approval):
        code, out = self.request("POST", f"/console/api/approvals/{approval}/approve", {}, console=True)
        require(code in (200, 409, 422), f"unexpected approval response: {code}: {out}")
        return self.ok("GET", f"/api/approvals/{approval}")

    def activate(self, slug, files):
        self.save(slug, files)
        approval = self.create_version(slug)
        decided = self.decide(approval)
        require(decided["status"] == "approved", f"human approval did not save version: {decided}")
        version = execution(decided).get("version") or self.versions(slug)[0]["number"]
        dep_approval = self.deploy_version(slug, version)
        require(self.decide(dep_approval)["status"] == "approved", "human approval did not deploy version")
        return self.flat(slug)

    def traffic(self, slug, path="/"):
        return self.ok("GET", path, local_host=slug)

    def cli(self, *args):
        argv = [self.binary, "--url", self.base, "--json", *args]
        env = os.environ.copy()
        for name in ("TS_AUTHKEY", "TS_AUTH_KEY", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
                     "http_proxy", "https_proxy", "all_proxy"):
            env.pop(name, None)
        proc = subprocess.run(argv, capture_output=True, text=True, timeout=30, env=env)
        self.trace.append({"kind": "cli", "argv": argv, "exit": proc.returncode,
                           "stdout": proc.stdout, "stderr": proc.stderr})
        return proc

    def rpc(self, method, params):
        self.serial += 1
        result = self.ok("POST", "/mcp", {"jsonrpc": "2.0", "id": self.serial,
                        "method": method, "params": params}, headers={
                            "Accept": "application/json, text/event-stream", "MCP-Protocol-Version": "2025-03-26"})
        require("error" not in result, f"MCP protocol error: {result}")
        return result["result"]

    def initialize_mcp(self):
        self.rpc("initialize", {"protocolVersion": "2025-03-26", "capabilities": {},
                                "clientInfo": {"name": "lifecyclecheck", "version": "2"}})
        self.ok("POST", "/mcp", {"jsonrpc": "2.0", "method": "notifications/initialized"}, status=202,
                headers={"Accept": "application/json, text/event-stream", "MCP-Protocol-Version": "2025-03-26"})

    def mcp(self, name, **args):
        result = self.rpc("tools/call", {"name": name, "arguments": args})
        require(not result.get("isError"), f"MCP {name}: {result}")
        return mcp_payload(result)


def draft_saves(h):
    for revision, marker in enumerate(("DRAFT-A", "DRAFT-B", "DRAFT-C"), 1):
        out = h.save("draft", static(marker))
        require(out.get("draft", {}).get("revision") == revision and not out.get("version")
                and not out.get("approval") and not out.get("deploy"), f"save did not return only a Draft: {out}")
    f = h.flat("draft")
    require(f["publication"] == "unpublished" and f["live_version"] == 0, f)
    require(h.versions("draft") == [], "drafts appeared in published history")
    draft = h.ok("GET", "/api/flats/draft/draft")
    require(draft["revision"] == 3, draft)
    code, _ = h.request("POST", "/api/flats/draft/draft?expected_revision=1", archive(static("LOST")),
                        headers={"Content-Type": "application/gzip"})
    require(code == 409, f"stale writer not rejected: {code}")
    require(h.ok("GET", "/api/flats/draft/draft")["revision"] == 3, "conflict overwrote draft")


def invalid_upload(h):
    h.save("validate", static("VALID"))
    before = h.flat("validate")
    before_draft = h.ok("GET", "/api/flats/validate/draft")
    code, _ = h.request("POST", "/api/flats/validate/draft", archive({"../escape": "bad"}),
                        headers={"Content-Type": "application/gzip"})
    require(code == 422, f"unsafe archive not rejected: {code}")
    require(h.flat("validate") == before, "invalid upload changed flat")
    require(h.ok("GET", "/api/flats/validate/draft") == before_draft, "invalid upload changed draft")


def preview(h):
    h.save("preview", static("PRIVATE-DRAFT"))
    p = h.ok("POST", "/api/flats/preview/previews", {"target": "draft"}, status=201)
    require("PRIVATE-DRAFT" in h.traffic(p["host"]), "preview did not serve uploaded bytes")
    require(h.flat("preview")["live_version"] == 0 and h.versions("preview") == [], "preview published")
    require(not p.get("public_url"), "draft returned public route")


def publish_idempotency(h):
    h.save("publish", static("LIVE-V1"))
    a = h.create_version("publish")
    require(h.create_version("publish") == a, "retry created a second pending approval")
    require(h.flat("publish")["live_version"] == 0, "request activated before human approval")
    code, _ = h.request("POST", f"/api/approvals/{a}/approve", {})
    require(code in (403, 404, 405), "agent approval path exists")
    with concurrent.futures.ThreadPoolExecutor(2) as pool:
        outcomes = list(pool.map(lambda _: h.decide(a), range(2)))
    require(any(o["status"] == "approved" for o in outcomes), outcomes)
    require(h.decide(a)["status"] == "approved", "completed approval retry failed")
    f = h.flat("publish")
    # A saved version that was never deployed does not publish the flat.
    require(f["live_version"] == 0 and f["publication"] == "unpublished" and f["visibility"] == "private", f)
    require(f.get("latest_version") == 1 and not f.get("latest_deployed"), f)
    require([v["number"] for v in h.versions("publish")] == [1], "duplicate version created")
    dep = h.deploy_version("publish", 1)
    require(h.deploy_version("publish", 1) == dep, "deploy retry created a second pending approval")
    require(h.decide(dep)["status"] == "approved", "deploy approval failed")
    f = h.flat("publish")
    require(f["live_version"] == 1 and f["publication"] == "published" and f["visibility"] == "private", f)
    require([v["number"] for v in h.versions("publish")] == [1], "deploy created duplicate version")
    require(len(h.ok("GET", "/api/flats/publish/deployments")["deployments"]) == 1, "duplicate activation")
    require("LIVE-V1" in h.traffic("publish"), "approved bytes not serving")
    h.save("publish", static("UNPUBLISHED-EDIT"))
    require("LIVE-V1" in h.traffic("publish"), "draft replaced current traffic")
    require(h.flat("publish")["live_version"] == 1, "draft changed live pointer")
    h.stop()
    h.start()
    require("LIVE-V1" in h.traffic("publish"), "restart did not preserve published traffic")
    require(len(h.ok("GET", "/api/flats/publish/deployments")["deployments"]) == 1, "restart activated twice")


def drift(h):
    h.save("drift", static("FROZEN"))
    a = h.create_version("drift")
    h.save("drift", static("NEW-DRAFT"))
    failed(h.decide(a), "draft-drift")
    require(h.flat("drift")["live_version"] == 0 and h.versions("drift") == [], "stale approval activated")
    require(h.decide(h.create_version("drift"))["status"] == "approved", "fresh Draft approval poisoned after drift")
    require(h.decide(h.deploy_version("drift", 1))["status"] == "approved", "fresh deploy approval poisoned after drift")
    require("NEW-DRAFT" in h.traffic("drift"), "fresh Draft positive control did not serve")
    h.save("drift", static("POLICY-CANDIDATE"))
    a = h.create_version("drift")
    h.set_flat_permission("drift", "tailscale", allow_not_ready=True)
    failed(h.decide(a), "policy-drift")
    require(h.flat("drift")["live_version"] == 1, "policy drift activated")
    h.ok("POST", "/console/api/flats/drift/providers", {"provider": "tailscale", "permitted": False}, console=True)
    require(h.decide(h.create_version("drift"))["status"] == "approved", "fresh policy approval poisoned after drift")
    require(h.decide(h.deploy_version("drift", 2))["status"] == "approved", "fresh policy deploy approval poisoned after drift")
    require("POLICY-CANDIDATE" in h.traffic("drift"), "policy positive control did not serve")


def cli_pending(h):
    folder = h.work / "upload"
    folder.mkdir()
    for name, text in static("CLI").items():
        (folder / name).write_text(text)
    saved = h.cli("save-draft", "cli", str(folder))
    require(saved.returncode == 0, f"CLI Draft save failed: {saved.stdout} {saved.stderr}")
    require(h.flat("cli")["live_version"] == 0 and h.versions("cli") == [], "CLI Draft save allocated or activated")
    proc = h.cli("create-version", "cli")
    require(proc.returncode == 3, f"CLI creation exit should be 3, got {proc.returncode}: {proc.stdout} {proc.stderr}")
    require(h.flat("cli")["live_version"] == 0 and h.versions("cli") == [], "CLI activated")
    pub_id = h.approval(json.loads(proc.stdout))
    require(h.decide(pub_id)["status"] == "approved", "CLI version save not approved")
    require(h.flat("cli")["live_version"] == 0 and [v["number"] for v in h.versions("cli")] == [1], "CLI version save should allocate v1 without deploying")
    # Exercise the console deploy request too: console surface cannot bypass.
    a = h.approval(h.ok("POST", "/console/api/flats/cli/deploy", {"version": 1}, status=202, console=True))
    require(h.flat("cli")["live_version"] == 0, "console request bypassed approval")
    require(h.decide(a)["status"] == "approved", "CLI candidate not approved")
    require(h.flat("cli")["live_version"] == 1 and "CLI" in h.traffic("cli"), "approved CLI bytes not live")


def mcp_pending(h):
    h.initialize_mcp()
    tools = h.rpc("tools/list", {})["tools"]
    require(not any("approve" in t["name"] for t in tools), "MCP has approval capability")
    files = [{"path": name, "content": text, "encoding": "utf8"} for name, text in static("MCP").items()]
    out = h.mcp("save_draft", slug="mcp", files=files)
    require(not out.get("deploy") and not out.get("approval"), "Draft save requested an approval")
    draft = h.flat("mcp")["draft"]
    pending = h.approval(h.mcp("create_version", slug="mcp", revision=draft["revision"], hash=draft["hash"]))
    require(h.flat("mcp")["live_version"] == 0 and h.versions("mcp") == [], "MCP upload/deploy activated")
    require(h.approval(h.mcp("create_version", slug="mcp", revision=draft["revision"], hash=draft["hash"])) == pending, "MCP duplicate publish request did not dedupe")
    require(h.decide(pending)["status"] == "approved", "MCP publish candidate not approved")
    require(h.flat("mcp")["live_version"] == 0 and [v["number"] for v in h.versions("mcp")] == [1], "MCP publish activated before deploy")
    dep = h.approval(h.mcp("deploy", slug="mcp", version=1))
    require(h.approval(h.mcp("deploy", slug="mcp", version=1)) == dep, "MCP duplicate deploy request did not dedupe")
    require(h.flat("mcp")["live_version"] == 0, "MCP deploy activated")
    require(h.decide(dep)["status"] == "approved", "MCP deploy candidate not approved")
    require(h.flat("mcp")["live_version"] == 1 and "MCP" in h.traffic("mcp"), "approved MCP bytes not live")


def api_pending(h):
    out = h.save("upload", static("API-UPLOAD"))
    require(not out.get("deploy") and not out.get("approval"), "Draft save requested an approval")
    pub = h.create_version("upload")
    require(h.flat("upload")["live_version"] == 0 and h.versions("upload") == [], "API upload activated")
    require(h.decide(pub)["status"] == "approved", "API publish approval failed")
    require(h.flat("upload")["live_version"] == 0 and [v["number"] for v in h.versions("upload")] == [1], "API publish deployed directly")
    for console in (False, True):
        prefix = "/console/api" if console else "/api"
        h.approval(h.ok("POST", prefix + "/flats/upload/deploy", {"version": 1}, status=202, console=console))
        require(h.flat("upload")["live_version"] == 0, "deploy surface activated before approval")


def version_quota_deletion(h):
    h.activate('quota', server(1))
    require(h.traffic('quota', '/hit') == {'version': 1, 'hits': 1}, 'live DB witness was not created')
    h.activate('quota', server(2))
    require(h.traffic('quota') == {'version': 2, 'hits': 1}, 'deployment lost live DB witness')
    before = h.flat('quota')
    require(before['version_count'] == 2 and before['effective_version_quota'] == 20
            and before['version_quota'] is None, 'host version quota default is not 20')
    quota_path = '/console/api/flats/quota/version-quota'
    versions_before = h.versions('quota')
    snapshots_before = h.ok('GET', '/api/flats/quota/snapshots')
    require(snapshots_before.get('snapshots'), 'server deployments did not create snapshot evidence')
    snapshot_dir = h.data / 'flats/quota/snapshots'
    snapshot_bytes = {p.relative_to(snapshot_dir).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest() for p in snapshot_dir.rglob('*') if p.is_file()}
    require(snapshot_bytes, 'snapshot metadata has no on-disk witness')
    h.ok('PUT', quota_path, {'version_quota': 1}, console=True)
    require(h.versions('quota') == versions_before, 'lower quota removed retained versions')
    h.save('quota', server(3))
    draft = h.flat('quota')['draft']
    code, out = h.request('POST', '/api/flats/quota/versions',
                          {'revision': draft['revision'], 'hash': draft['hash']})
    require(code == 409 and 'quota' in str(out).lower(), 'full quota accepted creation request')
    h.ok('PUT', quota_path, {'version_quota': 3}, console=True)
    pending = h.create_version('quota')
    h.ok('PUT', quota_path, {'version_quota': 2}, console=True)
    decision = h.decide(pending)
    require(decision['status'] == 'failed' and 'quota' in decision.get('result', '').lower(),
            'approval did not recheck version quota')
    require(h.versions('quota') == versions_before, 'quota failure changed immutable history')
    preview = h.ok('POST', '/api/flats/quota/previews', {'version': 1}, status=201)
    removal = h.ok('DELETE', '/api/flats/quota/versions/1', status=202)
    require(removal.get('approval', {}).get('action') == 'delete_version', 'wrong deletion approval action')
    require(h.versions('quota') == versions_before and h.traffic(preview['host'])['version'] == 1,
            'agent deletion applied before operator approval')
    require(h.decide(h.approval(removal))['status'] == 'approved', 'version deletion approval failed')
    require([v['number'] for v in h.versions('quota')] == [2] and h.flat('quota')['version_count'] == 1,
            'deletion did not free quota or removed the wrong version')
    require(h.request('GET', '/', local_host=preview['host'])[0] == 404, 'deleted version preview remains open')
    require(not (h.data / 'flats/quota/versions/1').exists(), 'deleted version files remain')
    require(h.ok('GET', '/api/flats/quota/snapshots') == snapshots_before, 'version deletion removed data snapshots')
    require(h.traffic('quota') == {'version': 2, 'hits': 1}, 'version deletion changed live traffic or data')
    require({p.relative_to(snapshot_dir).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest() for p in snapshot_dir.rglob('*') if p.is_file()} == snapshot_bytes, 'version deletion changed snapshot files')
    require(h.decide(h.create_version('quota'))['status'] == 'approved', 'freed quota did not admit next version')
    require([v['number'] for v in h.versions('quota')] == [3, 2], 'version number reused after deletion')
    h.ok('DELETE', '/console/api/flats/quota/versions/3', console=True)
    code, _ = h.request('DELETE', '/console/api/flats/quota/versions/2', console=True)
    require(code == 409 and h.flat('quota')['live_version'] == 2, 'live version deletion was admitted')
    for number in (0, None):
        out = h.ok('PUT', quota_path, {'version_quota': number}, console=True)
        require(out['effective_version_quota'] == (0 if number == 0 else 20), 'quota override/default incorrect')
    h.stop()
    h.start()
    after = h.flat('quota')
    require(after['version_count'] == 1 and after['next_version'] == 4 and after['live_version'] == 2,
            'restart lost retained history or number highwater')
    require(h.ok('GET', '/api/flats/quota/snapshots') == snapshots_before, 'restart removed snapshots')


def approval_boundary(h):
    h.activate("boundary", static("BOUNDARY-EARLIER"))
    h.activate("boundary", static("BOUNDARY-CURRENT-V1"))
    h.save("boundary", static("BOUNDARY-CANDIDATE"))
    pending = h.create_version("boundary")
    for prefix in ("/api", "/console/api"):
        for action in ("approve", "reject", "decide", "resolve"):
            code, _ = h.request("POST", f"{prefix}/approvals/{pending}/{action}",
                                {"approved": True, "decision": "approve"})
            require(code in (403, 404, 405), f"agent decision route admitted: {prefix}/{action}: {code}")
            require(h.ok("GET", f"/api/approvals/{pending}")["status"] == "pending", "HTTP probe decided approval")
    for command in ("approve", "reject", "decide", "resolve"):
        result = h.cli(command, pending)
        require(result.returncode not in (0, 3), f"CLI decision command admitted: {command}")
        require(h.ok("GET", f"/api/approvals/{pending}")["status"] == "pending", "CLI decided approval")
    require(h.cli("approvals").returncode == 0, "read-only CLI approvals failed")
    h.initialize_mcp()
    tools = h.rpc("tools/list", {})["tools"]
    folder = h.work / "mcp-boundary-source"
    folder.mkdir()
    for name, value in static("BOUNDARY-CANDIDATE").items():
        (folder / name).write_text(value)
    files = [{"path": n, "content": v, "encoding": "utf8"} for n, v in static("BOUNDARY-CANDIDATE").items()]
    # Keep explicit valid inputs for every tool. New tools fail the census
    # until their operation and authority behavior have been considered.
    inputs = {
        "check_client": {}, "get_client_skill": {}, "list_flats": {},
        "get_flat": {"slug": "boundary"}, "get_draft": {"slug": "boundary"},
        "create_flat": {"slug": "boundary-created"},
        "save_draft": {"slug": "boundary", "files": files},
        "save_draft_from_dir": {"slug": "boundary", "dir": str(folder)},
        "list_versions": {"slug": "boundary"},
        "deploy": {"slug": "boundary", "version": 1},
        "create_version": {"slug": "boundary"},
        "delete_version": {"slug": "boundary", "version": 1},
        "rollback": {"slug": "boundary", "version": 1},
        "open_preview": {"slug": "boundary", "target": "draft", "version": 0},
        "set_visibility": {"slug": "boundary", "visibility": "private"},
        "delete_flat": {"slug": "boundary", "reason": "authority census"},
        "get_logs": {"slug": "boundary"}, "get_approval": {"id": pending},
        "list_secrets": {"slug": "boundary"}, "get_runtime_reference": {}, "get_content_types": {},
        "save_document": {"slug": "boundary-document", "markdown": "# Separate document"},
        "get_document": {"slug": "boundary"},
        "save_page": {"slug": "boundary-page", "html": "<title>Separate page</title>"},
        "save_design": {"slug": "boundary-design", "message": "authority census"},
        "get_design": {"slug": "boundary"}, "list_revisions": {"slug": "boundary"},
        "get_revision": {"slug": "boundary", "revision": 1},
        "pin_revision": {"slug": "boundary", "revision": 1, "pinned": True},
        "rollback_revision": {"slug": "boundary", "revision": 1},
        "update_document": {"slug": "boundary", "ops": [{"op": "insert", "at": "end", "text": "x"}]},
        "get_network": {"slug": "boundary"},
        "guide": {"items": ["topic.index", "refusal.conflict"]},
        "list_env": {"slug": "boundary"},
        "set_env": {"slug": "boundary", "name": "MODE", "value": "census"},
        "delete_env": {"slug": "boundary", "name": "MODE"},
    }
    refusals = {"get_document": "not_docs", "update_document": "not_docs", "get_design": "not_design",
                "list_revisions": "no_revisions", "get_revision": "no_revisions",
                "pin_revision": "no_revisions", "rollback_revision": "no_revisions"}
    census = []
    for tool in tools:
        name = tool['name']
        require(name in inputs, f"behavioral census lacks an operation for {name}")
        args = dict(inputs[name])
        if name == 'create_version':
            draft = h.flat('boundary')['draft']
            args.update(revision=draft['revision'], hash=draft['hash'])
        if name == 'delete_env':
            h.ok('PUT', '/console/api/flats/boundary/env/MODE', {'value': 'census'}, console=True)
        unknown = set(tool.get('inputSchema', {}).get('required', [])) - set(args)
        require(not unknown, f"behavioral census lacks valid inputs for {name}: {unknown}")
        result = h.rpc("tools/call", {"name": name, "arguments": args})
        payload = mcp_payload(result)
        if name in refusals:
            require(result.get('isError') and isinstance(payload, dict) and payload.get('category') == refusals[name],
                    f"MCP {name} must refuse the website with {refusals[name]}: {result}")
        else:
            require(not result.get('isError'), f"MCP census tool failed rather than exercised: {name}: {result}")
        if name in {'deploy', 'create_version', 'delete_version', 'rollback', 'delete_flat'}:
            requested = h.approval(payload)
            require(h.ok('GET', f'/api/approvals/{requested}')['status'] == 'pending',
                    f"MCP {name} did not request a real pending approval")
        if tool['name'] == 'create_flat':
            require(h.flat('boundary-created')['publication'] == 'unpublished' and h.versions('boundary-created') == [],
                    'MCP create did not create an unpublished flat')
        require(h.ok("GET", f"/api/approvals/{pending}")["status"] == "pending",
                f"MCP tool decided existing approval: {tool['name']}")
        current = h.flat("boundary")
        require(current["live_version"] == 2 and current["visibility"] == "private"
                and provider_ids(current) == {"local"} and len(h.versions("boundary")) == 2
                and "BOUNDARY-CURRENT-V1" in h.traffic("boundary"),
                f"MCP tool changed approved current/policy/history: {tool['name']}")
        census.append({"name": tool["name"], "is_error": result.get("isError", False)})
    # Console decisions are authorized by the console transport (same-origin
    # browser requests). A local agent that forges those headers can decide;
    # a separate operator credential is a known open design item.
    failed(h.decide(pending), 'draft-drift')
    require(h.decide(h.create_version('boundary'))['status'] == 'approved', 'fresh authorized operator did not publish')
    require(h.decide(h.deploy_version('boundary', 3))['status'] == 'approved', 'fresh authorized operator did not deploy')
    require('BOUNDARY-CANDIDATE' in h.traffic('boundary'), 'authorized boundary bytes not serving')
    h.save('boundary', static('EXPLICITLY-REJECTED-CANDIDATE'))
    rejection = h.create_version('boundary')
    h.ok('POST', f'/console/api/approvals/{rejection}/reject', {}, console=True)
    rejected = h.ok('GET', f'/api/approvals/{rejection}')
    require(rejected['status'] == 'rejected' and rejected.get('decided_by') and rejected.get('authorized_at'),
            'rejection did not persist actor/time')
    require(not rejected.get('result_data'), 'rejection synthesized an execution receipt')
    after_rejection = h.flat('boundary')
    require(after_rejection['live_version'] == 3 and after_rejection['visibility'] == 'private'
            and provider_ids(after_rejection) == {'local'}
            and 'BOUNDARY-CANDIDATE' in h.traffic('boundary') and len(h.versions('boundary')) == 3,
            'rejection changed current/policy or allocated a version')
    h.trace.append({"kind": "human-approval-known-risk", "mcp_census": census,
                    "boundary": "console transport only; no separate operator credential"})


def denied_visibility(h, slug):
    before = h.flat(slug)
    pending = h.approval(h.ok("POST", f"/api/flats/{slug}/visibility", {"visibility": "public"}, status=202))
    after = h.flat(slug)
    require(after["visibility"] == before["visibility"] and after["live_version"] == before["live_version"],
            "permission request changed current state before approval")
    failed(h.decide(pending), "not-permitted")


def actual_binary_authority(h):
    require(not h.adapter and not h.legacy, 'authority population must exercise the real product binary')
    h.activate('authority', static('ACTUAL-BINARY-AUTHORITY'))
    before = h.flat('authority')
    require(provider_ids(before) == {'local'}, 'actual host unexpectedly granted a nonlocal provider')
    code, _ = h.request('POST', '/api/flats/authority/providers', {'provider': 'portal', 'permitted': True})
    require(code == 403, f'agent HTTP provider grant was not forbidden: {code}')
    require(provider_ids(h.flat('authority')) == {'local'}, 'forbidden HTTP request changed provider policy')
    attempted = h.cli('set-provider-permission', 'authority', 'portal')
    require(attempted.returncode == 2 and 'unknown command "set-provider-permission"' in attempted.stderr,
            f'CLI exposed a provider-grant command: {attempted.returncode}: {attempted.stderr}')
    require(provider_ids(h.flat('authority')) == {'local'}, 'CLI probe changed provider policy')
    h.initialize_mcp()
    h.serial += 1
    refused = h.ok('POST', '/mcp', {'jsonrpc': '2.0', 'id': h.serial, 'method': 'tools/call',
                                  'params': {'name': 'set_provider_permission',
                                             'arguments': {'slug': 'authority', 'provider': 'portal', 'permitted': True}}},
                    headers={'Accept': 'application/json, text/event-stream', 'MCP-Protocol-Version': '2025-03-26'})
    require(refused.get('error') == {'code': -32602, 'message': 'unknown tool "set_provider_permission"'},
            f'MCP exposed a provider grant instead of its typed unknown-tool error: {refused}')
    require(provider_ids(h.flat('authority')) == {'local'}, 'MCP probe changed provider policy')
    denied_visibility(h, 'authority')
    after = h.flat('authority')
    require(provider_ids(after) == {'local'} and after['visibility'] == 'private' and after['live_version'] == 1,
            'denied Public request changed the actual binary current state')
    require('ACTUAL-BINARY-AUTHORITY' in h.traffic('authority'), 'authority probes changed live content')


def provider_failure(h):
    capabilities = h.ok('GET', '/__gate/capabilities')
    require(capabilities.get('tailnet_backend') is False, 'missing-backend population unexpectedly has a tailnet backend')
    h.activate("provider", static("PUBLISHED"))
    require(provider_ids(h.flat("provider")) <= {"local"}, "provider auto permitted")
    code, _ = h.request("POST", "/api/flats/provider/providers", {"provider": "portal", "permitted": True})
    require(code in (403, 404, 405), "agent can grant provider permission")
    denied_visibility(h, "provider")
    h.set_flat_permission("provider", "tailscale", allow_not_ready=True)
    require(h.flat("provider")["visibility"] == "private", "connection permission authorized Public")
    require("tailscale-funnel" not in provider_ids(h.flat("provider")), "Tailscale authorized Funnel")
    h.ok("POST", "/console/api/flats/provider/providers", {"provider": "tailscale-funnel", "permitted": True}, console=True)
    require(h.flat("provider")["visibility"] == "private", "Funnel permission authorized Public")
    h.ok("POST", "/console/api/flats/provider/providers", {"provider": "tailscale", "permitted": False}, console=True)
    h.set_host_permissions(['tailscale-funnel'])
    a = h.approval(h.ok("POST", "/api/flats/provider/visibility", {"visibility": "public"}, status=202))
    failed(h.decide(a), "unavailable")
    f = h.flat("provider")
    require(f["publication"] == "published" and f["live_version"] == 1 and f["visibility"] == "private", f)
    require("PUBLISHED" in h.traffic("provider"), "provider failure lost current version")
    h.ok("POST", "/console/api/flats/provider/providers", {"provider": "tailscale", "permitted": False}, console=True)
    h.ok("POST", "/console/api/flats/provider/providers", {"provider": "tailscale-funnel", "permitted": False}, console=True)
    h.activate("provider", static("PROVIDER-FAILURE-RECOVERED"))
    require("PROVIDER-FAILURE-RECOVERED" in h.traffic("provider"), "provider failure poisoned future publish")


def runtime_data(h):
    h.activate("counter", server(1))
    require(h.traffic("counter", "/hit")["hits"] == 1, "runtime did not persist hit")
    h.save("counter", server(2, broken=True))
    pub = h.create_version("counter")
    require(h.decide(pub)["status"] == "approved", "saving version 2 failed")
    require([v["number"] for v in h.versions("counter")] == [2, 1], "saved version 2 missing")
    a = h.deploy_version("counter", 2)
    require(h.traffic("counter")["hits"] == 1, "health ran before approval")
    failure = failed(h.decide(a), "health")
    data_impact(failure)
    require(h.traffic("counter") == {"version": 1, "hits": 1}, "failed health touched live data")
    require(h.traffic("counter", "/health-file") is None, "failed health FILES sentinel escaped copy")
    h.activate("counter", server(3, healthy_write=True))
    require(h.traffic("counter", "/health-file") is None, "healthy health FILES sentinel escaped copy")
    require(h.traffic("counter", "/hit") == {"version": 3, "hits": 2}, "second publish lost data")
    a = h.approval(h.ok("POST", "/api/flats/counter/rollback", {"version": 1}, status=202))
    require(h.traffic("counter")["version"] == 3, "rollback applied before approval")
    require(h.decide(a)["status"] == "approved", "rollback failed")
    require(h.traffic("counter") == {"version": 1, "hits": 2}, "code rollback restored data implicitly")
    require(sorted(v["number"] for v in h.versions("counter")) == [1, 2, 3], "rollback allocated number")
    # A restore request is a separately frozen approval, not implicit rollback.
    h.traffic("counter", "/hit")
    a = h.approval(h.ok("POST", "/api/flats/counter/rollback", {"version": 3, "restore_data": True}, status=202))
    require(h.traffic("counter") == {"version": 1, "hits": 3}, "restore mutated data before approval")
    row = h.ok("GET", f"/api/approvals/{a}")
    params = row["params"]
    if isinstance(params, str):
        params = json.loads(params)
    require(params.get("restore_data") is True, "restore consent not frozen separately")
    h.ok("POST", f"/console/api/approvals/{a}/reject", {}, console=True)
    require(h.traffic("counter")["hits"] == 3, "rejected restore affected data")


def resume_claimed(h):
    # No direct store UPDATE: approval state must be reached by the real
    # decision path. Named phase hooks require the separately granted core
    # contract and exist only in the test executable, never product HTTP.
    phases = ('before_health', 'after_health', 'after_allocation_before_live', 'after_live_before_finalize')
    for i, phase in enumerate(phases):
        slug = "resume-" + str(i)
        # Set up with no crash phase armed: saving the version is itself an
        # approval, and after_live_before_finalize fires after every applied
        # approval, so arming it first would crash the setup instead of the
        # deployment under test.
        h.stop()
        h.phase, h.phase_marker = None, None
        h.start()
        h.save(slug, static("RESUME-" + phase))
        if phase != 'after_allocation_before_live':
            pub = h.create_version(slug)
            require(h.decide(pub)["status"] == "approved", "setup version save failed")
        h.stop()
        h.phase, h.phase_marker = phase, h.work / (phase + '.json')
        h.start()
        a = h.create_version(slug) if phase == 'after_allocation_before_live' else h.deploy_version(slug, 1)
        def approve_at_phase():
            try:
                h.request("POST", f"/console/api/approvals/{a}/approve", {}, console=True)
            except (OSError, urllib.error.URLError):
                pass
        thread = threading.Thread(target=approve_at_phase, daemon=True)
        thread.start()
        until = time.monotonic() + 15
        while time.monotonic() < until and not h.phase_marker.exists() and h.proc.poll() is None:
            time.sleep(.02)
        require(h.phase_marker.exists(), f"tagged core crash phase was not observed: {phase}")
        marker = json.loads(h.phase_marker.read_text())
        require(marker.get('phase') == phase, f"wrong observed crash phase: {marker}")
        h.proc.kill()
        require(h.proc.wait(timeout=10) == -9, "controlled SIGKILL exit not observed")
        thread.join(timeout=10)
        require(not thread.is_alive(), "approval request survived killed disposable process")
        h.trace.append({"kind": "intentional-phase-crash", "phase": phase, "exit": -9, "marker": marker})
        h.log.close()
        h.proc = None
        for _ in range(2):
            h.start()
            require(h.ok("GET", f"/api/approvals/{a}")["status"] == "approved", "claimed approval not resumed")
            require([v["number"] for v in h.versions(slug)] == [1], "crash resume allocated more than once")
            if phase != 'after_allocation_before_live':
                require(h.flat(slug)["live_version"] == 1 and "RESUME-" + phase in h.traffic(slug), "resumed bytes/pointer mismatch")
                require(len(h.ok("GET", f"/api/flats/{slug}/deployments")["deployments"]) == 1, "crash resume activation count")
            else:
                require(h.flat(slug)["live_version"] == 0, "publish crash resume deployed without deploy approval")
            h.stop()
        h.start()
        if phase == 'after_allocation_before_live':
            dep = h.deploy_version(slug, 1)
            require(h.decide(dep)["status"] == "approved", "post-resume deploy failed")
            require(h.flat(slug)["live_version"] == 1 and "RESUME-" + phase in h.traffic(slug), "post-resume deployed bytes mismatch")
    h.phase = None
    h.phase_marker = None


def production_hooks_absent(h):
    h.stop()
    h.phase = 'before_health'
    h.phase_marker = h.work / 'ordinary-build-must-not-pause.json'
    h.start()
    h.activate('ordinary', static('ORDINARY-BUILD-NO-FAULT-HOOK'))
    require(not h.phase_marker.exists(), 'ordinary production build read fault environment or paused')
    require('ORDINARY-BUILD-NO-FAULT-HOOK' in h.traffic('ordinary'), 'ordinary build did not publish')
    h.phase, h.phase_marker = None, None


def migration(h):
    require(h.legacy_binary, "migration needs --legacy-binary from pre-lifecycle base")
    h.stop()
    legacy = Host(h.legacy_binary, h.work / "legacy", legacy=True)
    try:
        legacy.start()
        for marker in ("LEGACY-A", "LEGACY-B"):
            legacy.save("historical", static(marker))
        require([v["number"] for v in legacy.versions("historical")] == [2, 1], "legacy seed not historical v1/v2")
        require(legacy.flat("historical")["live_version"] == 0, "legacy seed was deployed")
        before = {p.relative_to(legacy.data).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest()
                  for p in legacy.data.rglob("index.html")}
        require(len(before) == 2, "legacy saved files missing")
    finally:
        legacy.stop()
        h.trace.extend(legacy.trace)
    h.data = legacy.data
    h.addresses = legacy.addresses
    h.start()
    f = h.flat("historical")
    require(f["publication"] == "unpublished" and f["live_version"] == 0, "migration fabricated publication")
    require(h.versions("historical") == [], "never-deployed snapshots stayed published")
    after = sorted(hashlib.sha256(p.read_bytes()).hexdigest() for p in h.data.rglob("index.html"))
    require(set(after) == set(before.values()), "migration lost or changed saved file content")
    for rel, digest in before.items():
        path = h.data / rel
        require(path.is_file() and hashlib.sha256(path.read_bytes()).hexdigest() == digest,
                "migration moved or changed historical bytes: " + rel)
    p = h.ok("POST", "/api/flats/historical/previews", {"target": "draft"}, status=201)
    require("LEGACY-B" in h.traffic(p["host"]), "migration selected wrong current draft")
    h.stop()
    h.start()
    require(sorted(hashlib.sha256(p.read_bytes()).hexdigest() for p in h.data.rglob("index.html")) == after,
            "repeated startup damaged migration files")
    a = h.create_version("historical")
    require(h.decide(a)["status"] == "approved", "migrated draft not publishable")
    require(h.decide(h.deploy_version("historical", 1))["status"] == "approved", "migrated v1 not deployable")
    require(h.flat("historical")["live_version"] == 1, "historical never-deployed number not freed")
    require("LEGACY-B" in h.traffic("historical"), "migrated publish served wrong bytes")


def visibility_transitions(h):
    h.ok("POST", "/__gate/host-permission", {"permitted": ["portal"]})
    h.activate("public", static("PUBLIC-V1"))
    h.ok("POST", "/console/api/flats/public/providers", {"provider": "portal", "permitted": True}, console=True)
    h.initialize_mcp()
    for surface in ("api", "console", "cli", "mcp"):
        for target, previous in (("public", "private"), ("private", "public")):
            if surface in ("api", "console"):
                prefix = "/console/api" if surface == "console" else "/api"
                a = h.approval(h.ok("POST", prefix + "/flats/public/visibility", {"visibility": target},
                                    status=202, console=surface == "console"))
            elif surface == "cli":
                out = h.cli("visibility", "public", target)
                require(out.returncode == 3, f"CLI visibility {target}: exit {out.returncode}")
                a = h.approval(json.loads(out.stdout))
            else:
                a = h.approval(h.mcp("set_visibility", slug="public", visibility=target))
            require(h.flat("public")["visibility"] == previous, f"{surface} {target} applied before approval")
            require(h.decide(a)["status"] == "approved", f"{surface} visibility decision failed")
            f = h.flat("public")
            require(f["visibility"] == target and f["live_version"] == 1, "visibility changed version")
            code, body = h.request("GET", "/", local_host="pub-public")
            require((code == 200 and "PUBLIC-V1" in body) if target == "public" else code == 404,
                    f"simulated public route disagrees with {target}: {code} {body}")
            require("PUBLIC-V1" in h.traffic("public"), "visibility stopped private route")
    # Published public traffic remains immutable while a separate private draft is saved.
    a = h.approval(h.ok("POST", "/api/flats/public/visibility", {"visibility": "public"}, status=202))
    require(h.decide(a)["status"] == "approved", "could not setup teardown failure")
    h.save("public", static("PRIVATE-NEW-DRAFT"))
    require("PUBLIC-V1" in h.traffic("pub-public"), "public traffic served draft")
    p = h.ok("POST", "/api/flats/public/previews", {"target": "draft"}, status=201)
    require("PRIVATE-NEW-DRAFT" in h.traffic(p["host"]), "private draft preview wrong")
    require(h.request("GET", "/", local_host="pub-" + p["host"])[0] == 404, "draft publicly registered")
    # Fault keeps the actual loopback simulated public route answering.
    h.ok("POST", "/__gate/fault", {"stop": True})
    a = h.approval(h.ok("POST", "/api/flats/public/visibility", {"visibility": "private"}, status=202))
    failed(h.decide(a), "teardown")
    f = h.flat("public")
    require(f["visibility"] == "public" and f["publication"] == "published", "teardown failure reported private")
    require("PUBLIC-V1" in h.traffic("pub-public"), "fault did not preserve observable public route")
    require("PUBLIC-V1" in h.traffic("public"), "teardown failure lost private path")
    h.ok("POST", "/__gate/fault", {"stop": False})
    a = h.approval(h.ok("POST", "/api/flats/public/visibility", {"visibility": "private"}, status=202))
    require(h.decide(a)["status"] == "approved", "retry teardown not approved")
    require(h.request("GET", "/", local_host="pub-public")[0] == 404, "approved teardown still answering")
    h.ok("POST", "/__gate/fault", {"serve": True})
    a = h.approval(h.ok("POST", "/api/flats/public/visibility", {"visibility": "public"}, status=202))
    failed(h.decide(a), "serve")
    f = h.flat("public")
    require(f["publication"] == "published" and f["visibility"] == "private", "network fault changed publication")
    require(h.request("GET", "/", local_host="pub-public")[0] == 404, "network fault opened route")
    h.ok("POST", "/__gate/fault", {"serve": False})
    a = h.approval(h.ok("POST", "/api/flats/public/visibility", {"visibility": "public"}, status=202))
    require(h.decide(a)["status"] == "approved", "fresh public request poisoned after provider fault")
    require("PUBLIC-V1" in h.traffic("pub-public"), "provider positive control did not serve")


def partial_provider_failures(h):
    h.ok("POST", "/__gate/host-permission", {"permitted": ["portal", "tailscale-funnel"]})
    h.activate("siblings", static("SIBLING-CURRENT"))
    for provider in ("portal", "tailscale-funnel"):
        h.ok("POST", "/console/api/flats/siblings/providers", {"provider": provider, "permitted": True}, console=True)
    def change(target):
        return h.approval(h.ok("POST", "/api/flats/siblings/visibility", {"visibility": target}, status=202))
    require(h.decide(change("public"))["status"] == "approved", "both-provider setup failed")
    for host in ("siblings", "pub-siblings", "funnel-siblings"):
        require("SIBLING-CURRENT" in h.traffic(host), "configured sibling route missing")
    h.ok("POST", "/__gate/fault", {"funnel_stop": True})
    failed(h.decide(change("private")), "teardown")
    require(h.flat("siblings")["visibility"] == "public", "partial teardown falsely saved Private")
    require("SIBLING-CURRENT" in h.traffic("funnel-siblings") and "SIBLING-CURRENT" in h.traffic("siblings"),
            "unconfirmed public route or independent private path lost")
    require(h.request("GET", "/", local_host="pub-siblings")[0] == 404, "confirmed sibling stop stayed open")
    h.ok("POST", "/__gate/fault", {})
    require(h.decide(change("private"))["status"] == "approved", "partial stop retry failed")
    h.ok("POST", "/__gate/fault", {"portal_serve": True})
    failed(h.decide(change("public")), "serve")
    require(h.flat("siblings")["visibility"] == "private", "partial serve falsely reported Public")
    for host in ("pub-siblings", "funnel-siblings"):
        require(h.request("GET", "/", local_host=host)[0] == 404, "successful sibling leaked after failed preparation")
    require("SIBLING-CURRENT" in h.traffic("siblings"), "partial opening failure lost private traffic")
    h.ok("POST", "/__gate/fault", {})
    require(h.decide(change("public"))["status"] == "approved", "fresh both-provider approval poisoned")
    for host in ("pub-siblings", "funnel-siblings"):
        require("SIBLING-CURRENT" in h.traffic(host), "both-provider positive control missing")


def public_publish_rollback(h):
    h.ok("POST", "/__gate/host-permission", {"permitted": ["portal"]})
    h.activate("binding", static("BINDING-V1"))
    h.ok("POST", "/console/api/flats/binding/providers", {"provider": "portal", "permitted": True}, console=True)
    a = h.approval(h.ok("POST", "/api/flats/binding/visibility", {"visibility": "public"}, status=202))
    require(h.decide(a)["status"] == "approved", "public binding setup failed")
    def both(marker):
        for host in ("binding", "pub-binding"):
            require(marker in h.traffic(host), f"{host} did not serve approved {marker}")
    both("BINDING-V1")
    h.save("binding", static("BINDING-V2"))
    a = h.create_version("binding")
    both("BINDING-V1")
    require(h.decide(a)["status"] == "approved", "public v2 publish failed")
    both("BINDING-V1")
    dep = h.deploy_version("binding", 2)
    both("BINDING-V1")
    require(h.decide(dep)["status"] == "approved", "public v2 deploy failed")
    both("BINDING-V2")
    h.save("binding", static("UNAPPROVED-BINDING-DRAFT"))
    both("BINDING-V2")
    a = h.approval(h.ok("POST", "/api/flats/binding/rollback", {"version": 1}, status=202))
    both("BINDING-V2")
    require(h.decide(a)["status"] == "approved", "public rollback failed")
    both("BINDING-V1")
    require(h.flat("binding")["visibility"] == "public", "public rollback changed visibility")
    preview = h.ok("POST", "/api/flats/binding/previews", {"target": "draft"}, status=201)
    require("UNAPPROVED-BINDING-DRAFT" in h.traffic(preview["host"]), "rollback consumed draft")
    h.stop()
    h.start()
    both("BINDING-V1")


def provider_matrix(h):
    h.activate("matrix", static("MANAGER-MATRIX"))
    # Both injected backends exist throughout; permission causes cannot be
    # accidentally satisfied by the unavailable-backend failure used elsewhere.
    h.ok("POST", "/__gate/host-permission", {"permitted": ["tailscale", "tailscale-funnel", "portal"]})
    def public_request():
        return h.approval(h.ok("POST", "/api/flats/matrix/visibility", {"visibility": "public"}, status=202))
    def allow(provider, permitted=True):
        h.set_flat_permission("matrix", provider, permitted)
    denied_visibility(h, "matrix")
    allow("tailscale")
    denied_visibility(h, "matrix")
    require(h.ok("GET", "/__gate/state")["calls"].get("funnel_serve", 0) == 0, "Tailscale permission called Funnel")
    allow("tailscale", False)
    allow("tailscale-funnel")
    h.ok("POST", "/__gate/host-permission", {"permitted": ["portal"]})
    failed(h.decide(public_request()), "not-permitted")
    state = h.ok("GET", "/__gate/state")
    require(state["calls"].get("funnel_serve", 0) == state["calls"].get("portal_serve", 0) == 0,
            "host denial called backend or fell back to Portal")
    h.ok("POST", "/__gate/host-permission", {"permitted": ["tailscale-funnel", "portal"]})
    require(h.decide(public_request())["status"] == "approved", "both permissions did not enable Funnel")
    state = h.ok("GET", "/__gate/state")
    require(state["calls"].get("funnel_serve") == 1 and state["calls"].get("portal_serve", 0) == 0,
            "production Manager did not call exactly the explicitly permitted backend")
    require("MANAGER-MATRIX" in h.traffic("funnel-matrix"), "Funnel loopback double not serving")
    require("MANAGER-MATRIX" in h.traffic("matrix"), "Manager lost Private route")


def async_ready_public(h):
    h.ok('POST', '/__gate/host-permission', {'permitted': ['portal', 'tailscale-funnel']})
    for provider, host_prefix in [('portal', 'pub-'), ('tailscale-funnel', 'funnel-')]:
        slug = 'async-' + provider
        h.activate(slug, static('ASYNC-CURRENT-' + provider))
        h.ok('POST', f'/console/api/flats/{slug}/providers', {'provider': provider, 'permitted': True}, console=True)
        h.ok('POST', '/__gate/fault', {'async': True})
        before = h.ok('GET', '/__gate/state')['calls'].copy()
        pending = h.approval(h.ok('POST', f'/api/flats/{slug}/visibility', {'visibility': 'public'}, status=202))
        require(h.flat(slug)['visibility'] == 'private' and h.request('GET', '/', local_host=host_prefix + slug)[0] == 404,
                'Public opened before approval')
        require(h.decide(pending)['status'] == 'approved', 'async provider cannot commit approved Public')
        connecting = h.flat(slug)
        endpoint = next(ep for ep in connecting['endpoints'] if ep['provider'] == provider and ep['audience'] == 'current')
        require(connecting['visibility'] == 'public' and connecting['connection_state'] == 'starting'
                and endpoint['state'] == 'starting' and endpoint['ready'] is False, 'connecting status claims readiness')
        opened = h.ok('GET', '/__gate/state')['calls'].copy()
        for _ in range(3):
            require(h.decide(pending)['status'] == 'approved', 'repeat approved decision failed')
            same = h.ok('POST', f'/api/flats/{slug}/visibility', {'visibility': 'public'})
            require(same['status'] == 'done', 'unchanged policy spawned retry')
        require(h.ok('GET', '/__gate/state')['calls'] == opened, 'retries reopened/stopped asynchronously connecting route')
        counter = 'portal_serve' if provider == 'portal' else 'funnel_serve'
        stop_counter = 'portal_stop' if provider == 'portal' else 'funnel_stop'
        require(opened.get(counter, 0) == before.get(counter, 0) + 1 and opened.get(stop_counter, 0) == before.get(stop_counter, 0),
                'Public preparation looped open/stop')
        h.ok('POST', '/__gate/fault', {'async': True, 'ready': True})
        ready = h.flat(slug)
        require(ready['connection_state'] == 'ready' and ready['public_url'], 'asynchronous readiness did not converge')
        require('ASYNC-CURRENT-' + provider in h.traffic(host_prefix + slug), 'ready route did not serve current bytes')
        h.ok('POST', '/__gate/fault', {})


def private_tailscale(h):
    h.ok('POST', '/__gate/host-permission', {'permitted': ['tailscale']})
    h.activate('tail-private', static('LOCAL-ONLY'))
    before = h.flat('tail-private')
    require('tailnet-' not in before['private_url'] and before['private_state'] == 'ready', 'unserved Tailscale advertised')
    h.ok('POST', '/console/api/flats/tail-private/providers', {'provider': 'tailscale', 'permitted': True}, console=True)
    h.activate('tail-private', static('TAILNET-CURRENT'))
    current = h.flat('tail-private')
    endpoint = next(ep for ep in current['endpoints'] if ep['provider'] == 'tailscale' and ep['audience'] == 'current')
    require(current['private_url'] == endpoint['url'] and current['private_state'] == 'ready' and endpoint['ready']
            and endpoint['url'] != before['private_url'], 'Private Tailscale selection/DTO incorrect')
    parsed = urllib.parse.urlparse(endpoint['url'])
    req = urllib.request.Request(f'http://127.0.0.1:{parsed.port}/', headers={'Host': parsed.netloc})
    with h.opener.open(req, timeout=10) as response:
        require(response.code == 200 and 'TAILNET-CURRENT' in response.read().decode(), 'distinct Tailscale listener not serving')
    require('TAILNET-CURRENT' in h.traffic('tail-private'), 'independent Local route lost')
    require(h.request('GET', '/', local_host='tailnet-tail-private')[0] == 404, 'Tailscale double shared Local listener')
    preview = h.ok('POST', '/api/flats/tail-private/previews', {'target': 'draft'}, status=201)
    draft_endpoint = next(ep for ep in h.flat('tail-private')['endpoints'] if ep['provider'] == 'tailscale' and ep['audience'] == 'draft' and ep['host'] == preview['host'])
    require('tailnet-' in draft_endpoint['url'] and draft_endpoint['ready'], 'Draft Private Tailscale route missing')
    state = h.ok('GET', '/__gate/state')['calls']
    require(state.get('tailscale_serve', 0) > 0 and state.get('funnel_serve', 0) == state.get('portal_serve', 0) == 0,
            'Private Tailscale granted or opened Public provider')
    h.ok('POST', '/__gate/fault', {'tailscale_stop': True})
    code, body = h.request('POST', '/console/api/flats/tail-private/providers', {'provider': 'tailscale', 'permitted': False}, console=True)
    require(code == 409 and body['category'] == 'provider_in_use' and 'tailscale' in provider_ids(h.flat('tail-private')),
            'unconfirmed Private stop did not preserve permission')
    with h.opener.open(req, timeout=10) as response:
        require(response.code == 200, 'failed stop unexpectedly removed route')
    h.ok('POST', '/__gate/fault', {})
    h.ok('POST', '/console/api/flats/tail-private/providers', {'provider': 'tailscale', 'permitted': False}, console=True)
    require('tailscale' not in provider_ids(h.flat('tail-private')), 'confirmed stop did not revoke permission')
    for url in [endpoint['url'], draft_endpoint['url']]:
        parsed = urllib.parse.urlparse(url)
        request = urllib.request.Request(f'http://127.0.0.1:{parsed.port}/', headers={'Host': parsed.netloc})
        try:
            h.opener.open(request, timeout=10)
            raise AssertionError('revoked Private Tailscale route remains reachable')
        except urllib.error.HTTPError as error:
            require(error.code == 404, 'revoked route did not close')
    require('TAILNET-CURRENT' in h.traffic('tail-private'), 'revocation stopped Local current')
    require(h.request('GET', '/', local_host=preview['host'])[0] == 200, 'revocation stopped Local preview')


def provider_revocation_teardown(h):
    h.ok('POST', '/__gate/host-permission', {'permitted': ['portal', 'tailscale-funnel']})
    for provider, prefix in [('portal', 'pub-'), ('tailscale-funnel', 'funnel-')]:
        slug = 'cleanup-' + provider
        h.activate(slug, static('CLEANUP-CURRENT'))
        h.ok('POST', f'/console/api/flats/{slug}/providers', {'provider': provider, 'permitted': True}, console=True)
        def visibility(name, value):
            pending = h.approval(h.ok('POST', f'/api/flats/{name}/visibility', {'visibility': value}, status=202))
            require(h.decide(pending)['status'] == 'approved', 'cleanup visibility approval failed')
        visibility(slug, 'public')
        code, body = h.request('POST', f'/console/api/flats/{slug}/providers', {'provider': provider, 'permitted': False}, console=True)
        unchanged = h.flat(slug)
        require(code == 409 and body['category'] == 'provider_in_use' and provider in provider_ids(unchanged)
                and unchanged['visibility'] == 'public' and 'CLEANUP-CURRENT' in h.traffic(prefix + slug),
                'revocation removed permission under a live route or reported false policy')
        require(any(ep['provider'] == provider and ep['permitted'] and ep['ready'] for ep in unchanged['endpoints']),
                'refused revocation misreported permission/readiness')
        visibility(slug, 'private')
        h.ok('POST', f'/console/api/flats/{slug}/providers', {'provider': provider, 'permitted': False}, console=True)
        require(provider not in provider_ids(h.flat(slug)) and h.request('GET', '/', local_host=prefix + slug)[0] == 404,
                'successful idle revocation left route serving')
        h.ok('POST', f'/console/api/flats/{slug}/providers', {'provider': provider, 'permitted': True}, console=True)
        visibility(slug, 'public')
        moved = slug + '-moved'
        h.ok('POST', f'/api/flats/{slug}/rename', {'slug': moved})
        require('CLEANUP-CURRENT' in h.traffic(prefix + moved), 'renamed public route missing')
        # The test-only clock advances expiry; actual core Sweep owns teardown.
        h.ok('POST', '/__gate/expire-redirects', {})
        require(h.request('GET', '/', local_host=prefix + slug)[0] == 404 and h.request('GET', '/', local_host=slug)[0] == 404,
                'expired rename redirects stayed registered')
        pending = h.approval(h.ok('DELETE', f'/api/flats/{moved}', status=202))
        require(h.decide(pending)['status'] == 'approved', 'public delete approval failed')
        require(h.request('GET', '/', local_host=prefix + moved)[0] == 404 and h.request('GET', '/', local_host=moved)[0] == 404,
                'Manager-owned Public or Private route survived delete')
        h.trace.append({'kind': 'provider-revocation-and-teardown', 'provider': provider, 'refusal_category': body['category']})


from populations import (mixed_migration, seed_mixed, restore_success, runtime_initialization,
                         probe_runtime, current_live_drift, visibility_drift)


LOCAL_CASES = [("draft-save-conflict", draft_saves), ("archive-validation", invalid_upload),
               ("private-draft-traffic", preview), ("publish-human-idempotency-restart", publish_idempotency),
               ("frozen-revision-policy", drift), ("cli-console-pending", cli_pending),
               ("mcp-pending", mcp_pending), ("api-upload-deploy-pending", api_pending),
               ("approval-boundary-census", approval_boundary),
               ("version-quota-explicit-deletion", version_quota_deletion),
               ("production-fault-hooks-absent", production_hooks_absent),
               ("actual-binary-provider-authority", actual_binary_authority),
               ("runtime-health-rollback-data", runtime_data),
               ("historical-migration", migration),
               ("historical-mixed-migration", mixed_migration),
               ("runtime-initialization-data", runtime_initialization),
               ("rollback-approved-restore-data", restore_success),
               ("frozen-current-live", current_live_drift)]
ADAPTER_CASES = [("provider-permission-failure", provider_failure),
                 ("visibility-all-surfaces-teardown", visibility_transitions),
                 ("public-publish-rollback-route-binding", public_publish_rollback),
                 ("partial-provider-failures", partial_provider_failures),
                 ("frozen-visibility", visibility_drift),
                 ("provider-host-flat-permission-matrix", provider_matrix),
                 ("async-public-readiness", async_ready_public),
                 ("private-tailscale-distinct-provider", private_tailscale),
                 ("provider-revocation-rename-delete-expiry", provider_revocation_teardown),
                 ("restart-claimed-approval", resume_claimed)]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True)
    parser.add_argument("--work", required=True)
    parser.add_argument("--prepare-only", action="store_true",
                        help="seed/probe legacy binary only; never acceptance")
    parser.add_argument("--only", help="comma separated case IDs")
    parser.add_argument("--legacy-binary")
    parser.add_argument("--adapter-binary")
    parser.add_argument("--legacy-adapter-binary")
    parser.add_argument("--crash-binary")
    args = parser.parse_args()
    work = Path(args.work)
    work.mkdir(parents=True, exist_ok=True)
    source = source_identity()
    binaries = {"legacy": binary_identity(args.legacy_binary),
                "legacy_provider_fixture": binary_identity(args.legacy_adapter_binary)}
    if not args.prepare_only:
        binaries.update(candidate=binary_identity(args.binary), provider_adapter=binary_identity(args.adapter_binary))
        binaries['tagged_crash_adapter'] = binary_identity(args.crash_binary)
    report = {"source": source, "source_head": source["head"], "binaries": binaries,
              "cases": [], "scope": "actual binary, disposable Local only; no external provider traffic",
              "required_lanes": sorted(REQUIRED_LANES), "human_approval": "OPEN: forged headers are not human proof"}
    cases = ([("prepare-mixed-history", seed_mixed), ("prepare-runtime-capability", probe_runtime)]
             if args.prepare_only else LOCAL_CASES + ADAPTER_CASES)
    report["full_suite_selected"] = not args.prepare_only and not args.only
    report["mode"] = "legacy-preparation" if args.prepare_only else "acceptance"
    selected = set(args.only.split(",")) if args.only else {n for n, _ in cases}
    require(selected <= {n for n, _ in cases}, f"unknown case IDs: {selected}")
    for name, test in cases:
        if name not in selected:
            continue
        adapter = (name, test) in ADAPTER_CASES
        binary = args.crash_binary if name == 'restart-claimed-approval' else (args.adapter_binary if adapter else args.binary)
        host = Host(binary or args.binary, work / name, adapter=adapter, without_tailnet=name == "provider-permission-failure")
        host.legacy_binary = args.legacy_binary
        host.legacy_adapter_binary = args.legacy_adapter_binary
        if args.prepare_only:
            require(args.legacy_binary, "preparation needs --legacy-binary")
            host.binary = args.legacy_binary
            host.legacy = True
        result = {"id": name, "lane": "unverified-provider-adapter" if adapter else "actual-binary-local"}
        try:
            require(not adapter or args.adapter_binary, "missing --adapter-binary; provider lane cannot pass")
            require(name != 'restart-claimed-approval' or args.crash_binary, 'missing separately tagged --crash-binary')
            host.start()
            if adapter:
                capabilities = host.ok("GET", "/__gate/capabilities")
                require(capabilities.get("production_provider_manager") is True and
                        capabilities.get("legacy_public_fallback") is False,
                        f"production manager required; legacy fallback cannot pass: {capabilities}")
                result["lane"] = "production-provider-manager"
            test(host)
            result["outcome"] = "pass"
        except Exception as error:
            result.update(outcome="fail", error=str(error), traceback=traceback.format_exc())
        finally:
            try:
                host.stop()
            except Exception as error:
                result.update(outcome="fail", shutdown_error=str(error))
            result["trace"] = host.trace
        report["cases"].append(result)
        print(f"{result['outcome'].upper()}: {name}" + (": " + result["error"] if "error" in result else ""), flush=True)
    report["exit"] = int(any(c["outcome"] != "pass" for c in report["cases"]))
    report["passing_lanes"] = sorted({c["lane"] for c in report["cases"] if c["outcome"] == "pass"})
    report["lane_complete"] = REQUIRED_LANES <= set(report["passing_lanes"])
    report["identity_bound"] = acceptance_identity(source, binaries) if not args.prepare_only else not source["dirty"]
    boundary = next((c for c in report['cases'] if c['id'] == 'approval-boundary-census'), None)
    if boundary and boundary['outcome'] == 'pass' and any(t.get('human_approval_proven') is True for t in boundary['trace']):
        report['human_approval'] = 'proven'
    report["acceptance"] = accepted(report)
    if report["full_suite_selected"] and not report["acceptance"]:
        report["exit"] = 1
    (work / "evidence.json").write_text(json.dumps(report, indent=2) + "\n")
    print(f"Evidence: {work / 'evidence.json'}", flush=True)
    return report["exit"]


if __name__ == "__main__":
    raise SystemExit(main())
