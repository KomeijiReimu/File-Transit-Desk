#!/usr/bin/env python3
"""Wrapper contract only; Argon2/output-format correctness belongs to the Go CLI tests."""

import importlib.util
import io
from pathlib import Path
import subprocess
import sys
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("password_helper", Path(__file__).with_name("hash-admin-password.py"))
assert SPEC is not None and SPEC.loader is not None
HELPER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(HELPER)


class HashAdminPasswordScriptTest(unittest.TestCase):
    def test_password_stays_on_stdin_and_formats_are_forwarded(self) -> None:
        secret = "not-a-process-argument"
        for output_format in ("bootstrap", "yaml", "phc", "legacy-sha256"):
            with self.subTest(output_format=output_format), patch.object(sys, "argv", ["helper", "--format", output_format]), patch.object(sys, "stdin", io.StringIO(secret + "\n")), patch.object(sys, "stdout", io.StringIO()) as stdout, patch.object(HELPER.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, "hash-output\n", "")) as run:
                HELPER.main()
                args, kwargs = run.call_args
                self.assertNotIn(secret, " ".join(args[0]))
                self.assertEqual(args[0][-2:], ["--format", output_format])
                self.assertEqual(kwargs["input"], secret + "\n")
                self.assertFalse(kwargs["shell"])
                self.assertEqual(stdout.getvalue(), "hash-output\n")

    def test_native_error_code_is_preserved_without_success_output(self) -> None:
        with patch.object(sys, "argv", ["helper"]), patch.object(sys, "stdin", io.StringIO("secret\n")), patch.object(sys, "stdout", io.StringIO()) as stdout, patch.object(sys, "stderr", io.StringIO()) as stderr, patch.object(HELPER.subprocess, "run", return_value=subprocess.CompletedProcess([], 23, "", "Go CLI failed")):
            with self.assertRaises(SystemExit) as error:
                HELPER.main()
            self.assertEqual(error.exception.code, 23)
            self.assertEqual(stdout.getvalue(), "")
            self.assertIn("Go CLI failed", stderr.getvalue())

    def test_invalid_input_never_launches_go(self) -> None:
        for value in ("", "密" * 342):
            with self.subTest(length=len(value)), patch.object(sys, "argv", ["helper"]), patch.object(sys, "stdin", io.StringIO(value)), patch.object(HELPER.subprocess, "run") as run:
                with self.assertRaises(SystemExit):
                    HELPER.main()
                run.assert_not_called()


if __name__ == "__main__":
    unittest.main()
