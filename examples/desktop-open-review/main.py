import json
import sys

if len(sys.argv) != 2:
    raise ValueError('expected one JSON object: {"folder":"/absolute/path/to/review"}')
value = json.loads(sys.argv[1])
if not isinstance(value, dict) or set(value) != {"folder"}:
    raise ValueError("input must contain only folder")
path = value["folder"]
if not isinstance(path, str) or not path.startswith("/") or "\x00" in path or "\n" in path:
    raise ValueError("folder must be an absolute path without NUL or newline")
if path == "/":
    raise ValueError("refusing to reveal the filesystem root")
result = tap.exec("open", ["-R", path])
if result["exit"] != 0:
    raise RuntimeError("Finder could not reveal the folder: " + result["stderr"][:300])
print(json.dumps({"folder": path, "status": "revealed"}))
