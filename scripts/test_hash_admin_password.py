#!/usr/bin/env python3

from pathlib import Path
import subprocess
import sys
import unittest


class HashAdminPasswordScriptTest(unittest.TestCase):
    def test_default_yaml_contains_phc_and_matching_rollback_sha(self) -> None:
        secret = "script-default-secret"
        script = Path(__file__).with_name("hash-admin-password.py")
        completed = subprocess.run(
            [sys.executable, str(script)],
            input=secret + "\n",
            text=True,
            capture_output=True,
            shell=False,
            check=False,
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertNotIn(secret, completed.stdout)
        self.assertNotIn(secret, completed.stderr)
        self.assertIn("password_hash: \"$argon2id$v=19$", completed.stdout)
        self.assertIn(
            'password_sha256: "868b8c2a0496333cb6617d249176e130f23161d44bdf99630f218b3c1a89d6b3"',
            completed.stdout,
        )

    def test_pipe_input_is_not_echoed(self) -> None:
        secret = "script-test-plaintext"
        script = Path(__file__).with_name("hash-admin-password.py")
        completed = subprocess.run(
            [sys.executable, str(script), "--format", "legacy-sha256"],
            input=secret + "\n",
            text=True,
            capture_output=True,
            shell=False,
            check=False,
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertNotIn(secret, completed.stdout)
        self.assertNotIn(secret, completed.stderr)
        self.assertEqual(
            completed.stdout.strip(),
            "37e16df85f548316ef3175cf86881963eaf3477a0cfb1fc54e083d5dcfd1a43a",
        )


if __name__ == "__main__":
    unittest.main()
