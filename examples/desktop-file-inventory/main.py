import json
import sys


def inputs():
    if len(sys.argv) != 2:
        raise ValueError('expected one JSON object: {"directory":"/absolute/path"}')
    value = json.loads(sys.argv[1])
    if not isinstance(value, dict) or set(value) != {"directory"}:
        raise ValueError("input must contain only directory")
    directory = value["directory"]
    if not isinstance(directory, str) or not directory.startswith("/") or "\x00" in directory or "\n" in directory:
        raise ValueError("directory must be an absolute path without NUL or newline")
    if directory == "/":
        raise ValueError("refusing to inventory the filesystem root")
    return directory


directory = inputs()
listed = tap.exec("find", [directory, "-maxdepth", "1", "-type", "f", "-print0"])
if listed["exit"] != 0:
    raise RuntimeError("find failed: " + listed["stderr"][:300])
paths = [p for p in listed["stdout"].split("\x00") if p]
if len(paths) > 100:
    raise ValueError("directory has more than 100 immediate files; choose a narrower folder")
files = []
for path in paths:
    result = tap.exec("stat", ["-f", "%z|%Sm", "-t", "%Y-%m-%dT%H:%M:%S", path])
    if result["exit"] != 0:
        files.append({"path": path, "error": result["stderr"][:200]})
        continue
    fields = result["stdout"].strip().split("|", 1)
    if len(fields) != 2:
        raise RuntimeError("stat returned an unexpected result")
    files.append({"path": path, "bytes": int(fields[0]), "modified": fields[1]})
print(json.dumps({"directory": directory, "files": files}, ensure_ascii=True))
