import json
import re
import sys
import tap


def fail(message):
    raise ValueError(message)


def parse_input(raw):
    if not isinstance(raw, str) or len(raw) > 4096:
        fail("input must be one JSON object under 4096 characters")
    try:
        value = json.loads(raw)
    except Exception:
        fail("input must be valid JSON")
    if not isinstance(value, dict):
        fail("input must be a JSON object")
    jql = value.get("jql")
    maximum = value.get("max_results", 5)
    if not isinstance(jql, str) or not jql.strip() or len(jql) > 500:
        fail("jql must be 1 to 500 characters")
    if "\n" in jql or "\r" in jql or "//" in jql or "/*" in jql:
        fail("jql must be a single-line query without comments")
    if not isinstance(maximum, int) or isinstance(maximum, bool) or maximum < 1 or maximum > 10:
        fail("max_results must be an integer from 1 to 10")
    return jql.strip(), maximum


def object_result(value):
    if isinstance(value, str):
        try:
            value = json.loads(value)
        except Exception:
            fail("Jira returned non-JSON text")
    if not isinstance(value, dict):
        fail("Jira returned an unexpected shape")
    if "issues" not in value or not isinstance(value["issues"], list):
        fail("Jira result did not contain the documented issues array")
    return value


if len(sys.argv) != 2:
    fail('usage: args: ["{\\"jql\\":...,\\"max_results\\":5}"]')
jql, maximum = parse_input(sys.argv[1])
result = object_result(tap.call("search_issues", {
    "params_jql": jql,
    "params_max_results": maximum,
    "params_mode": "summary",
}))
raw_issues = result["issues"]
issues = []
for issue in raw_issues[:maximum]:
    if not isinstance(issue, dict):
        continue
    status = issue.get("status", "")
    if isinstance(status, dict):
        status = status.get("name", "")
    priority = issue.get("priority", "")
    if isinstance(priority, dict):
        priority = priority.get("name", "")
    issues.append({
        "key": str(issue.get("key", ""))[:40],
        "summary": str(issue.get("summary", issue.get("title", "")))[:180],
        "status": str(status)[:60],
        "priority": str(priority)[:40],
    })
print(json.dumps({"returned": len(issues), "issues": issues}, ensure_ascii=False, separators=(",", ":")))
