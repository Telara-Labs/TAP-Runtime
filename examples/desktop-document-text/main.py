import json
import os.path
import sys

ALLOWED = {".rtf", ".doc", ".docx"}
if len(sys.argv) != 2:
    raise ValueError('expected one JSON object: {"document":"/absolute/path/file.docx"}')
value = json.loads(sys.argv[1])
if not isinstance(value, dict) or set(value) != {"document"}:
    raise ValueError("input must contain only document")
path = value["document"]
if not isinstance(path, str) or not path.startswith("/") or "\x00" in path or "\n" in path:
    raise ValueError("document must be an absolute path without NUL or newline")
if os.path.splitext(path)[1].lower() not in ALLOWED:
    raise ValueError("supported formats: rtf, doc, docx")
result = tap.exec("textutil", ["-convert", "txt", "-stdout", path])
if result["exit"] != 0:
    raise RuntimeError("textutil could not extract text: " + result["stderr"][:300])
print(json.dumps({"document": path, "text": result["stdout"]}))
