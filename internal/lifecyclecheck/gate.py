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
from pathlib import Path
import socket
import sqlite3
import subprocess
import tarfile
import time
import traceback
import urllib.error
import urllib.parse
import urllib.request


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


def server(version, broken=False):
    # /health writes a sentinel before failure: live DB must not see this write.
    return {"flats.json": '{"kind":"server","health":"/health"}', "server.js": """
export default { async fetch(request, env) {
  env.DB.exec("CREATE TABLE IF NOT EXISTS hits (n INTEGER)");
  const path = new URL(request.url).pathname;
  if (path === "/hit") env.DB.exec("INSERT INTO hits VALUES (1)");
  if (path === "/health" && BROKEN) {
    env.DB.exec("INSERT INTO hits VALUES (999)");
    return new Response("not healthy", {status:503});
  }
  const [{c}] = env.DB.query("SELECT count(*) AS c FROM hits");
  return Response.json({version: VERSION, hits:c});
}};
""".replace("VERSION", str(version)).replace("BROKEN", str(broken).lower())}


class Host:
    def __init__(self, binary, work, adapter=False):
        self.binary, self.work, self.adapter = str(binary), Path(work), adapter
        self.work.mkdir(parents=True, exist_ok=True)
        self.data = self.work / "data"
        self.proc = None
        self.serial = 0
        self.trace = []
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

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
        for sock in reservations:
            sock.close()
        env = os.environ.copy()
        # Explicitly remove inherited provider credentials; not even their values
        # are inspected or recorded. Worker child keeps this disposable binary.
        for name in ("TS_AUTHKEY", "FLATS_URL"):
            env.pop(name, None)
        self.log = open(self.work / "serve.log", "ab")
        self.proc = subprocess.Popen(argv, stdout=self.log, stderr=self.log, env=env)
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
            self.proc.terminate()
            try:
                self.proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=5)
            self.trace.append({"kind": "stop", "exit": self.proc.returncode})
            self.proc = None
            self.log.close()

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
        raw = body if isinstance(body, bytes) else (None if body is None else json.dumps(body).encode())
        req = urllib.request.Request(base + path, data=raw, headers=hdr, method=method)
        try:
            response = self.opener.open(req, timeout=25)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            status, raw = response.code, response.read()
        try:
            value = json.loads(raw)
        except (ValueError, UnicodeDecodeError):
            value = raw.decode(errors="replace")
        self.trace.append({"method": method, "path": path, "console": console,
                           "host": local_host, "status": status, "response": value})
        return status, value

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
        require(code in (200, 409, 422, 500), f"unexpected approval response: {code}: {out}")
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
        proc = subprocess.run(argv, capture_output=True, text=True, timeout=30)
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
    code, _ = h.request("POST", "/api/flats/validate/versions", archive({"../escape": "bad"}),
                        headers={"Content-Type": "application/gzip"})
    require(code == 422, f"unsafe archive not rejected: {code}")
    require(h.flat("validate") == before, "invalid upload changed flat")


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
    require(h.decide(a)["status"] == "failed", "draft drift accepted stale approval")
    require(h.flat("drift")["live_version"] == 0 and h.versions("drift") == [], "stale approval activated")
    a = h.publish("drift")
    h.ok("POST", "/console/api/flats/drift/providers", {"provider": "tailscale", "permitted": True}, console=True)
    require(h.decide(a)["status"] == "failed", "provider policy drift accepted stale approval")
    require(h.flat("drift")["live_version"] == 0, "policy drift activated")


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


def mcp_pending(h):
    h.rpc("initialize", {"protocolVersion": "2025-03-26", "capabilities": {},
                         "clientInfo": {"name": "lifecyclecheck", "version": "1"}})
    tools = h.rpc("tools/list", {})["tools"]
    require(not any("approve" in t["name"] for t in tools), "MCP has approval capability")
    files = [{"path": name, "content": text, "encoding": "utf8"} for name, text in static("MCP").items()]
    out = h.mcp("save_version", slug="mcp", files=files, deploy=True)
    h.approval(out)
    require(h.flat("mcp")["live_version"] == 0 and h.versions("mcp") == [], "MCP upload/deploy activated")
    h.approval(h.mcp("deploy", slug="mcp", version=0))
    require(h.flat("mcp")["live_version"] == 0, "MCP deploy activated")


def api_pending(h):
    out = h.save("upload", static("API-UPLOAD"), deploy=True)
    h.approval(out)
    require(h.flat("upload")["live_version"] == 0 and h.versions("upload") == [], "API upload activated")
    for console in (False, True):
        prefix = "/console/api" if console else "/api"
        h.approval(h.ok("POST", prefix + "/flats/upload/deploy", {"version": 0}, status=202, console=console))
        require(h.flat("upload")["live_version"] == 0, "deploy surface activated before approval")


def provider_failure(h):
    h.activate("provider", static("PUBLISHED"))
    require(h.flat("provider").get("providers") in ([], ["local"]), "provider auto permitted")
    code, _ = h.request("POST", "/api/flats/provider/providers", {"provider": "portal", "permitted": True})
    require(code in (403, 404, 405), "agent can grant provider permission")
    a = h.approval(h.ok("POST", "/api/flats/provider/visibility", {"visibility": "public"}, status=202))
    require(h.flat("provider")["visibility"] == "private", "visibility changed before approval")
    require(h.decide(a)["status"] == "failed", "Public succeeded without available/permitted provider")
    h.ok("POST", "/console/api/flats/provider/providers", {"provider": "tailscale", "permitted": True}, console=True)
    require(h.flat("provider")["visibility"] == "private", "connection permission authorized Public")
    require("tailscale-funnel" not in h.flat("provider")["providers"], "Tailscale authorized Funnel")
    h.ok("POST", "/console/api/flats/provider/providers", {"provider": "tailscale-funnel", "permitted": True}, console=True)
    require(h.flat("provider")["visibility"] == "private", "Funnel permission authorized Public")
    a = h.approval(h.ok("POST", "/api/flats/provider/visibility", {"visibility": "public"}, status=202))
    require(h.decide(a)["status"] == "failed", "unavailable Funnel reported success")
    f = h.flat("provider")
    require(f["publication"] == "published" and f["live_version"] == 1 and f["visibility"] == "private", f)
    require("PUBLISHED" in h.traffic("provider"), "provider failure lost current version")


