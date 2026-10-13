import json
import re
import sys
from datetime import datetime, timezone

namespace = "default"
if len(sys.argv) > 2:
    raise ValueError("expected at most one JSON input with a namespace")
if len(sys.argv) == 2:
    value = json.loads(sys.argv[1])
    if not isinstance(value, dict) or set(value) != {"namespace"}:
        raise ValueError("input must contain only namespace")
    namespace = value["namespace"]
if not isinstance(namespace, str) or not re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", namespace):
    raise ValueError("namespace must be a valid Kubernetes namespace")

r = tap.exec("kubectl", ["get", "deploy", "-n", namespace, "--context", "minikube", "-o", "json"])
items = json.loads(r["stdout"])["items"]
now = datetime.now(timezone.utc)
for d in items[:5]:
    created = datetime.fromisoformat(d["metadata"]["creationTimestamp"].replace("Z", "+00:00"))
    print(d["metadata"]["name"], f'{d["status"].get("readyReplicas", 0)}/{d["spec"]["replicas"]}', f"age={(now - created).days}d")

for cmd, args in [("curl", ["-s", "https://example.com"]), ("kubectl", ["delete", "pod", "x", "-n", namespace, "--context", "minikube"])]:
    try:
        tap.exec(cmd, args)
        print(cmd, "->", "RAN")
    except PermissionError as e:
        print(cmd, "->", e)
