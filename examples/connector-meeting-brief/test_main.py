import ast
import pathlib
import unittest


def load_helpers():
    source = pathlib.Path(__file__).with_name("main.py").read_text()
    tree = ast.parse(source)
    names = {"fail", "parse_input", "obj_result", "required_list"}
    body = [node for node in tree.body if isinstance(node, (ast.Import, ast.ImportFrom)) and not (isinstance(node, ast.Import) and any(alias.name == "tap" for alias in node.names))]
    body += [node for node in tree.body if isinstance(node, ast.FunctionDef) and node.name in names]
    namespace = {}
    exec(compile(ast.Module(body=body, type_ignores=[]), "main.py", "exec"), namespace)
    return namespace


class MeetingBriefTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.helpers = load_helpers()

    def test_rejects_unbounded_or_timezone_free_windows(self):
        parse = self.helpers["parse_input"]
        with self.assertRaisesRegex(ValueError, "UTC offset"):
            parse('{"time_min":"2026-10-08T09:00:00","time_max":"2026-10-08T10:00:00","gmail_query":"subject:planning"}')

    def test_missing_or_malformed_connector_arrays_are_errors(self):
        required = self.helpers["required_list"]
        with self.assertRaisesRegex(ValueError, "items array"):
            required({}, "items", "Calendar")
        with self.assertRaisesRegex(ValueError, "messages array"):
            required({"messages": "error"}, "messages", "Gmail")
        self.assertEqual(required({"items": []}, "items", "Calendar"), [])


if __name__ == "__main__":
    unittest.main()
