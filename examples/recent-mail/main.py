# Counts recent inbox items. Prints numbers only, never mail content.
bound = tap.tools()
if "threads" in bound:
    r = tap.call("threads", {"query": "in:inbox newer_than:3d", "pageSize": 25})
    items = r.get("threads", []) if isinstance(r, dict) else []
    print("bound: threads   unit: thread   in last 3 days (first page):", len(items))
elif "emails" in bound:
    r = tap.call("emails", {"query": "in:inbox newer_than:3d", "max_results": 25})
    items = r.get("emails", []) if isinstance(r, dict) else []
    print("bound: emails    unit: message  in last 3 days (first page):", len(items))
else:
    print("no Gmail search tool is bound on this client")
print("response keys:", sorted(r.keys()) if isinstance(r, dict) else type(r).__name__)
