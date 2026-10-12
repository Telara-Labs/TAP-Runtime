"""Execute real TAP guests, including permission and malformed-data failures.

python3 -B examples/languages/test_languages.py --runner /absolute/path/to/tap
The runner is explicit so an old global installation cannot silently be tested.
"""
import argparse
import hashlib
import json
import os
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
LANGUAGES = ("bash", "python", "javascript", "typescript", "go", "cpp", "rust")


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

    def install_child(self):
        """Install the Python task summary where the runner's catalog looks,
        and return the lines that pin it under uses."""
        child = self.root / ".claude/skills/task-summary-python"
        shutil.copytree(self.root / "examples/languages/python", child)
        (child / ".tap-primitive.json").write_text(json.dumps(
            {"name": "examples.telara.dev/task-summary-python", "digest": "installed"}))
        digest = hashlib.sha256((child / "primitive.yaml").read_bytes() + (child / "main.py").read_bytes()).hexdigest()
        return ("uses:\n  - {alias: tasks, ref: examples.telara.dev/task-summary-python, "
                "version: 0.1.0, digest: %s, effect: read}\n" % digest)

    def test_a_primitive_runs_a_pinned_primitive_in_every_source_language(self):
        uses = self.install_child()
        programs = {
            "main.py": 'import json\nprint(json.dumps(tap.run("tasks", {"status": "open"})))\n',
            "main.js": 'print(JSON.stringify(tap.run("tasks", { status: "open" })));\n',
            "main.ts": 'declare const tap: any;\nconst r: any = tap.run("tasks", { status: "open" });\nprint(JSON.stringify(r));\n',
            "main.sh": "tap run tasks '{\"status\":\"open\"}'\n",
        }
        # A runner built from source builds its bash interpreter itself, so
        # the bash guest is tested with one built from this checkout's source.
        store = self.root / "interpreters"
        store.mkdir()
        built = shutil.which("go") and subprocess.run(
            ["go", "build", "-o", str(store / "sh.wasm"), "./guest-sh"], cwd=REPO,
            env={**os.environ, "GOOS": "wasip1", "GOARCH": "wasm", "GOWORK": "off"}).returncode == 0
        for entry, program in programs.items():
            if entry == "main.sh" and not built:
                continue
            with self.subTest(entry=entry):
                parent = self.root / ("parent-" + entry.split(".")[1])
                parent.mkdir()
                (parent / entry).write_text(program)
                (parent / "primitive.yaml").write_text(
                    "apiVersion: primitives.telara.dev/v3\nkind: Primitive\n"
                    "metadata: {publisher: examples.telara.dev, name: parent-%s, version: 0.1.0}\n"
                    "execution: {entrypoint: %s}\n%s" % (entry.split(".")[1], entry, uses))
                result = subprocess.run(
                    [RUNNER, "--cache", str(CACHE), "--runs", str(self.root / "runs")]
                    + (["--interpreters", str(store)] if entry == "main.sh" else []) + [str(parent)],
                    cwd=self.root, text=True, capture_output=True, timeout=90)
                self.assertEqual(result.returncode, 0, result.stderr)
                marker = re.search(r"^RESULT \(exit (\d+)\)\n", result.stdout, re.MULTILINE)
                data = json.loads(result.stdout[marker.end():])
                self.assertEqual(data["status"], "open")
                self.assertEqual([row["id"] for row in data["items"]], ["TAP-1", "TAP-3"])

    def test_a_pin_that_no_longer_matches_is_refused_before_anything_runs(self):
        # A digest that names nothing installed.
        uses = "uses:\n  - {alias: tasks, ref: examples.telara.dev/task-summary-python, version: 0.1.0, digest: %s, effect: read}\n" % ("ab" * 32)
        parent = self.root / "parent-stale"
        parent.mkdir()
        (parent / "main.py").write_text('print(tap.run("tasks", {"status": "open"}))\n')
        (parent / "primitive.yaml").write_text(
            "apiVersion: primitives.telara.dev/v3\nkind: Primitive\n"
            "metadata: {publisher: examples.telara.dev, name: parent-stale, version: 0.1.0}\n"
            "execution: {entrypoint: main.py}\n" + uses)
        result = subprocess.run(
            [RUNNER, "--cache", str(CACHE), "--runs", str(self.root / "runs"), str(parent)],
            cwd=self.root, text=True, capture_output=True, timeout=90)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("not installed here", result.stderr)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--runner", required=True)
    args, remaining = parser.parse_known_args()
    RUNNER = str(Path(args.runner).resolve(strict=True))
    with tempfile.TemporaryDirectory(prefix="tap-language-cache-") as cache:
        CACHE = Path(cache)
        unittest.main(argv=[__file__] + remaining, verbosity=2)
