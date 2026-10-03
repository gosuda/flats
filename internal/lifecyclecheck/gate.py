#!/usr/bin/env python3
"""Black-box acceptance runner: real archive validation, HTTP, CLI and MCP.

Only disposable, loopback servers are started. No provider credentials are read.
JSON evidence is retained locally; do not commit raw server logs.
"""
import argparse
import concurrent.futures
import hashlib
import http.cookiejar
import io
import json
import os
import re
import secrets
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
    patterns = {
        "draft-drift": r"(?:draft|revision|hash|candidate).*(?:changed|stale|mismatch)|stale.*(?:draft|revision|candidate)",
        "policy-drift": r"(?:provider|policy|permission).*(?:changed|stale|mismatch)|stale.*(?:provider|policy)",
        "live-drift": r"(?:live|current|base).*(?:changed|stale|mismatch)|stale.*(?:live|current|base)",
        "visibility-drift": r"visibility.*(?:changed|stale|mismatch)|stale.*visibility",
        "not-permitted": r"(?:no|not|without|absent|missing|unpermitted).*permit|permission|no.*public.*provider|permit.*before.*public",
        "unavailable": r"unavailable|not configured|disabled|not available",
        "health": r"503|not healthy",
        "module-init": r"GATE-MODULE-INITIALIZATION-FAILURE",
        "teardown": r"unconfirmed|teardown|(?:stop|block|clos).*(?:fail|confirm)",
        "serve": r"deterministic provider connection failure",
    }
    require(row.get("status") == "failed", f"{category}: approval did not fail: {row}")
    dto = execution(row)
    require(dto.get('status') == 'failed' and dto.get('failure_code'), f"missing typed failure DTO: {row}")
    cause = json.dumps({"result": row.get("result"), "failure_code": dto['failure_code']})
    require(re.search(patterns[category], cause, re.I), f"{category}: unrelated failure cause: {row}")
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


