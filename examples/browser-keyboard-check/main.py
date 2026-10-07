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


tap.call("navigate", {"url": base_url() + "/keyboard.html"})
tap.call("click", {"target": "#keyboard-start", "element": "focus point above keyboard control"})
tap.call("press", {"key": "Tab"})
tap.call("press", {"key": " "})
tap.call("press", {"key": "Tab"})
result = str(tap.call("snapshot", {}))
if "Keys: Tab, Space, Tab" not in result or 'checkbox "Confirm keyboard test" [checked]' not in result:
    raise RuntimeError("Space and Tab did not produce the expected keyboard state; snapshot was: " + result)
print(json.dumps({"status": "passed", "keys": "Tab, Space, Tab", "checked": True}))
