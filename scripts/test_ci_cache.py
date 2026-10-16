"""Exercise cache metadata with source scopes and event permissions."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / '.github/actions/setup-go/metadata.sh'


class CacheMetadataTests(unittest.TestCase):
    def metadata(self, scope='root', event='push', ref='refs/heads/main', root_hash='root-deps'):
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory)
            go = work / 'go'
            go.write_text('''#!/bin/sh
case "$2" in
GOVERSION) echo go1.27.1 ;;
GOCACHE) echo /fixture/build ;;
GOMODCACHE) echo /fixture/modules ;;
*) exit 1 ;;
esac
''')
            go.chmod(0o755)
            output = work / 'output'
            output.touch()
            env = {**os.environ, 'PATH': f"{work}:{os.environ['PATH']}",
                   'MODULE_SCOPE': scope, 'ROOT_HASH': root_hash,
                   'NETBIRD_HASH': 'both-module-deps', 'GITHUB_OUTPUT': str(output),
                   'GITHUB_EVENT_NAME': event, 'GITHUB_REF': ref}
            result = subprocess.run(['bash', str(SCRIPT)], env=env, capture_output=True, text=True)
            values = dict(line.split('=', 1) for line in output.read_text().splitlines())
            return result, values

    def test_root_and_nested_dependency_scopes(self):
        for scope, expected in [('root', 'root-deps'), ('netbird', 'both-module-deps')]:
            with self.subTest(scope=scope):
                result, values = self.metadata(scope=scope)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(values['hash'], expected)
                self.assertEqual(values['toolchain'], 'go1.27.1')
                self.assertRegex(values['week'], r'^\d{4}-\d{2}$')
                self.assertEqual(values['save'], 'true')

    def test_non_main_push_events_never_write_cache(self):
        for event, ref in [('pull_request', 'refs/pull/1/merge'),
                           ('workflow_dispatch', 'refs/heads/main'),
                           ('push', 'refs/tags/v1.0.0'), ('push', 'refs/heads/topic')]:
            with self.subTest(event=event, ref=ref):
                result, values = self.metadata(event=event, ref=ref)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertNotIn('save', values)

    def test_invalid_scope_and_missing_manifests_fail(self):
        for kwargs in [{'scope': 'unknown'}, {'root_hash': ''}]:
            result, values = self.metadata(**kwargs)
            self.assertNotEqual(result.returncode, 0)
            self.assertNotIn('save', values)


if __name__ == '__main__':
    unittest.main()
