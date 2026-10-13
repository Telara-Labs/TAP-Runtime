import json
from datetime import datetime, timezone

r = tap.exec("kubectl", ["get", "deploy", "-n", "telara-agents", "--context", "minikube", "-o", "json"])
items = json.loads(r["stdout"])["items"]
now = datetime.now(timezone.utc)
for d in items[:5]:
    created = datetime.fromisoformat(d["metadata"]["creationTimestamp"].replace("Z", "+00:00"))
    print(d["metadata"]["name"], f'{d["status"].get("readyReplicas", 0)}/{d["spec"]["replicas"]}', f"age={(now - created).days}d")

for cmd, args in [("curl", ["-s", "https://example.com"]), ("kubectl", ["delete", "pod", "x", "--context", "minikube"])]:
    try:
        tap.exec(cmd, args)
        print(cmd, "->", "RAN")
    except PermissionError as e:
        print(cmd, "->", e)
