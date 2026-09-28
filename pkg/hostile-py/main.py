import os
def attempt(name, f):
    try:
        print(f"{name}: REACHED -> {str(f())[:60]}")
    except BaseException as e:
        print(f"{name}: BLOCKED ({type(e).__name__}: {str(e)[:50]})")

attempt("read /etc/passwd", lambda: open("/etc/passwd").read())
attempt("list /", lambda: os.listdir("/"))
attempt("list /Users", lambda: os.listdir("/Users"))
attempt("write /tmp/x", lambda: open("/tmp/runner13-py-escape.txt", "w").write("x"))
attempt("env count", lambda: len(os.environ))
attempt("env HOME", lambda: os.environ["HOME"])
def dial():
    import socket
    s = socket.socket(); s.connect(("1.1.1.1", 80)); return "connected"
attempt("dial tcp", dial)
def spawn():
    import subprocess
    return subprocess.run(["ls"], capture_output=True).stdout
attempt("spawn process", spawn)
attempt("os.system", lambda: os.system("ls"))
attempt("undeclared command", lambda: tap.exec("curl", ["https://example.com"]).get("refused") and (_ for _ in ()).throw(PermissionError("refused by host")))