def runtime_data(h):
    h.activate("counter", server(1))
    require(h.traffic("counter", "/hit")["hits"] == 1, "runtime did not persist hit")
    h.save("counter", server(2, broken=True))
    a = h.publish("counter")
    require(h.traffic("counter")["hits"] == 1, "health ran before approval")
    require(h.decide(a)["status"] == "failed", "broken health published")
    require(h.traffic("counter") == {"version": 1, "hits": 1}, "failed health touched live data")
    require([v["number"] for v in h.versions("counter")] == [1], "failed publish consumed number")
    h.activate("counter", server(2))
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
    h.save("resume", static("RESUME-CANDIDATE"))
    a = h.publish("resume")
    h.stop()
    # Deterministic crash boundary after persisted claim but before activation.
    # Do not inject a fake published row: restored service must perform work.
    with sqlite3.connect(h.data / "flats.db") as db:
        require(db.execute("UPDATE approvals SET status='applying' WHERE id=? AND status='pending'", (a,)).rowcount == 1,
                "could not model crash after approval claim")
    h.start()
    require(h.ok("GET", f"/api/approvals/{a}")["status"] == "approved", "claimed approval not resumed")
    require("RESUME-CANDIDATE" in h.traffic("resume"), "resumed activation did not serve bytes")
    require(len(h.ok("GET", "/api/flats/resume/deployments")["deployments"]) == 1, "resume activation count")
    h.stop()
    h.start()
    require(len(h.ok("GET", "/api/flats/resume/deployments")["deployments"]) == 1, "second restart activated twice")


def migration(h):
    require(h.legacy_binary, "migration needs --legacy-binary from pre-lifecycle base")
    h.stop()
    legacy = Host(h.legacy_binary, h.work / "legacy")
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
    require(after == sorted(before.values()), "migration lost or changed saved files")
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
    h.activate("public", static("PUBLIC-V1"))
    h.ok("POST", "/console/api/flats/public/providers", {"provider": "portal", "permitted": True}, console=True)
    h.rpc("initialize", {"protocolVersion": "2025-03-26", "capabilities": {},
                         "clientInfo": {"name": "lifecyclecheck", "version": "1"}})
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
    require(h.decide(a)["status"] == "failed", "unconfirmed teardown reported approval success")
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
    require(h.decide(a)["status"] == "failed", "provider fault reported public success")
    f = h.flat("public")
    require(f["publication"] == "published" and f["visibility"] == "private", "network fault changed publication")
    require(h.request("GET", "/", local_host="pub-public")[0] == 404, "network fault opened route")


LOCAL_CASES = [("draft-save-conflict", draft_saves), ("archive-validation", invalid_upload),
               ("private-draft-traffic", preview), ("publish-human-idempotency-restart", publish_idempotency),
               ("frozen-revision-policy", drift), ("cli-console-pending", cli_pending),
               ("mcp-pending", mcp_pending), ("api-upload-deploy-pending", api_pending),
               ("provider-permission-failure", provider_failure), ("runtime-health-rollback-data", runtime_data),
               ("restart-claimed-approval", resume_claimed), ("historical-migration", migration)]
ADAPTER_CASES = [("visibility-all-surfaces-teardown", visibility_transitions)]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True)
    parser.add_argument("--work", required=True)
    parser.add_argument("--only", help="comma separated case IDs")
    parser.add_argument("--legacy-binary")
    parser.add_argument("--adapter-binary")
    args = parser.parse_args()
    work = Path(args.work)
    work.mkdir(parents=True, exist_ok=True)
    report = {"binary_sha256": hashlib.sha256(Path(args.binary).read_bytes()).hexdigest(),
              "source_head": subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip(),
              "cases": [], "scope": "actual binary, disposable Local only; no external provider traffic"}
    cases = LOCAL_CASES + ADAPTER_CASES
    selected = set(args.only.split(",")) if args.only else {n for n, _ in cases}
    require(selected <= {n for n, _ in cases}, f"unknown case IDs: {selected}")
    for name, test in cases:
        if name not in selected:
            continue
        adapter = (name, test) in ADAPTER_CASES
        host = Host((args.adapter_binary or args.binary) if adapter else args.binary, work / name, adapter=adapter)
        host.legacy_binary = args.legacy_binary
        result = {"id": name, "lane": "deterministic-provider-loopback" if adapter else "actual-binary-local"}
        try:
            require(not adapter or args.adapter_binary, "missing --adapter-binary; provider lane cannot pass")
            host.start()
            test(host)
            result["outcome"] = "pass"
        except Exception as error:
            result.update(outcome="fail", error=str(error), traceback=traceback.format_exc())
        finally:
            host.stop()
            result["trace"] = host.trace
        report["cases"].append(result)
        print(f"{result['outcome'].upper()}: {name}" + (": " + result["error"] if "error" in result else ""), flush=True)
    report["exit"] = int(any(c["outcome"] != "pass" for c in report["cases"]))
    (work / "evidence.json").write_text(json.dumps(report, indent=2) + "\n")
    print(f"Evidence: {work / 'evidence.json'}", flush=True)
    return report["exit"]


if __name__ == "__main__":
    raise SystemExit(main())
