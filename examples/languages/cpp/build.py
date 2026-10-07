"""Build the C++ primitive with a configured WASI Preview1 compiler.

Edit compiler.json to name the installed compiler or its absolute path. This
script passes fixed arguments directly to the compiler without invoking a shell.
"""
import json
import subprocess
from pathlib import Path

root = Path(__file__).resolve().parent
config = json.loads((root / "compiler.json").read_text(encoding="utf-8"))
if not isinstance(config, dict) or set(config) != {"compiler"}:
    raise SystemExit("compiler.json must contain only the compiler field")
compiler = config["compiler"]
if not isinstance(compiler, str) or not compiler:
    raise SystemExit("compiler must be a non-empty program name or absolute path")
command = [compiler, "-std=c++17", "-fno-exceptions", "-O2", "-Ivendor", "-o", "main.wasm", "main.cpp"]
subprocess.run(command, cwd=root, check=True)
artifact = (root / "main.wasm").read_bytes()
if not artifact.startswith(b"\0asm\x01\0\0\0"):
    raise SystemExit("compiler did not produce a WebAssembly module")
