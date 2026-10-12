"""Build the Rust primitive as a WASI Preview 2 component.

Edit compiler.json to name the installed rustc or its absolute path. The
build runs with HOME set to a temporary directory, so a rustup proxy may not
find its toolchain; name the toolchain's own rustc (`rustup which rustc`).
This script passes fixed arguments directly to the compiler without a shell.
"""
import json
import subprocess
from pathlib import Path

root = Path(__file__).resolve().parent
config = json.loads((root / "compiler.json").read_text(encoding="utf-8"))
if not isinstance(config, dict) or set(config) != {"rustc"}:
    raise SystemExit("compiler.json must contain only the rustc field")
rustc = config["rustc"]
if not isinstance(rustc, str) or not rustc:
    raise SystemExit("rustc must be a non-empty program name or absolute path")
command = [rustc, "--edition", "2021", "--target", "wasm32-wasip2", "-C", "opt-level=s",
           "-C", "debuginfo=0", "-C", "strip=symbols", "-o", "main.wasm", "src/main.rs"]
subprocess.run(command, cwd=root, check=True)
artifact = (root / "main.wasm").read_bytes()
if not artifact.startswith(b"\0asm\x0d\0\x01\0"):
    raise SystemExit("rustc did not produce a WebAssembly component")
