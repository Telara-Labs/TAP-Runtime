import json
import re
import sys
import tap


def fail(message):
    raise ValueError(message)


def read_object(raw):
    if not isinstance(raw, str) or len(raw) > 2048:
        fail("input must be one JSON object under 2048 characters")
    try:
        value = json.loads(raw)
    except Exception:
        fail("input must be valid JSON")
    if not isinstance(value, dict):
        fail("input must be a JSON object")
    owner, repo, ref = value.get("owner"), value.get("repo"), value.get("ref")
    if not isinstance(owner, str) or not re.fullmatch(r"[A-Za-z0-9-]{1,39}", owner):
        fail("owner must be a GitHub login")
    if not isinstance(repo, str) or not re.fullmatch(r"[A-Za-z0-9_.-]{1,100}", repo):
        fail("repo must be a repository name")
    if not isinstance(ref, str) or not re.fullmatch(r"[A-Za-z0-9._/-]{1,100}", ref) or ".." in ref:
        fail("ref must be a short branch, tag, or commit name")
    return owner, repo, ref


def fetch_json(url, allow_http_error=False):
    response = tap.fetch(url, method="GET", headers={"Accept": "application/vnd.github+json"})
    status = response.get("status")
    body = response.get("body", "")
    if not isinstance(status, int) or status < 200 or status >= 300:
        if allow_http_error and isinstance(status, int):
            return status, None
        fail("GitHub API returned HTTP " + str(status))
    if not isinstance(body, str) or len(body) > 2_000_000:
        fail("GitHub response exceeded the 2 MB example bound")
    try:
        return status, decode_json(body)
    except Exception:
        fail("GitHub API response was not JSON")


def decode_json(body):
    try:
        return json.loads(body)
    except Exception:
        fail("GitHub API response was not JSON")


def validate_release(release):
    if not isinstance(release, dict) or not isinstance(release.get("tag_name"), str) or not release["tag_name"]:
        fail("GitHub latest-release response had an unexpected shape")
    return release


def summarize_checks(status, checks, checks_url):
    if not isinstance(status, int):
        fail("GitHub check-runs response had an invalid HTTP status")
    if status < 200 or status >= 300:
        return {
            "state": "unavailable",
            "http_status": status,
            "missing_evidence": ["No check-run data was returned by GitHub; no CI verdict is available."],
            "url": checks_url,
        }
    if not isinstance(checks, dict) or not isinstance(checks.get("check_runs"), list) or not isinstance(checks.get("total_count"), int) or isinstance(checks.get("total_count"), bool):
        fail("GitHub check-runs response had an unexpected shape")
    if any(not isinstance(run, dict) for run in checks["check_runs"]):
        fail("GitHub check-runs response contained a malformed run")
    runs = checks["check_runs"]
    counts = {"success": 0, "failure": 0, "pending": 0, "other": 0}
    sample = []
    for run in runs[:8]:
        state = run.get("conclusion") if run.get("status") == "completed" and isinstance(run.get("conclusion"), str) else "pending"
        bucket = "success" if state == "success" else "failure" if state in ("failure", "timed_out", "action_required", "cancelled") else "pending" if state in ("pending", "queued", "in_progress", "requested", "waiting") else "other"
        counts[bucket] += 1
        sample.append({"name": str(run.get("name", "unknown"))[:100], "state": state[:40], "url": str(run.get("html_url", ""))[:300]})
    return {
        "state": "sampled",
        "sampled": len(sample),
        "returned_by_api": len(runs),
        "total_count": checks["total_count"],
        "sample_truncated": len(runs) > len(sample),
        "api_results_truncated": checks["total_count"] > len(runs),
        "counts_in_sample": counts,
        "runs": sample,
        "url": checks_url,
    }


if len(sys.argv) != 2:
    fail('usage: args: ["{\\"owner\\":\\"cli\\",\\"repo\\":\\"cli\\",\\"ref\\":\\"trunk\\"}"]')
owner, repo, ref = read_object(sys.argv[1])
base = "https://api.github.com/repos/" + owner + "/" + repo
release_url = base + "/releases/latest"
checks_url = base + "/commits/" + ref + "/check-runs?per_page=100"
_, release = fetch_json(release_url)
release = validate_release(release)
checks_status, checks = fetch_json(checks_url, allow_http_error=True)
checks_summary = summarize_checks(checks_status, checks, checks_url)
out = {
    "repository": owner + "/" + repo,
    "ref": ref,
    "release": {"tag": str(release.get("tag_name", ""))[:100], "name": str(release.get("name", ""))[:140], "published_at": str(release.get("published_at", ""))[:40], "url": str(release.get("html_url", ""))[:300]},
    "checks": checks_summary,
    "evidence": [release_url, checks_url],
}
print(json.dumps(out, ensure_ascii=False, separators=(",", ":")))
