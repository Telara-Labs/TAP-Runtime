import ast
import pathlib
import unittest


def load_helpers():
    source = pathlib.Path(__file__).with_name("main.py").read_text()
    tree = ast.parse(source)
    names = {"fail", "parse_input", "as_object", "draft_id"}
    body = [node for node in tree.body if isinstance(node, (ast.Import, ast.ImportFrom)) and not (isinstance(node, ast.Import) and any(alias.name == "tap" for alias in node.names))]
    body += [node for node in tree.body if isinstance(node, ast.FunctionDef) and node.name in names]
    namespace = {}
    exec(compile(ast.Module(body=body, type_ignores=[]), "main.py", "exec"), namespace)
    return namespace


class DraftReplyTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.helpers = load_helpers()

    def test_validates_single_recipient_and_bounded_content(self):
        parse = self.helpers["parse_input"]
        self.assertEqual(parse('{"to":"a@example.com","subject":"Hello","body":"Thanks"}')["to"], "a@example.com")
        with self.assertRaisesRegex(ValueError, "one email address"):
            parse('{"to":"a@example.com,b@example.com","subject":"Hello","body":"Thanks"}')

    def test_only_a_real_draft_id_counts_as_success(self):
        get_id = self.helpers["draft_id"]
        self.assertEqual(get_id({"draft": {"id": "draft-1"}}), "draft-1")
        with self.assertRaisesRegex(ValueError, "did not return a draft id"):
            get_id({"error": "write refused"})
        with self.assertRaisesRegex(ValueError, "did not return a draft id"):
            get_id({"draft": {"id": ""}})


if __name__ == "__main__":
    unittest.main()
