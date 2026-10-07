"""Execute real TAP guests, including permission and malformed-data failures.

python3 -B examples/languages/test_languages.py --runner /absolute/path/to/tap
The runner is explicit so an old global installation cannot silently be tested.
"""
import argparse
import json
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
LANGUAGES = ("bash", "python", "javascript", "typescript", "go", "cpp")


class LanguageRuntimeTest(unittest.TestCase):
    def setUp(self):
        self.scratch = tempfile.TemporaryDirectory(prefix="tap-language-test-")
        self.addCleanup(self.scratch.cleanup)
        self.root = Path(self.scratch.name)
        shutil.copytree(REPO / "examples/languages", self.root / "examples/languages")
        self.fixture = self.root / "examples/languages/fixtures/tasks.json"

    def run_guest(self, language, payload):
        result = subprocess.run(
            [RUNNER, "--cache", str(CACHE), "--runs", str(self.root / "runs"),
             str(self.root / "examples/languages" / language), json.dumps(payload)],
            cwd=self.root, text=True, capture_output=True, timeout=90,
        )
        marker = re.search(r"^RESULT \(exit (\d+)\)\n", result.stdout, re.MULTILINE)
        output = result.stdout[marker.end():] if marker else ""
        return result, output

    def test_same_extraction_with_changed_inputs(self):
        for language in LANGUAGES:
            for status, ids in [("open", ["TAP-1", "TAP-3"]), ("done", ["TAP-2"])]:
                with self.subTest(language=language, status=status):
                    result, output = self.run_guest(language, {"status": status})
                    self.assertEqual(result.returncode, 0, result.stderr)
                    data = json.loads(output)
                    self.assertEqual(data["status"], status)
                    self.assertEqual(data["count"], len(ids))
                    self.assertEqual([row["id"] for row in data["items"]], ids)
                    self.assertIn("1 call(s) and command(s) run, 0 refused", result.stderr)

    def test_invalid_inputs_fail_before_read(self):
        for language in LANGUAGES:
            for payload in ({"status": "unknown"}, {"status": "open", "extra": True}):
                with self.subTest(language=language, payload=payload):
                    result, _ = self.run_guest(language, payload)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("0 call(s) and command(s) run", result.stderr)

    def test_malformed_task_data_is_not_reported_as_empty_success(self):
        self.fixture.write_text('[{"id":12,"title":"bad","status":"open"}]')
        for language in LANGUAGES:
            with self.subTest(language=language):
                result, _ = self.run_guest(language, {"status": "open"})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("1 call(s) and command(s) run", result.stderr)

    def test_removing_read_declaration_refuses_real_broker_request(self):
        for language in LANGUAGES:
            with self.subTest(language=language):
                manifest = self.root / "examples/languages" / language / "primitive.yaml"
                manifest.write_text(manifest.read_text().replace(
                    "files:\n  - {path: examples/languages/fixtures/tasks.json, access: read}\n", ""))
                result, _ = self.run_guest(language, {"status": "open"})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("0 call(s) and command(s) run, 1 refused", result.stderr)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--runner", required=True)
    args, remaining = parser.parse_known_args()
    RUNNER = str(Path(args.runner).resolve(strict=True))
    with tempfile.TemporaryDirectory(prefix="tap-language-cache-") as cache:
        CACHE = Path(cache)
        unittest.main(argv=[__file__] + remaining, verbosity=2)
