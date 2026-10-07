import datetime
import json
import sys
import tap


def fail(message):
    raise ValueError(message)


def parse_input(raw):
    if not isinstance(raw, str) or len(raw) > 2048:
        fail("input must be one JSON object under 2048 characters")
    try:
        value = json.loads(raw)
    except Exception:
        fail("input must be valid JSON")
    if not isinstance(value, dict):
        fail("input must be a JSON object")
    start_raw, end_raw = value.get("time_min"), value.get("time_max")
    query = value.get("gmail_query")
    if not isinstance(start_raw, str) or not isinstance(end_raw, str):
        fail("time_min and time_max must be RFC3339 timestamps")
    if not isinstance(query, str) or not query.strip() or len(query) > 200:
        fail("gmail_query must be 1 to 200 characters")
    try:
        start = datetime.datetime.fromisoformat(start_raw.replace("Z", "+00:00"))
        end = datetime.datetime.fromisoformat(end_raw.replace("Z", "+00:00"))
    except Exception:
        fail("time_min and time_max must be valid RFC3339 timestamps")
    if start.tzinfo is None or end.tzinfo is None:
        fail("time_min and time_max must include a UTC offset")
    span = end - start
    if span.total_seconds() <= 0 or span.total_seconds() > 7 * 24 * 3600:
        fail("time window must be positive and no longer than seven days")
    return start_raw, end_raw, query.strip()


def obj_result(value, label):
    if isinstance(value, str):
        try:
            value = json.loads(value)
        except Exception:
            fail(label + " returned non-JSON text")
    if not isinstance(value, dict):
        fail(label + " returned an unexpected shape")
    return value


def required_list(value, key, label):
    if key not in value or not isinstance(value[key], list):
        fail(label + " response did not contain the documented " + key + " array")
    return value[key]


if len(sys.argv) != 2:
    fail('usage: args: ["{\\"time_min\\":...,\\"time_max\\":...,\\"gmail_query\\":...}"]')
time_min, time_max, query = parse_input(sys.argv[1])
events_result = obj_result(tap.call("calendar", {
    "time_min": time_min,
    "time_max": time_max,
    "max_results": 5,
}), "Calendar")
mail_result = obj_result(tap.call("mail", {
    "q": query,
    "max_results": 5,
}), "Gmail")
raw_events = required_list(events_result, "items", "Calendar")
raw_messages = required_list(mail_result, "messages", "Gmail")
events = []
for event in raw_events[:5]:
    if not isinstance(event, dict):
        continue
    start_value, end_value = event.get("start", ""), event.get("end", "")
    if isinstance(start_value, dict):
        start_value = start_value.get("dateTime", start_value.get("date", ""))
    if isinstance(end_value, dict):
        end_value = end_value.get("dateTime", end_value.get("date", ""))
    events.append({
        "summary": str(event.get("summary", "Untitled event"))[:120],
        "start": str(start_value)[:40],
        "end": str(end_value)[:40],
        "id": str(event.get("id", ""))[:100],
    })
out = {
    "window": {"time_min": time_min, "time_max": time_max},
    "events": events,
    "matching_messages": min(len(raw_messages), 5),
}
print(json.dumps(out, ensure_ascii=False, separators=(",", ":")))
