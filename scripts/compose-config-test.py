#!/usr/bin/env python3
"""Check Compose's resolved configuration without starting containers.

Run with: python3 scripts/compose-config-test.py
Requires Docker Compose v2 or newer; no Docker daemon is needed.
"""

import json
import os
from pathlib import Path
import subprocess
import unittest

ROOT = Path(__file__).resolve().parent.parent


class ComposeConfigTest(unittest.TestCase):
    def config(self, **overrides):
        env = {key: value for key, value in os.environ.items() if key != "FLATS_IMAGE"}
        env.update(overrides)
        result = subprocess.run(
            ["docker", "compose", "--env-file", os.devnull, "-f", str(ROOT / "compose.yaml"), "config", "--format", "json"],
            env=env, capture_output=True, text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    def test_private_persistent_host(self):
        config = self.config()
        service = config["services"]["flats"]
        self.assertEqual(service["image"], "ghcr.io/gosuda/flats:latest")
        self.assertEqual(service["command"], ["serve", "--listen", "0.0.0.0:7878", "--local-addr", "0.0.0.0:7879"])
        self.assertEqual(service["restart"], "unless-stopped")
        self.assertEqual(
            {(p["host_ip"], str(p["published"]), p["target"]) for p in service["ports"]},
            {("127.0.0.1", "7878", 7878), ("127.0.0.1", "7879", 7879)},
        )
        self.assertEqual(service["volumes"][0]["type"], "volume")
        self.assertEqual(service["volumes"][0]["target"], "/data")
        self.assertIn(service["volumes"][0]["source"], config["volumes"])
        self.assertEqual(set(service["networks"]), {"default"})
        self.assertNotIn("network_mode", service)
        self.assertTrue(service["read_only"])
        self.assertEqual(service["cap_drop"], ["ALL"])
        self.assertIn("no-new-privileges:true", service["security_opt"])
        self.assertIn("/tmp", service["tmpfs"])

    def test_image_override(self):
        image = "ghcr.io/gosuda/flats:1.2.3"
        self.assertEqual(self.config(FLATS_IMAGE=image)["services"]["flats"]["image"], image)


if __name__ == "__main__":
    unittest.main()
