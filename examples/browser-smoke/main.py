import json
import sys
from urllib.parse import urlsplit


def base_url_from_args():
    if len(sys.argv) != 2:
        raise ValueError('expected one JSON object: {"base_url":"http://127.0.0.1:4173"}')
    try:
        data = json.loads(sys.argv[1])
    except Exception:
        raise ValueError("input must be valid JSON")
    if not isinstance(data, dict) or set(data) != {"base_url"}:
        raise ValueError("input must contain only the required base_url string")
    value = data["base_url"]
    if not isinstance(value, str) or len(value) > 120:
        raise ValueError("base_url must be a string no longer than 120 characters")
    parsed = urlsplit(value)
    if (parsed.scheme != "http" or parsed.hostname not in ("127.0.0.1", "localhost")
            or parsed.port is None or parsed.port < 1024 or parsed.port > 65535
            or parsed.username or parsed.password or parsed.query or parsed.fragment
            or parsed.path not in ("", "/")):
        raise ValueError("base_url must be an http localhost origin with explicit port and no path, query, or fragment")
    return "http://" + parsed.netloc


base = base_url_from_args()
tap.call("navigate", {"url": base + "/"})
page = str(tap.call("snapshot", {}))
if "TAP Browser Smoke" not in page:
    raise RuntimeError("the rendered page did not contain the expected smoke heading")
print(json.dumps({"status": "passed", "title": "TAP Browser Smoke", "heading": "TAP Browser Smoke"}))
