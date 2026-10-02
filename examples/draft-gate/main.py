# Attempts a write. Run WITHOUT --approve: nothing must be created.
try:
    tap.call("draft", {"to": ["nobody@example.com"], "subject": "tap gate test", "body": "must never be created"})
    print("DRAFT CREATED - the gate failed")
except PermissionError as e:
    print("gated:", e)
