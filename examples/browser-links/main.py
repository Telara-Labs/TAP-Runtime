import json
import sys
from urllib.parse import urlsplit


def base_url():
    if len(sys.argv) != 2:
        raise ValueError('expected one JSON object: {"base_url":"http://127.0.0.1:4173"}')
    try:
        data = json.loads(sys.argv[1])
    except Exception:
        raise ValueError("input must be valid JSON")
    if not isinstance(data, dict) or set(data) != {"base_url"} or not isinstance(data["base_url"], str):
        raise ValueError("input must contain only a string base_url")
    value = data["base_url"]
    if len(value) > 120:
        raise ValueError("base_url is too long")
    parsed = urlsplit(value)
    if (parsed.scheme != "http" or parsed.hostname not in ("127.0.0.1", "localhost")
            or parsed.port is None or parsed.port < 1024 or parsed.port > 65535
            or parsed.username or parsed.password or parsed.query or parsed.fragment
            or parsed.path not in ("", "/")):
        raise ValueError("base_url must be an http localhost origin with explicit port and no path, query, or fragment")
    return "http://" + parsed.netloc


base = base_url()
tap.call("navigate", {"url": base + "/links.html"})
initial = str(tap.call("snapshot", {}))
if "Link checker start" not in initial:
    raise RuntimeError("the source page did not render")
tap.call("click", {"target": "#details-link", "element": "same-origin release checklist link"})
destination = str(tap.call("snapshot", {}))
if "Release checklist" not in destination:
    raise RuntimeError("the link did not open the expected local destination")
print(json.dumps({"status": "passed", "destination": "Release checklist"}))
