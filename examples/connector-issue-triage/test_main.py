import ast
import pathlib
import unittest


def load_helpers():
    source = pathlib.Path(__file__).with_name("main.py").read_text()
    tree = ast.parse(source)
    names = {"fail", "parse_input", "object_result"}
    body = [node for node in tree.body if isinstance(node, (ast.Import, ast.ImportFrom)) and not (isinstance(node, ast.Import) and any(alias.name == "tap" for alias in node.names))]
    body += [node for node in tree.body if isinstance(node, ast.FunctionDef) and node.name in names]
    namespace = {}
    exec(compile(ast.Module(body=body, type_ignores=[]), "main.py", "exec"), namespace)
    return namespace


class IssueTriageTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.helpers = load_helpers()

    def test_bounds_query(self):
        parse = self.helpers["parse_input"]
        self.assertEqual(parse('{"jql":"key = TENG-3259","max_results":1}'), ("key = TENG-3259", 1))
        with self.assertRaisesRegex(ValueError, "max_results"):
            parse('{"jql":"project = TENG","max_results":11}')

    def test_missing_or_wrapped_error_issues_are_not_empty_success(self):
        result = self.helpers["object_result"]
        with self.assertRaisesRegex(ValueError, "issues array"):
            result({"error": "permission denied"})
        with self.assertRaisesRegex(ValueError, "issues array"):
            result('{"issues":"permission denied"}')
        self.assertEqual(result('{"issues":[]}')["issues"], [])


if __name__ == "__main__":
    unittest.main()