class Host:
    def __init__(self, binary, work, adapter=False, operator=True):
        self.binary, self.work, self.adapter = str(binary), Path(work), adapter
        self.work.mkdir(parents=True, exist_ok=True)
        self.data = self.work / "data"
        self.proc = None
        self.serial = 0
        self.mcp_session = None
        self.operator = operator
        self.credential = None
        self.phase = None
        self.phase_marker = None
        self.trace = []
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        self.cookies = http.cookiejar.CookieJar()
        self.console_opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(self.cookies))

    def start(self):
        # Reserve both ports until immediately before spawn. Bind-race failures
        # fail the run; never kill another process or reuse the operator's port.
        reservations = [socket.socket(), socket.socket()]
        for sock in reservations:
            sock.bind(("127.0.0.1", 0))
        ports = [sock.getsockname()[1] for sock in reservations]
        self.base = f"http://127.0.0.1:{ports[0]}"
        self.local = f"http://127.0.0.1:{ports[1]}"
        argv = [self.binary, "serve", "--data", str(self.data), "--listen", f"127.0.0.1:{ports[0]}",
                "--network", "local", "--local-addr", f"127.0.0.1:{ports[1]}", "--portal=false"]
        if self.operator:
            argv.append("--operator-credential-stdin")
        for sock in reservations:
            sock.close()
        env = os.environ.copy()
        # Explicitly remove inherited provider credentials; not even their values
        # are inspected or recorded. Worker child keeps this disposable binary.
        for name in ("TS_AUTHKEY", "TS_AUTH_KEY", "FLATS_URL", "HTTP_PROXY", "HTTPS_PROXY",
                     "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy", "FLATS_TEST_PHASE", "FLATS_TEST_PHASE_MARKER"):
            env.pop(name, None)
        if self.phase:
            env['FLATS_TEST_PHASE'] = self.phase
            env['FLATS_TEST_PHASE_MARKER'] = str(self.phase_marker)
        self.log = open(self.work / "serve.log", "ab")
        self.proc = subprocess.Popen(argv, stdin=subprocess.PIPE if self.operator else subprocess.DEVNULL,
                                     stdout=self.log, stderr=self.log, env=env)
        if self.operator:
            self.credential = secrets.token_urlsafe(40)
            self.proc.stdin.write((self.credential + "\n").encode())
            self.proc.stdin.close()
            self.cookies.clear()
        self.trace.append({"argv": argv, "kind": "start"})
        until = time.monotonic() + 30
        while time.monotonic() < until:
            if self.proc.poll() is not None:
                raise AssertionError(f"server exited {self.proc.returncode}; inspect disposable serve.log")
            try:
                if self.request("GET", "/api/status")[0] == 200:
                    if self.operator:
                        self.authorize()
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

    def request(self, method, path, body=None, console=False, headers=None, local_host=None, authorized=True):
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
            opener = self.console_opener if console and authorized else self.opener
            response = opener.open(req, timeout=25)
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

    def authorize(self):
        require(self.credential, "no out-of-band operator authority provisioned")
        self.ok("POST", "/console/api/operator/session", {"credential": self.credential}, console=True)

    def set_host_permissions(self, permitted):
        # Explicit disposable host permission file. This changes permission,
        # never constructs a backend or restarts into a live network provider.
        path = self.data / 'network-provider.json'
        value = json.loads(path.read_text())
        value['permitted'] = permitted
        path.write_text(json.dumps(value) + '\n')
        self.trace.append({'kind': 'disposable-host-permission', 'permitted': permitted})

    def ok(self, method, path, body=None, status=200, **kwargs):
        code, value = self.request(method, path, body, **kwargs)
        require(code == status, f"{method} {path}: wanted {status}, got {code}: {value}")
        return value

    def save(self, slug, files, deploy=False):
        return self.ok("POST", f"/api/flats/{slug}/versions" + ("?deploy=1" if deploy else ""),
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

    def publish(self, slug):
        return self.approval(self.ok("POST", f"/api/flats/{slug}/publish", {"revision": 0}, status=202))

    def decide(self, approval):
        code, out = self.request("POST", f"/console/api/approvals/{approval}/approve", {}, console=True)
        require(code in (200, 409, 422), f"unexpected approval response: {code}: {out}")
        return self.ok("GET", f"/api/approvals/{approval}")

    def activate(self, slug, files):
        self.save(slug, files)
        approval = self.publish(slug)
        require(self.decide(approval)["status"] == "approved", "human approval did not publish")
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
        if result.get("structuredContent"):
            return result["structuredContent"]
        for item in reversed(result.get("content", [])):
            try:
                value = json.loads(item.get("text", ""))
                if isinstance(value, dict):
                    return value
            except ValueError:
                pass
        raise AssertionError(f"no structured MCP result: {result}")


def draft_saves(h):
    for marker in ("DRAFT-A", "DRAFT-B", "DRAFT-C"):
        out = h.save("draft", static(marker))
        require(out["version"]["number"] == 0, f"save allocated published number: {out}")
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
    code, _ = h.request("POST", "/api/flats/validate/versions", archive({"../escape": "bad"}),
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
    a = h.publish("publish")
    require(h.publish("publish") == a, "retry created a second pending approval")
    require(h.flat("publish")["live_version"] == 0, "request activated before human approval")
    code, _ = h.request("POST", f"/api/approvals/{a}/approve", {})
    require(code in (403, 404, 405), "agent approval path exists")
    with concurrent.futures.ThreadPoolExecutor(2) as pool:
        outcomes = list(pool.map(lambda _: h.decide(a), range(2)))
    require(any(o["status"] == "approved" for o in outcomes), outcomes)
    require(h.decide(a)["status"] == "approved", "completed approval retry failed")
    f = h.flat("publish")
    require(f["live_version"] == 1 and f["publication"] == "published" and f["visibility"] == "private", f)
    require([v["number"] for v in h.versions("publish")] == [1], "duplicate version created")
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
    a = h.publish("drift")
    h.save("drift", static("NEW-DRAFT"))
    failed(h.decide(a), "draft-drift")
    require(h.flat("drift")["live_version"] == 0 and h.versions("drift") == [], "stale approval activated")
    require(h.decide(h.publish("drift"))["status"] == "approved", "fresh Draft approval poisoned after drift")
    require("NEW-DRAFT" in h.traffic("drift"), "fresh Draft positive control did not serve")
    h.save("drift", static("POLICY-CANDIDATE"))
    a = h.publish("drift")
    h.ok("POST", "/console/api/flats/drift/providers", {"provider": "tailscale", "permitted": True}, console=True)
    failed(h.decide(a), "policy-drift")
    require(h.flat("drift")["live_version"] == 1, "policy drift activated")
    h.ok("POST", "/console/api/flats/drift/providers", {"provider": "tailscale", "permitted": False}, console=True)
    require(h.decide(h.publish("drift"))["status"] == "approved", "fresh policy approval poisoned after drift")
    require("POLICY-CANDIDATE" in h.traffic("drift"), "policy positive control did not serve")


def cli_pending(h):
    folder = h.work / "upload"
    folder.mkdir()
    for name, text in static("CLI").items():
        (folder / name).write_text(text)
    proc = h.cli("deploy", str(folder), "--flat", "cli")
    require(proc.returncode == 3, f"CLI publish exit should be 3, got {proc.returncode}: {proc.stdout} {proc.stderr}")
    require(h.flat("cli")["live_version"] == 0 and h.versions("cli") == [], "CLI activated")
    # Exercise the console deploy request too: console surface cannot bypass.
    a = h.approval(h.ok("POST", "/console/api/flats/cli/deploy", {"version": 0}, status=202, console=True))
    require(h.flat("cli")["live_version"] == 0, "console request bypassed approval")
    require(h.decide(a)["status"] == "approved", "CLI candidate not approved")
    require(h.flat("cli")["live_version"] == 1 and "CLI" in h.traffic("cli"), "approved CLI bytes not live")


def mcp_pending(h):
    h.initialize_mcp()
    tools = h.rpc("tools/list", {})["tools"]
    require(not any("approve" in t["name"] for t in tools), "MCP has approval capability")
    files = [{"path": name, "content": text, "encoding": "utf8"} for name, text in static("MCP").items()]
    out = h.mcp("save_version", slug="mcp", files=files, deploy=True)
    pending = h.approval(out)
    require(h.flat("mcp")["live_version"] == 0 and h.versions("mcp") == [], "MCP upload/deploy activated")
    require(h.approval(h.mcp("deploy", slug="mcp", version=0)) == pending, "MCP duplicate request did not dedupe")
    require(h.flat("mcp")["live_version"] == 0, "MCP deploy activated")
    require(h.decide(pending)["status"] == "approved", "MCP candidate not approved")
    require(h.flat("mcp")["live_version"] == 1 and "MCP" in h.traffic("mcp"), "approved MCP bytes not live")


def api_pending(h):
    out = h.save("upload", static("API-UPLOAD"), deploy=True)
    h.approval(out)
    require(h.flat("upload")["live_version"] == 0 and h.versions("upload") == [], "API upload activated")
    for console in (False, True):
        prefix = "/console/api" if console else "/api"
        h.approval(h.ok("POST", prefix + "/flats/upload/deploy", {"version": 0}, status=202, console=console))
        require(h.flat("upload")["live_version"] == 0, "deploy surface activated before approval")


def approval_boundary(h):
    h.activate("boundary", static("BOUNDARY-CURRENT-V1"))
    h.save("boundary", static("BOUNDARY-CANDIDATE"))
    pending = h.publish("boundary")
    for prefix in ("/api", "/console/api"):
        for action in ("approve", "reject", "decide", "resolve"):
            code, _ = h.request("POST", f"{prefix}/approvals/{pending}/{action}",
                                {"approved": True, "decision": "approve"}, authorized=False)
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
    inputs = {"slug": "boundary", "flat": "boundary", "id": pending, "approval": pending,
              "approval_id": pending, "version": 0, "revision": 0, "hash": h.flat("boundary").get("draft", {}).get("hash", ""), "visibility": "private",
              "provider": "portal", "permitted": False, "deploy": False, "path": str(folder),
              "dir": str(folder), "directory": str(folder), "restore_data": False,
              "files": [{"path": n, "content": v, "encoding": "utf8"} for n, v in static("BOUNDARY-CANDIDATE").items()]}
    census = []
    for tool in tools:
        schema = tool.get("inputSchema", {})
        args = {key: inputs[key] for key in schema.get("properties", {}) if key in inputs}
        unknown = set(schema.get("required", [])) - set(args)
        require(not unknown, f"behavioral census lacks valid inputs for {tool['name']}: {unknown}")
        result = h.rpc("tools/call", {"name": tool["name"], "arguments": args})
        require(h.ok("GET", f"/api/approvals/{pending}")["status"] == "pending",
                f"MCP tool decided existing approval: {tool['name']}")
        current = h.flat("boundary")
        require(current["live_version"] == 1 and current["visibility"] == "private"
                and provider_ids(current) == {"local"} and len(h.versions("boundary")) == 1
                and "BOUNDARY-CURRENT-V1" in h.traffic("boundary"),
                f"MCP tool changed approved current/policy/history: {tool['name']}")
        census.append({"name": tool["name"], "is_error": result.get("isError", False)})
    # Browser headers alone must never establish operator authority.
    code, response = h.request("POST", f"/console/api/approvals/{pending}/approve", {}, console=True, authorized=False)
    row = h.ok("GET", f"/api/approvals/{pending}")
    require(code == 403 and row['status'] == 'pending', 'forged console headers gained operator authority')
    code, _ = h.request("POST", "/console/api/operator/session", {"credential": "wrong-operator-credential"}, console=True, authorized=False)
    require(code == 403, 'agent credential granted operator session')
    # Correctly provisioned operator session remains a separate positive control.
    failed(h.decide(pending), 'draft-drift')
    require(h.decide(h.publish('boundary'))['status'] == 'approved', 'fresh authorized operator did not publish')
    require('BOUNDARY-CANDIDATE' in h.traffic('boundary'), 'authorized boundary bytes not serving')
    h.save('boundary', static('EXPLICITLY-REJECTED-CANDIDATE'))
    rejection = h.publish('boundary')
    h.ok('POST', f'/console/api/approvals/{rejection}/reject', {}, console=True)
    rejected = h.ok('GET', f'/api/approvals/{rejection}')
    require(rejected['status'] == 'rejected' and rejected.get('decided_by') and rejected.get('authorized_at'),
            'rejection did not persist validated actor/time')
    require(not rejected.get('result_data'), 'rejection synthesized an execution receipt')
    after_rejection = h.flat('boundary')
    require(after_rejection['live_version'] == 2 and after_rejection['visibility'] == 'private'
            and provider_ids(after_rejection) == {'local'}
            and 'BOUNDARY-CANDIDATE' in h.traffic('boundary') and len(h.versions('boundary')) == 2,
            'rejection changed current/policy or allocated a version')
    h.ok('DELETE', '/console/api/operator/session', console=True)
    h.save('boundary', static('REVOKED-SESSION-CANDIDATE'))
    next_pending = h.publish('boundary')
    require(h.request('POST', f'/console/api/approvals/{next_pending}/approve', {}, console=True)[0] == 403,
            'revoked operator session still decides')
    h.authorize()
    h.trace.append({"kind": "human-approval-known-risk", "mcp_census": census,
                    "forged_console_status": code, "approval_status": row["status"],
                    "human_approval_proven": True, "boundary": "separate operator credential; excludes OS/server/browser secret access",
                    "response": response})


def denied_visibility(h, slug):
    before = h.flat(slug)
    pending = h.approval(h.ok("POST", f"/api/flats/{slug}/visibility", {"visibility": "public"}, status=202))
    after = h.flat(slug)
    require(after["visibility"] == before["visibility"] and after["live_version"] == before["live_version"],
            "permission request changed current state before approval")
    failed(h.decide(pending), "not-permitted")


def provider_failure(h):
    h.activate("provider", static("PUBLISHED"))
    require(provider_ids(h.flat("provider")) <= {"local"}, "provider auto permitted")
    code, _ = h.request("POST", "/api/flats/provider/providers", {"provider": "portal", "permitted": True})
    require(code in (403, 404, 405), "agent can grant provider permission")
    denied_visibility(h, "provider")
    h.ok("POST", "/console/api/flats/provider/providers", {"provider": "tailscale", "permitted": True}, console=True)
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
    a = h.publish("counter")
    require(h.traffic("counter")["hits"] == 1, "health ran before approval")
    failure = failed(h.decide(a), "health")
    data_impact(failure)
    require(h.traffic("counter") == {"version": 1, "hits": 1}, "failed health touched live data")
    require(h.traffic("counter", "/health-file") is None, "failed health FILES sentinel escaped copy")
    require([v["number"] for v in h.versions("counter")] == [1], "failed publish consumed number")
    h.activate("counter", server(2, healthy_write=True))
    require(h.traffic("counter", "/health-file") is None, "healthy health FILES sentinel escaped copy")
    require(h.traffic("counter", "/hit") == {"version": 2, "hits": 2}, "second publish lost data")
    a = h.approval(h.ok("POST", "/api/flats/counter/rollback", {"version": 1}, status=202))
    require(h.traffic("counter")["version"] == 2, "rollback applied before approval")
    require(h.decide(a)["status"] == "approved", "rollback failed")
    require(h.traffic("counter") == {"version": 1, "hits": 2}, "code rollback restored data implicitly")
    require(sorted(v["number"] for v in h.versions("counter")) == [1, 2], "rollback allocated number")
    # A restore request is a separately frozen approval, not implicit rollback.
    h.traffic("counter", "/hit")
    a = h.approval(h.ok("POST", "/api/flats/counter/rollback", {"version": 2, "restore_data": True}, status=202))
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
        h.stop()
        h.phase, h.phase_marker = phase, h.work / (phase + '.json')
        h.start()
        slug = "resume-" + str(i)
        h.save(slug, static("RESUME-" + phase))
        a = h.publish(slug)
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
            require(h.flat(slug)["live_version"] == 1 and "RESUME-" + phase in h.traffic(slug), "resumed bytes/pointer mismatch")
            require(len(h.ok("GET", f"/api/flats/{slug}/deployments")["deployments"]) == 1, "crash resume activation count")
            h.stop()
        h.start()
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
    legacy = Host(h.legacy_binary, h.work / "legacy", operator=False)
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
    a = h.publish("historical")
    require(h.decide(a)["status"] == "approved", "migrated draft not publishable")
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
    a = h.publish("binding")
    both("BINDING-V1")
    require(h.decide(a)["status"] == "approved", "public v2 publish failed")
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
    h.ok("POST", "/__gate/host-permission", {"permitted": ["tailscale-funnel", "portal"]})
    def public_request():
        return h.approval(h.ok("POST", "/api/flats/matrix/visibility", {"visibility": "public"}, status=202))
    def allow(provider, permitted=True):
        h.ok("POST", "/console/api/flats/matrix/providers", {"provider": provider, "permitted": permitted}, console=True)
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


from populations import (mixed_migration, seed_mixed, restore_success, runtime_initialization,
                         probe_runtime, current_live_drift, visibility_drift)


LOCAL_CASES = [("draft-save-conflict", draft_saves), ("archive-validation", invalid_upload),
               ("private-draft-traffic", preview), ("publish-human-idempotency-restart", publish_idempotency),
               ("frozen-revision-policy", drift), ("cli-console-pending", cli_pending),
               ("mcp-pending", mcp_pending), ("api-upload-deploy-pending", api_pending),
               ("approval-boundary-census", approval_boundary),
               ("production-fault-hooks-absent", production_hooks_absent),
               ("provider-permission-failure", provider_failure), ("runtime-health-rollback-data", runtime_data),
               ("historical-migration", migration),
               ("historical-mixed-migration", mixed_migration),
               ("runtime-initialization-data", runtime_initialization),
               ("rollback-approved-restore-data", restore_success),
               ("frozen-current-live", current_live_drift)]
ADAPTER_CASES = [("visibility-all-surfaces-teardown", visibility_transitions),
                 ("public-publish-rollback-route-binding", public_publish_rollback),
                 ("partial-provider-failures", partial_provider_failures),
                 ("frozen-visibility", visibility_drift),
                 ("provider-host-flat-permission-matrix", provider_matrix),
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
        host = Host(binary or args.binary, work / name, adapter=adapter)
        host.legacy_binary = args.legacy_binary
        host.legacy_adapter_binary = args.legacy_adapter_binary
        if args.prepare_only:
            require(args.legacy_binary, "preparation needs --legacy-binary")
            host.binary = args.legacy_binary
            host.operator = False
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
