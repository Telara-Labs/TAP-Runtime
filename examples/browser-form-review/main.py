import json
import sys
from urllib.parse import urlsplit


def inputs():
    if len(sys.argv) != 2:
        raise ValueError('expected one JSON object: {"base_url":"http://127.0.0.1:4173","project":"tap","summary":"local demo"}')
    try:
        data = json.loads(sys.argv[1])
    except Exception:
        raise ValueError("input must be valid JSON")
    if not isinstance(data, dict) or set(data) != {"base_url", "project", "summary"}:
        raise ValueError("input must contain exactly base_url, project, and summary")
    for name, limit in (("project", 60), ("summary", 160)):
        value = data[name]
        if not isinstance(value, str) or not value.strip() or len(value) > limit or any(ord(c) < 32 for c in value):
            raise ValueError(name + " must be non-empty, bounded, and contain no control characters")
    value = data["base_url"]
    if not isinstance(value, str) or len(value) > 120:
        raise ValueError("base_url must be a string no longer than 120 characters")
    parsed = urlsplit(value)
    if (parsed.scheme != "http" or parsed.hostname not in ("127.0.0.1", "localhost")
            or parsed.port is None or parsed.port < 1024 or parsed.port > 65535
            or parsed.username or parsed.password or parsed.query or parsed.fragment
            or parsed.path not in ("", "/")):
        raise ValueError("base_url must be an http localhost origin with explicit port and no path, query, or fragment")
    return "http://" + parsed.netloc, data["project"].strip(), data["summary"].strip()


base, project, summary = inputs()
tap.call("navigate", {"url": base + "/form.html"})
page = str(tap.call("snapshot", {}))
if "Local change review" not in page:
    raise RuntimeError("the local form did not render")
tap.call("type", {"target": "#project-name", "element": "project name field", "text": project, "submit": False})
tap.call("type", {"target": "#change-summary", "element": "change summary field", "text": summary, "submit": False})
tap.call("click", {"target": "#review-submit", "element": "local review button"})
result = str(tap.call("snapshot", {}))
expected = "Review ready: " + project + " — " + summary
if expected not in result:
    raise RuntimeError("the fixture did not show the submitted review text")
print(json.dumps({"status": "passed", "review": expected}))
