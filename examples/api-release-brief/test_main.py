import ast
import pathlib
import unittest


def load_helpers():
    source = pathlib.Path(__file__).with_name("main.py").read_text()
    tree = ast.parse(source)
    names = {"fail", "read_object", "decode_json", "validate_release", "summarize_checks"}
    body = [node for node in tree.body if isinstance(node, (ast.Import, ast.ImportFrom)) and not (isinstance(node, ast.Import) and any(alias.name == "tap" for alias in node.names))]
    body += [node for node in tree.body if isinstance(node, ast.FunctionDef) and node.name in names]
    namespace = {}
    exec(compile(ast.Module(body=body, type_ignores=[]), "main.py", "exec"), namespace)
    return namespace


class ApiReleaseBriefTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.helpers = load_helpers()

    def test_validates_input_and_rejects_repository_path(self):
        read = self.helpers["read_object"]
        self.assertEqual(read('{"owner":"cli","repo":"cli","ref":"trunk"}'), ("cli", "cli", "trunk"))
        with self.assertRaisesRegex(ValueError, "repo must be a repository name"):
            read('{"owner":"cli","repo":"../cli","ref":"trunk"}')

    def test_requires_latest_release_shape(self):
        validate = self.helpers["validate_release"]
        with self.assertRaisesRegex(ValueError, "latest-release"):
            validate([])
        with self.assertRaisesRegex(ValueError, "latest-release"):
            validate({"tag_name": ""})

    def test_unavailable_check_runs_are_explicit_partial_evidence(self):
        summarize = self.helpers["summarize_checks"]
        result = summarize(500, None, "https://api.github.com/check-runs")
        self.assertEqual(result["state"], "unavailable")
        self.assertEqual(result["http_status"], 500)
        self.assertIn("no CI verdict", result["missing_evidence"][0])
        self.assertNotIn("counts_in_sample", result)

    def test_healthy_check_runs_are_explicitly_sampled_and_bounded(self):
        summarize = self.helpers["summarize_checks"]
        runs = [{"name": str(index), "status": "completed", "conclusion": "success"} for index in range(10)]
        result = summarize(200, {"check_runs": runs, "total_count": 12}, "https://api.github.com/check-runs")
        self.assertEqual(result["state"], "sampled")
        self.assertEqual(result["sampled"], 8)
        self.assertEqual(result["total_count"], 12)
        self.assertTrue(result["sample_truncated"])
        self.assertTrue(result["api_results_truncated"])
        self.assertNotIn("verdict", result)

    def test_invalid_json_and_malformed_success_shape_fail_closed(self):
        decode = self.helpers["decode_json"]
        with self.assertRaisesRegex(ValueError, "not JSON"):
            decode("upstream error page")
        summarize = self.helpers["summarize_checks"]
        with self.assertRaisesRegex(ValueError, "unexpected shape"):
            summarize(200, {"check_runs": []}, "https://api.github.com/check-runs")
        with self.assertRaisesRegex(ValueError, "malformed run"):
            summarize(200, {"check_runs": [None], "total_count": 1}, "https://api.github.com/check-runs")


if __name__ == "__main__":
    unittest.main()
