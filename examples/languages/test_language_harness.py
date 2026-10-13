"""Checks that the language suite explains its explicit runner prerequisite."""

import subprocess
import sys
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
LANGUAGE_TEST = HERE / "test_languages.py"


class LanguageHarnessTest(unittest.TestCase):
    def test_unittest_discovery_without_runner_reports_a_clear_skip(self):
        result = subprocess.run(
            [sys.executable, "-B", "-m", "unittest", "discover", "-s", str(HERE),
             "-p", "test_languages.py", "-v"],
            cwd=HERE.parents[1], text=True, capture_output=True, timeout=15,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("language acceptance needs an explicit TAP runner", result.stderr)
        self.assertNotIn("NameError", result.stderr)

    def test_direct_language_suite_requires_a_runner_argument(self):
        result = subprocess.run(
            [sys.executable, "-B", str(LANGUAGE_TEST)],
            cwd=HERE.parents[1], text=True, capture_output=True, timeout=15,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("--runner", result.stderr)
        self.assertIn("required", result.stderr)


if __name__ == "__main__":
    unittest.main()
