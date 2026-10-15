"""Regression checks for the review's concrete gate false-pass classes."""
import os
import socket
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from gate import Host, accepted, data_impact, failed, mcp_payload, reserve_ports, source_identity


class HarnessChecks(unittest.TestCase):
    def test_acceptance_requires_manager_identity_and_human_boundary(self):
        report = dict(full_suite_selected=True, exit=0, lane_complete=True,
                      identity_bound=True, human_approval="proven")
        self.assertTrue(accepted(report))
        for field, value in (("lane_complete", False), ("identity_bound", False),
                             ("human_approval", "OPEN"), ("full_suite_selected", False), ("exit", 1)):
            self.assertFalse(accepted({**report, field: value}), field)

    def test_failure_needs_its_cause(self):
        failed({"status": "failed", "result": "draft revision changed",
                "result_data": {"status": "failed", "failure_code": "stale_approval"}}, "draft-drift")
        with self.assertRaises(AssertionError):
            failed({"status": "failed", "result": "database unavailable"}, "draft-drift")
        with self.assertRaises(AssertionError):
            failed({"status": "failed", "result": "500 internal error"}, "health")

    def test_impact_requires_a_field_and_truthful_value(self):
        self.assertEqual(data_impact({"data_impact": "none", "health_data": "not_run", "live_data": "untouched"}), "none")
        with self.assertRaises(AssertionError):
            data_impact({"error": 'some mention of "data_impact"'})
        with self.assertRaises(AssertionError):
            data_impact({"data_impact": "none"}, changed=True)

    def test_source_identity_uses_harness_root_from_non_git_cwd(self):
        previous = Path.cwd()
        with tempfile.TemporaryDirectory() as folder:
            try:
                os.chdir(folder)
                identity = source_identity()
            finally:
                os.chdir(previous)
        self.assertEqual(len(identity["head"]), 40)
        self.assertIn("scripts/lifecycle-gate.sh", identity["harness_sha256"])

    def test_draft_upload_cannot_create_an_approval_or_use_version_route(self):
        with tempfile.TemporaryDirectory() as folder:
            host = Host("unused", folder)
            calls = []
            host.ok = lambda *args, **kwargs: calls.append((args, kwargs)) or {"draft": {"revision": 1}}
            host.save("example", {"index.html": "draft"})
            self.assertEqual(calls[0][0][:2], ("POST", "/api/flats/example/draft"))
            self.assertEqual(calls[0][1]["status"], 201)
            with self.assertRaises(AssertionError):
                host.save("example", {"index.html": "draft"}, deploy=True)
            historical = Host("pinned-archive-binary", folder, legacy=True)
            historical.ok = host.ok
            historical.save("example", {"index.html": "historical"})
            self.assertEqual(calls[-1][0][:2], ("POST", "/api/flats/example/versions"))

    def test_version_request_freezes_the_observed_draft_and_requires_new_action(self):
        with tempfile.TemporaryDirectory() as folder:
            host = Host("unused", folder)
            calls = []
            def response(method, path, body=None, **kwargs):
                calls.append((method, path, body))
                if method == "GET":
                    return {"revision": 7, "hash": "frozen-hash"}
                return {"status": "pending_approval", "approval": {"id": "request", "action": "create_version"}}
            host.ok = response
            self.assertEqual(host.create_version("example"), "request")
            self.assertEqual(calls[-1], ("POST", "/api/flats/example/versions", {"revision": 7, "hash": "frozen-hash"}))
            host.ok = lambda *args, **kwargs: ({"revision": 7, "hash": "frozen-hash"} if args[0] == "GET"
                                              else {"status": "pending_approval", "approval": {"id": "wrong", "action": "publish"}})
            with self.assertRaises(AssertionError):
                host.create_version("example")

    def test_mcp_error_payload_accepts_only_a_complete_typed_object(self):
        expected = {"category": "not_design", "error": "not a design"}
        self.assertEqual(mcp_payload({"isError": True, "content": [{"text":
            'not a design\nHint: use save_design.\n{"category":"not_design","error":"not a design"}'}]}), expected)
        self.assertEqual(mcp_payload({"isError": True, "structuredContent": expected}), expected)
        self.assertEqual(mcp_payload({"content": [{"text": '{"status":"pending_approval"}'}]}), {"status": "pending_approval"})
        for text in ('prose {"category":"not_design"}', '{"category":"not_design"} trailing junk',
                     'Hint: {"category":"not_design"}\nnot the structured suffix', '{"error":"missing category"}'):
            with self.subTest(text=text), self.assertRaises(AssertionError):
                mcp_payload({"isError": True, "content": [{"text": text}]})

    def test_permission_readiness_error_requires_exact_category_and_saved_policy(self):
        with tempfile.TemporaryDirectory() as folder:
            host = Host("unused", folder)
            for code, category, saved, accepted_outcome in (
                (200, None, True, True), (409, "provider_not_ready", True, True),
                (409, "provider_not_ready", False, False), (200, None, False, False),
                (409, "conflict", True, False), (503, "provider_not_ready", True, False)):
                with self.subTest(code=code, category=category, saved=saved):
                    host.request = lambda *args, **kwargs: (code, {"category": category})
                    host.flat = lambda slug: {"providers": ["local", "tailscale"] if saved else ["local"]}
                    if accepted_outcome:
                        host.set_flat_permission("example", "tailscale", allow_not_ready=True)
                    else:
                        with self.assertRaises(AssertionError):
                            host.set_flat_permission("example", "tailscale", allow_not_ready=True)
            host.request = lambda *args, **kwargs: (409, {"category": "provider_not_ready"})
            host.flat = lambda slug: {"providers": ["local", "tailscale"]}
            with self.assertRaises(AssertionError):
                host.set_flat_permission("example", "tailscale")
            # Readiness tolerance is only for saving a grant. A revocation
            # must never accept a 409, even with the opt-in enabled and the
            # requested policy already absent from the subsequent read.
            host.flat = lambda slug: {"providers": ["local"]}
            with self.assertRaises(AssertionError):
                host.set_flat_permission("example", "tailscale", permitted=False, allow_not_ready=True)

    def test_owned_port_time_wait_is_reusable_but_live_listener_is_not(self):
        listener = socket.socket()
        listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        listener.bind(("127.0.0.1", 0))
        listener.listen(1)
        port = listener.getsockname()[1]
        # A running listener still prevents a reservation: REUSEPORT is not
        # used, and no unrelated server can be silently replaced.
        with self.assertRaises(OSError):
            reserve_ports((port, 0))
        client = socket.create_connection(("127.0.0.1", port))
        accepted_socket, _ = listener.accept()
        accepted_socket.close()  # active server close produces TIME_WAIT
        self.assertEqual(client.recv(1), b"")
        client.close()
        listener.close()
        sockets, ports = reserve_ports((port, 0))
        try:
            self.assertEqual(ports[0], port)
        finally:
            for sock in sockets:
                sock.close()

    def test_adapter_preflight_failure_explicitly_closes_reserved_ports(self):
        with tempfile.TemporaryDirectory() as folder:
            host = Host("unused", folder, without_tailnet=True)
            sockets, ports = reserve_ports()
            # Keep references alive so garbage collection cannot hide a leak.
            try:
                with patch('gate.reserve_ports', return_value=(sockets, ports)):
                    with self.assertRaisesRegex(AssertionError, 'only to the test adapter'):
                        host.start()
                self.assertTrue(all(sock.fileno() == -1 for sock in sockets))
                self.assertIsNone(host.proc)
            finally:
                for sock in sockets:
                    sock.close()

    def test_stop_rejects_nonzero_exit_and_recovered_panic(self):
        for marker, exit_code in (("", 7), ("panic: simulated handler panic", 0)):
            with self.subTest(marker=marker, exit_code=exit_code), tempfile.TemporaryDirectory() as folder:
                host = Host("unused", folder)
                host.log = open(Path(folder) / "serve.log", "wb")
                host.proc = subprocess.Popen(["python3", "-c", f"print({marker!r}); raise SystemExit({exit_code})"],
                                             stdout=host.log, stderr=host.log)
                host.proc.wait(timeout=5)
                with self.assertRaises(AssertionError):
                    host.stop()


if __name__ == "__main__":
    unittest.main()
