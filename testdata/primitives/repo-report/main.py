import json
r = tap.fetch("https://api.github.com/repos/mvdan/sh")
repo = json.loads(r["body"])
line = f'{repo["full_name"]} licence={repo["license"]["spdx_id"]} status={r["status"]}'
print(line)
for what, f in [("other origin", lambda: tap.fetch("https://example.com/")),
                ("read outside", lambda: tap.read("/etc/hosts")),
                ("write outside", lambda: tap.write("/tmp/tap-escape.txt", "x")),
                ("write inside", lambda: tap.write("out/report.txt", line + "\n"))]:
    try:
        f(); print(what, "-> ran")
    except Exception as e:
        print(what, "->", type(e).__name__ + ":", e)
