import json
import sys


def fail(message):
    raise ValueError(message)


if len(sys.argv) != 2:
    fail('usage: args: ["{\\"owner\\":\\"cli\\",\\"repo\\":\\"cli\\",\\"ref\\":\\"trunk\\"}"]')
payload = json.loads(sys.argv[1])
if not isinstance(payload, dict) or not all(isinstance(payload.get(k), str) and payload[k] for k in ("owner", "repo", "ref")):
    fail("owner, repo and ref must be non-empty strings")

# Both children are asked at once. Each runs under its own manifest: the
# release brief may reach GitHub's API and the task summary may read the task
# file, and this primitive declares neither.
brief, tasks = tap.run_many([
    ("release_brief", {"owner": payload["owner"], "repo": payload["repo"], "ref": payload["ref"]}),
    ("open_tasks", {"status": "open"}),
])
for name, answer in (("release_brief", brief), ("open_tasks", tasks)):
    if isinstance(answer, Exception):
        fail(name + " did not return: " + str(answer))

reasons = []
checks = brief["checks"]
if checks.get("state") != "sampled":
    reasons.append("no CI verdict is available for " + payload["ref"])
else:
    counts = checks["counts_in_sample"]
    if counts["failure"]:
        reasons.append(str(counts["failure"]) + " check run(s) failed")
    if counts["pending"]:
        reasons.append(str(counts["pending"]) + " check run(s) are still pending")
    if checks.get("sample_truncated") or checks.get("api_results_truncated"):
        reasons.append("only part of the check runs was read")
if tasks["count"]:
    reasons.append(str(tasks["count"]) + " task(s) are still open")

print(json.dumps({
    "repository": brief["repository"],
    "ref": brief["ref"],
    "ready": not reasons,
    "reasons": reasons,
    "open_tasks": tasks["count"],
    "release": brief["release"],
    "evidence": brief["evidence"],
}, ensure_ascii=False, separators=(",", ":")))
