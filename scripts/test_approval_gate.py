"""Local fake-client contract tests; never starts Flats or a real provider."""
import http.server
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import unittest


class GateApprovalTests(unittest.TestCase):
    def run_gate(self, *, disposable, outcome="approved", cleanup=False):
        calls = []
        decisions = set()

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_POST(self):
                calls.append(("POST", self.path))
                self.send_response(200)
                self.end_headers()
                decisions.add(self.path.split("/")[-2])
                self.wfile.write(b"{}")

            def do_GET(self):
                calls.append(("GET", self.path))
                identity = self.path.split("/")[-1]
                state = outcome
                if disposable and identity not in decisions:
                    state = "pending"
                response = {"id": identity, "status": state,
                            "result_data": {"version": 11, "cleanup_pending": cleanup}}
                self.send_response(200)
                self.end_headers()
                self.wfile.write(json.dumps(response).encode())

        with tempfile.TemporaryDirectory() as directory:
            cli = Path(directory) / "flats"
            cli.write_text("""#!/usr/bin/env python3
import json,sys
args=sys.argv[1:]
assert args[0]=='--json',args
if args[1]=='save-draft':
 assert args[2:]==['gate','build'],args
 print(json.dumps({'draft':{'revision':7,'hash':'saved-hash'}}))
 sys.exit(0)
if args[1]=='create-version':
 assert args[2:]==['gate','--revision','7','--hash','saved-hash'],args
 identity='apr-create'
else:
 assert args[1:]==['deploy','--flat','gate','--version','11'],args
 identity='apr-deploy'
print(json.dumps({'status':'pending_approval','approval':{'id':identity},'approval_url':'http://console.test/approvals/'+identity}))
sys.exit(3)
""")
            cli.chmod(0o700)
            server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                helper = Path(__file__).with_name("approval-gate.sh").resolve()
                env = {**os.environ, "F": str(cli),
                       "FLATS_URL": f"http://127.0.0.1:{server.server_port}",
                       "FLATS_GATE_DISPOSABLE_OPERATOR": str(int(disposable)),
                       "FLATS_GATE_APPROVAL_TIMEOUT": "2"}
                result = subprocess.run(
                    ["bash", "-euc", 'source "$1"; gate_save_and_deploy gate build', "gate", str(helper)],
                    env=env, capture_output=True, text=True, timeout=10)
            finally:
                server.shutdown()
                server.server_close()
                thread.join()
        return result, calls

    def test_disposable_operator_approves_only_exact_requests(self):
        result, calls = self.run_gate(disposable=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([p for method, p in calls if method == "POST"], [
            "/console/api/approvals/apr-create/approve",
            "/console/api/approvals/apr-deploy/approve"])
        self.assertIn("OPERATOR INTERVENTION", result.stderr)
        self.assertTrue(all("apr-create" in p or "apr-deploy" in p for _, p in calls))

    def test_real_operator_mode_never_sends_a_decision(self):
        result, calls = self.run_gate(disposable=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(all(method == "GET" for method, _ in calls))
        self.assertIn("http://console.test/approvals/apr-create", result.stderr)
        self.assertNotIn("OPERATOR INTERVENTION", result.stderr)

    def test_rejection_stops_before_deploy(self):
        result, calls = self.run_gate(disposable=False, outcome="rejected")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ended rejected", result.stderr)
        self.assertEqual(calls, [("GET", "/api/approvals/apr-create")])

    def test_cleanup_pending_is_not_reported_complete(self):
        result, calls = self.run_gate(disposable=False, cleanup=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Do not repeat deletion", result.stderr)
        self.assertEqual(calls, [("GET", "/api/approvals/apr-create")])


class SecretLogTests(unittest.TestCase):
    def check_logs(self, mode, *, broken_grep=False):
        source = Path(__file__).with_name("server-flat-gate.sh").read_text()
        check = source.split("# BEGIN secret log check\n", 1)[1].split(
            "# END secret log check", 1)[0]
        helpers = [line for line in source.splitlines()
                   if line.startswith(("pass()", "fail()"))]
        self.assertEqual(len(helpers), 2)
        with tempfile.TemporaryDirectory() as directory:
            executable = Path(directory) / "flats"
            executable.write_text("""#!/usr/bin/env python3
import os,sys
assert sys.argv[1:]==['logs','counter'],sys.argv
mode=os.environ['LOG_MODE']
if mode=='failed':
 print('otherwise safe output');sys.exit(7)
if mode=='secret': print('s3cr3t-value')
# Exceed normal pipe capacity after the early match. The former grep -q
# pipeline caused BrokenPipeError in this fake writer and incorrectly passed.
sys.stdout.write('safe log entry\\n'*150000)
""")
            executable.chmod(0o700)
            if broken_grep:
                grep = Path(directory) / "grep"
                grep.write_text("#!/bin/sh\nexit 2\n")
                grep.chmod(0o700)
            result = subprocess.run(
                ["bash", "-uo", "pipefail", "-c",
                 'FAIL=0;\n' + '\n'.join(helpers) + '\n' + check + '\nexit "$FAIL"'],
                env={**os.environ, "F": str(executable), "LOG_MODE": mode,
                     "PATH": directory + os.pathsep + os.environ["PATH"]},
                capture_output=True, text=True, timeout=10)
        return result

    def test_failed_logs_never_pass_secret_check(self):
        result = self.check_logs("failed")
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("cannot read logs", result.stdout)
        self.assertNotIn("PASS:", result.stdout)

    def test_early_secret_in_large_output_is_rejected(self):
        result = self.check_logs("secret")
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("secret value leaked", result.stdout)
        self.assertNotIn("PASS:", result.stdout)

    def test_successful_large_safe_logs_pass(self):
        result = self.check_logs("safe")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("PASS: secret value not in logs", result.stdout)

    def test_broken_grep_cannot_hide_secret(self):
        result = self.check_logs("secret", broken_grep=True)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("secret value leaked", result.stdout)
        self.assertNotIn("PASS:", result.stdout)


class ListenerOwnerTests(unittest.TestCase):
    def test_listener_requires_live_owned_process_and_tailscale(self):
        source = Path(__file__).with_name("tailnet-gate.sh").read_text()
        check = 'if ! kill -0 "$PID"' + source.split(
            'if ! kill -0 "$PID"', 1)[1].split('\nfi', 1)[0] + '\nfi'
        # Override kill and tail only in this child shell: never probe or signal
        # an actual process, start the gate, or contact a provider.
        for owned in (0, 1):
            for kind in ("tailscale", "local"):
                with self.subTest(kill_exit=owned, kind=kind):
                    result = subprocess.run(
                        ["bash", "-euc", 'kill(){ return "$OWNED"; }; tail(){ :; }; '
                         'PID=123; PORT=1; WORK=unused;\n' + check],
                        env={**os.environ, "OWNED": str(owned), "kind": kind},
                        capture_output=True, text=True, timeout=2)
                    self.assertEqual(result.returncode,
                                     0 if owned == 0 and kind == "tailscale" else 1,
                                     result.stderr)


class ContainerApprovalTests(unittest.TestCase):
    def test_container_cli_uses_exact_create_then_deploy_and_live_docs(self):
        # Execute the checked-in container helpers with fake executables only;
        # loading their definitions must not execute docker or start a host.
        source = Path(__file__).with_name("container-smoke.sh").read_text()
        helpers = source.split("# console sends", 1)[1].split("# smoke MODE", 1)[0]
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            executable = root / "flats"
            executable.write_text("""#!/usr/bin/env python3
import json,os,sys
args=sys.argv[1:]
with open(os.environ['CALL_LOG'],'a') as stream: stream.write(json.dumps(['cli',args])+'\\n')
if args==['--json','save-draft','hello',os.environ['work']+'/hello']:
 print(json.dumps({'draft':{'revision':7,'hash':'captured-seven'}}));sys.exit(0)
if args==['--json','create-version','hello','--revision','7','--hash','captured-seven']:
 identity='apr-created-seven'
elif args==['--json','deploy','--flat','hello','--version','11']:
 identity='apr-deploy-eleven'
elif args==['--json','save-document','document',os.environ['work']+'/document']:
 print(json.dumps({'flat':{'type':'docs','running':True}}));sys.exit(0)
elif args==['approvals','--status','pending','--json']:
 print(json.dumps({'approvals':[]}));sys.exit(0)
else: raise AssertionError(args)
print(json.dumps({'status':'pending_approval','approval':{'id':identity}}));sys.exit(3)
""")
            curl = root / "curl"
            curl.write_text("""#!/usr/bin/env python3
import json,os,sys
args=sys.argv[1:];url=args[-1]
method=args[args.index('-X')+1] if '-X' in args else 'GET'
with open(os.environ['CALL_LOG'],'a') as stream: stream.write(json.dumps(['http',method,url])+'\\n')
identities=('apr-created-seven','apr-deploy-eleven')
if method=='POST':
 assert any(url.endswith('/console/api/approvals/'+identity+'/approve') for identity in identities),args
 for header in ('X-Flats-Console: 1','Origin: http://127.0.0.1:9999','Sec-Fetch-Site: same-origin'): assert header in args,args
 print('{}')
else:
 assert any(url.endswith('/api/approvals/'+identity) for identity in identities),args
 print(json.dumps({'status':'approved','result_data':{'version':11}}))
""")
            executable.chmod(0o700)
            curl.chmod(0o700)
            definition = root / "helpers.sh"
            definition.write_text("# console sends" + helpers)
            log = root / "calls.jsonl"
            env = {**os.environ, "flats": str(executable), "work": directory,
                   "url": "http://127.0.0.1:9999", "CALL_LOG": str(log),
                   "PATH": str(root) + os.pathsep + os.environ["PATH"]}
            result = subprocess.run(
                ["sh", "-euc", 'fail(){ echo "$*" >&2; exit 1; }; wait_for(){ shift; "$@"; }; . "$1"; deploy_approved hello; deploy_document document',
                 "container-test", str(definition)],
                env=env, capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            calls = [json.loads(line) for line in log.read_text().splitlines()]
            self.assertEqual([item[2] for item in calls if item[:2] == ["http", "POST"]], [
                "http://127.0.0.1:9999/console/api/approvals/apr-created-seven/approve",
                "http://127.0.0.1:9999/console/api/approvals/apr-deploy-eleven/approve"])
            self.assertEqual([item[1][1] for item in calls if item[0] == "cli" and item[1][0] == "--json"],
                             ["save-draft", "create-version", "deploy", "save-document"])


if __name__ == "__main__":
    unittest.main()
