import json
import re
import sys
import tap


def fail(message):
    raise ValueError(message)


def parse_input(raw):
    if not isinstance(raw, str) or len(raw) > 8192:
        fail("input must be one JSON object under 8192 characters")
    try:
        value = json.loads(raw)
    except Exception:
        fail("input must be valid JSON")
    if not isinstance(value, dict):
        fail("input must be a JSON object")
    to, subject, body = value.get("to"), value.get("subject"), value.get("body")
    if not isinstance(to, str) or len(to) > 254 or not re.fullmatch(r"[^\s@]+@[^\s@]+\.[^\s@]+", to):
        fail("to must contain one email address")
    if not isinstance(subject, str) or not subject.strip() or len(subject) > 160 or "\n" in subject or "\r" in subject:
        fail("subject must be 1 to 160 characters on one line")
    if not isinstance(body, str) or not body.strip() or len(body) > 4000:
        fail("body must be 1 to 4000 characters")
    thread_id = value.get("thread_id")
    if thread_id is not None and (not isinstance(thread_id, str) or not thread_id.strip() or len(thread_id) > 200):
        fail("thread_id must be a non-empty string up to 200 characters when supplied")
    return {"to": to, "subject": subject.strip(), "body": body, **({"thread_id": thread_id} if thread_id else {})}


def as_object(value):
    if isinstance(value, str):
        try:
            value = json.loads(value)
        except Exception:
            fail("Gmail returned non-JSON text")
    if not isinstance(value, dict):
        fail("Gmail returned an unexpected shape")
    return value


def draft_id(value):
    result = as_object(value)
    draft = result.get("draft", result)
    if not isinstance(draft, dict) or not isinstance(draft.get("id"), str) or not draft["id"]:
        fail("Gmail did not return a draft id; no successful draft is claimed")
    return draft["id"]


if len(sys.argv) != 2:
    fail('usage: args: ["{\\"to\\":...,\\"subject\\":...,\\"body\\":...}"]')
message = parse_input(sys.argv[1])
# The current Telara action schema exposes `message` as a string. Pass the
# documented structured message encoded as JSON; TAP adapts params_message to
# the pinned gmail_create_draft action and gates it as one write.
result = tap.call("create_draft", {"params_message": json.dumps(message, separators=(",", ":"))})
print(json.dumps({"draft_id": draft_id(result)[:200], "status": "unsent_draft_created"}, separators=(",", ":")))
