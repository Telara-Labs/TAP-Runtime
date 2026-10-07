"""Pure Python unit tests for the desktop TAP examples.

These tests run each source file with runpy and a fake tap.exec broker. They
exercise input checks and command/result handling; they do not invoke macOS
programs and are not native runtime proof.
Run from the repository root with:
    python3 -m unittest discover -s examples/desktop-support -v
"""

import contextlib
import io
import json
import runpy
import sys
import unittest
from pathlib import Path
from unittest.mock import patch


EXAMPLES = Path(__file__).resolve().parents[1]


class FakeTap:
    def __init__(self, responses):
        self.responses = list(responses)
        self.calls = []

    def exec(self, command, args):
        self.calls.append((command, list(args)))
        if not self.responses:
            raise AssertionError(f"unexpected command: {command} {args!r}")
        expected_command, response = self.responses.pop(0)
        if command != expected_command:
            raise AssertionError(f"expected {expected_command}, got {command}")
        return response


def result(exit_code=0, stdout="", stderr=""):
    return {"exit": exit_code, "stdout": stdout, "stderr": stderr}


def run_example(name, payload, broker):
    output = io.StringIO()
    source = EXAMPLES / name / "main.py"
    with patch.object(sys, "argv", [str(source), json.dumps(payload)]):
        with contextlib.redirect_stdout(output):
            runpy.run_path(str(source), init_globals={"tap": broker})
    return json.loads(output.getvalue())


class DesktopExamplesTest(unittest.TestCase):
    def test_inventory_returns_metadata_for_immediate_paths(self):
        broker = FakeTap([
            ("find", result(stdout="/tmp/demo/a.txt\0/tmp/demo/b.txt\0")),
            ("stat", result(stdout="3|2026-10-07T11:00:00\n")),
            ("stat", result(stdout="7|2026-10-07T11:01:00\n")),
        ])
        output = run_example("desktop-file-inventory", {"directory": "/tmp/demo"}, broker)
        self.assertEqual(output["directory"], "/tmp/demo")
        self.assertEqual(output["files"], [
            {"path": "/tmp/demo/a.txt", "bytes": 3, "modified": "2026-10-07T11:00:00"},
            {"path": "/tmp/demo/b.txt", "bytes": 7, "modified": "2026-10-07T11:01:00"},
        ])
        self.assertEqual([call[0] for call in broker.calls], ["find", "stat", "stat"])
        self.assertEqual(broker.calls[0][1][1:], ["-maxdepth", "1", "-type", "f", "-print0"])
        self.assertTrue(all(call[1][-1].startswith("/") for call in broker.calls[1:]))
        self.assertEqual(broker.responses, [])

    def test_image_inspector_reports_native_metadata(self):
        broker = FakeTap([("sips", result(stdout="/tmp/demo.png\n  pixelWidth: 20\n  pixelHeight: 10\n  format: png\n"))])
        output = run_example("desktop-image-inspect", {"image": "/tmp/demo.png"}, broker)
        self.assertIn("pixelWidth: 20", output["metadata"])
        self.assertEqual(broker.calls, [("sips", ["-g", "pixelWidth", "-g", "pixelHeight", "-g", "format", "/tmp/demo.png"])])

    def test_document_extractor_returns_stdout_without_output_path(self):
        broker = FakeTap([("textutil", result(stdout="Reviewed text\n"))])
        output = run_example("desktop-document-text", {"document": "/tmp/notes.docx"}, broker)
        self.assertEqual(output, {"document": "/tmp/notes.docx", "text": "Reviewed text\n"})
        self.assertEqual(broker.calls, [("textutil", ["-convert", "txt", "-stdout", "/tmp/notes.docx"])])

    def test_open_review_calls_only_approved_reveal_command(self):
        broker = FakeTap([("open", result())])
        output = run_example("desktop-open-review", {"folder": "/tmp/review"}, broker)
        self.assertEqual(output, {"folder": "/tmp/review", "status": "revealed"})
        self.assertEqual(broker.calls, [("open", ["-R", "/tmp/review"])])

    def test_every_example_rejects_missing_wrong_and_unknown_inputs_before_commands(self):
        cases = [
            ("desktop-file-inventory", "directory", "/tmp/demo"),
            ("desktop-image-inspect", "image", "/tmp/demo.png"),
            ("desktop-document-text", "document", "/tmp/demo.rtf"),
            ("desktop-open-review", "folder", "/tmp/demo"),
        ]
        for name, field, valid in cases:
            for payload in ({}, {field: 12}, {field: valid, "unexpected": True}):
                with self.subTest(example=name, payload=payload):
                    broker = FakeTap([])
                    with self.assertRaises((ValueError, TypeError, json.JSONDecodeError)):
                        run_example(name, payload, broker)
                    self.assertEqual(broker.calls, [])

    def test_each_example_handles_a_nonzero_host_exit(self):
        cases = [
            ("desktop-file-inventory", {"directory": "/tmp/demo"}, [("find", result(1, stderr="unreadable"))]),
            ("desktop-image-inspect", {"image": "/tmp/demo.png"}, [("sips", result(1, stderr="decode failed"))]),
            ("desktop-document-text", {"document": "/tmp/demo.rtf"}, [("textutil", result(1, stderr="conversion failed"))]),
            ("desktop-open-review", {"folder": "/tmp/demo"}, [("open", result(1, stderr="Finder unavailable"))]),
        ]
        for name, payload, responses in cases:
            with self.subTest(example=name):
                broker = FakeTap(responses)
                with self.assertRaisesRegex(RuntimeError, "failed|could not"):
                    run_example(name, payload, broker)
                self.assertEqual(len(broker.calls), 1)

    def test_inventory_preserves_per_file_stat_errors(self):
        broker = FakeTap([
            ("find", result(stdout="/tmp/demo/a.txt\0")),
            ("stat", result(1, stderr="permission denied")),
        ])
        output = run_example("desktop-file-inventory", {"directory": "/tmp/demo"}, broker)
        self.assertEqual(output["files"], [{"path": "/tmp/demo/a.txt", "error": "permission denied"}])

    def test_inventory_rejects_more_than_100_files(self):
        paths = "".join(f"/tmp/demo/{i}\0" for i in range(101))
        broker = FakeTap([("find", result(stdout=paths))])
        with self.assertRaisesRegex(ValueError, "more than 100"):
            run_example("desktop-file-inventory", {"directory": "/tmp/demo"}, broker)
        self.assertEqual(len(broker.calls), 1)

    def test_image_and_document_reject_unsupported_extensions(self):
        cases = [
            ("desktop-image-inspect", {"image": "/tmp/demo.txt"}, "supported extensions"),
            ("desktop-document-text", {"document": "/tmp/demo.pdf"}, "supported formats"),
        ]
        for name, payload, message in cases:
            with self.subTest(example=name):
                broker = FakeTap([])
                with self.assertRaisesRegex(ValueError, message):
                    run_example(name, payload, broker)
                self.assertEqual(broker.calls, [])

    def test_open_review_propagates_runner_permission_refusal(self):
        class RefusingTap:
            calls = []

            def exec(self, command, args):
                self.calls.append((command, list(args)))
                raise PermissionError("write command needs approval")

        broker = RefusingTap()
        with self.assertRaisesRegex(PermissionError, "needs approval"):
            run_example("desktop-open-review", {"folder": "/tmp/review"}, broker)
        self.assertEqual(broker.calls, [("open", ["-R", "/tmp/review"])])


if __name__ == "__main__":
    unittest.main()
