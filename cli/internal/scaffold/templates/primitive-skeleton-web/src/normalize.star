# normalize.star — merge cached/reasoned extractions, filter, sort, stamp provenance.
# Predeclared params: cached, reasoned, drift, drift_reason, since, cap, page_url. Output via
# `result`. Pure transform: zero builtins, no I/O possible.

def _norm(raw, mode):
    out = []
    for e in raw or []:
        title = e.get("title")
        if title == None or title.strip() == "":
            continue                      # schema requires title; drop silently-empty rows, don't fabricate
        out.append({
            "title": title.strip(),
            "date": e.get("date"),          # REPLACE: validate/normalize date shape as needed
            "url": e.get("url"),
            "extracted_by": mode,
        })
    return out

entries = _norm(cached, "cached_selectors") if not drift else _norm(reasoned, "reasoning_fallback")

if since != None:
    entries = [e for e in entries if e["date"] == None or e["date"] > since]

entries = entries[:cap]

result = {
    "entries": entries,
    "source": {"url": page_url, "checked_entries": len(entries)},
    "extraction": {
        "mode": "reasoning_fallback" if drift else "cached_selectors",
        "drift_detected": drift,
        "drift_reason": drift_reason if drift else None,
        "repair_proposed": drift,        # drift always opens a repair proposal
    },
}
