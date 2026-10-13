# Four searches sent together. Prints counts only, never mail content.
alias = "threads" if "threads" in tap.tools() else "emails"
size = "pageSize" if alias == "threads" else "max_results"
windows = ["1d", "3d", "7d", "14d"]
results = tap.call_many([(alias, {"query": "in:inbox newer_than:" + w, size: 10}) for w in windows])
for w, r in zip(windows, results):
    n = len(r.get(alias, [])) if isinstance(r, dict) else repr(r)
    print("newer_than:" + w, "->", n, "on the first page")
