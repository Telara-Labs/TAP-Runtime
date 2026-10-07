import json
import os.path
import sys

ALLOWED = {".png", ".jpg", ".jpeg", ".tif", ".tiff", ".gif", ".heic"}

if len(sys.argv) != 2:
    raise ValueError('expected one JSON object: {"image":"/absolute/path/image.png"}')
value = json.loads(sys.argv[1])
if not isinstance(value, dict) or set(value) != {"image"}:
    raise ValueError("input must contain only image")
path = value["image"]
if not isinstance(path, str) or not path.startswith("/") or "\x00" in path or "\n" in path:
    raise ValueError("image must be an absolute path without NUL or newline")
if os.path.splitext(path)[1].lower() not in ALLOWED:
    raise ValueError("supported extensions: png, jpg, jpeg, tif, tiff, gif, heic")
result = tap.exec("sips", ["-g", "pixelWidth", "-g", "pixelHeight", "-g", "format", path])
if result["exit"] != 0:
    raise RuntimeError("sips could not inspect the image: " + result["stderr"][:300])
print(json.dumps({"image": path, "metadata": result["stdout"].strip()}))
