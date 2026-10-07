"""Input validation tests for the standalone browser primitives.

Only validator function definitions are loaded from each primitive's AST.
These tests never replace or imitate the Playwright MCP transport; real browser
coverage is exercised by the documented local integration run.
"""

import ast
import json
import sys
import unittest
from pathlib import Path
from urllib.parse import urlsplit


ROOT = Path(__file__).resolve().parents[2]
EXAMPLES = ROOT / "examples"


def load_validator(package, function):
    source_path = EXAMPLES / package / "main.py"
    tree = ast.parse(source_path.read_text(encoding="utf-8"), filename=str(source_path))
    definition = next(
        node for node in tree.body
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name == function
    )
    module = ast.Module(body=[definition], type_ignores=[])
    namespace = {"json": json, "sys": sys, "urlsplit": urlsplit}
    exec(compile(module, str(source_path), "exec"), namespace)
    return namespace[function]


BASE_URL_VALIDATORS = (
    ("browser-smoke", "base_url_from_args"),
    ("browser-links", "base_url"),
    ("browser-keyboard-check", "base_url"),
)


class BrowserInputValidationTests(unittest.TestCase):
    def setUp(self):
        self.original_argv = sys.argv

    def tearDown(self):
        sys.argv = self.original_argv

    def call_base_url(self, package, function, value):
        sys.argv = ["main.py", json.dumps({"base_url": value})]
        return load_validator(package, function)()

    def test_accepts_explicit_local_origins(self):
        for package, function in BASE_URL_VALIDATORS:
            with self.subTest(package=package):
                self.assertEqual(self.call_base_url(package, function, "http://127.0.0.1:4173"), "http://127.0.0.1:4173")
                self.assertEqual(self.call_base_url(package, function, "http://localhost:4173/"), "http://localhost:4173")

    def test_rejects_nonlocal_origins_and_unsafe_url_parts(self):
        invalid = (
            "https://example.com:4173",
            "http://example.com:4173",
            "http://localhost.evil:4173",
            "http://user@127.0.0.1:4173",
            "http://127.0.0.1:4173/path",
            "http://127.0.0.1:4173/?q=1",
            "http://127.0.0.1:4173/#fragment",
            "http://127.0.0.1",
            "http://127.0.0.1:80",
            "http://127.0.0.1:65536",
        )
        for package, function in BASE_URL_VALIDATORS:
            for value in invalid:
                with self.subTest(package=package, value=value):
                    with self.assertRaises(ValueError):
                        self.call_base_url(package, function, value)

    def test_rejects_oversized_and_malformed_base_url_inputs(self):
        for package, function in BASE_URL_VALIDATORS:
            with self.subTest(package=package, case="oversized"):
                with self.assertRaises(ValueError):
                    self.call_base_url(package, function, "http://127.0.0.1:" + "4" * 130)
            with self.subTest(package=package, case="extra-field"):
                sys.argv = ["main.py", json.dumps({"base_url": "http://127.0.0.1:4173", "url": "https://example.com"})]
                with self.assertRaises(ValueError):
                    load_validator(package, function)()
            with self.subTest(package=package, case="malformed-json"):
                sys.argv = ["main.py", "{not json"]
                with self.assertRaises(ValueError):
                    load_validator(package, function)()

    def call_form(self, project, summary, base="http://127.0.0.1:4173"):
        sys.argv = ["main.py", json.dumps({"base_url": base, "project": project, "summary": summary})]
        return load_validator("browser-form-review", "inputs")()

    def test_accepts_bounded_form_values(self):
        self.assertEqual(self.call_form("tap", "review the local demo form"), ("http://127.0.0.1:4173", "tap", "review the local demo form"))
        self.assertEqual(self.call_form("p" * 60, "s" * 160)[1:], ("p" * 60, "s" * 160))

    def test_rejects_missing_empty_long_and_control_character_form_values(self):
        invalid_cases = (
            ({"base_url": "http://127.0.0.1:4173", "project": "", "summary": "ok"}, "empty project"),
            ({"base_url": "http://127.0.0.1:4173", "project": "ok", "summary": "  "}, "blank summary"),
            ({"base_url": "http://127.0.0.1:4173", "project": "p" * 61, "summary": "ok"}, "long project"),
            ({"base_url": "http://127.0.0.1:4173", "project": "ok", "summary": "s" * 161}, "long summary"),
            ({"base_url": "http://127.0.0.1:4173", "project": "ta\np", "summary": "ok"}, "project control char"),
            ({"base_url": "http://127.0.0.1:4173", "project": "ok", "summary": "line\nbreak"}, "summary control char"),
            ({"base_url": "http://127.0.0.1:4173", "project": "ok"}, "missing summary"),
        )
        validator = load_validator("browser-form-review", "inputs")
        for data, name in invalid_cases:
            with self.subTest(case=name):
                sys.argv = ["main.py", json.dumps(data)]
                with self.assertRaises(ValueError):
                    validator()

    def test_form_rejects_nonlocal_origin_and_extra_fields(self):
        for data in (
            {"base_url": "https://example.com", "project": "tap", "summary": "ok"},
            {"base_url": "http://127.0.0.1:4173", "project": "tap", "summary": "ok", "send": True},
        ):
            with self.subTest(data=data):
                sys.argv = ["main.py", json.dumps(data)]
                with self.assertRaises(ValueError):
                    load_validator("browser-form-review", "inputs")()


if __name__ == "__main__":
    unittest.main()
