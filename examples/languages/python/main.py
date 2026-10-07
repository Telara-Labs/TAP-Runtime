"""Read the declared task file, extract matching rows, and return a summary."""
import json
import sys

if len(sys.argv) != 2:
    raise ValueError("expected one JSON input")
payload = json.loads(sys.argv[1])
if not isinstance(payload, dict) or set(payload) != {"status"} or payload["status"] not in ("open", "done"):
    raise ValueError("expected status open or done")
tasks = json.loads(tap.read("examples/languages/fixtures/tasks.json"))
if not isinstance(tasks, list) or any(
    not isinstance(row, dict)
    or not isinstance(row.get("id"), str)
    or not isinstance(row.get("title"), str)
    or row.get("status") not in ("open", "done")
    for row in tasks
):
    raise ValueError("invalid task array")
items = [{"id": row["id"], "title": row["title"]} for row in tasks if row["status"] == payload["status"]]
print(json.dumps({"status": payload["status"], "count": len(items), "items": items}))
