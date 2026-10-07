# Calendar and email meeting brief

List up to five calendar events in a supplied RFC3339 window and count up to five Gmail message matches. It prints event title/time/id and a count; it never prints message content or attendee addresses.

## Run

```sh
tap --client codex examples/connector-meeting-brief '{"time_min":"2026-10-08T09:00:00-04:00","time_max":"2026-10-08T17:00:00-04:00","gmail_query":"newer_than:14d subject:planning"}'
```

Prerequisites: the client must have authorized Calendar and Gmail connections. The manifest declares `calendar.events.list` and `gmail.messages.search` with parameter/result contracts; it does not pin a gateway or server name. TAP binds compatible direct tools or discovers those operations through the connected Telara gateway's read-only search/describe catalog, then wraps arguments for its dispatcher. Other gateways need a host adapter for their discovery protocol. A tool with different parameter or result semantics does not satisfy this package. The user supplies the time window and Gmail query; the example does not infer a timezone.

The current action schemas accept RFC3339 `time_min`/`time_max`, `max_results`, and required Gmail `q`. Calendar returns events in `items`; Gmail returns them in `messages`. Guest code also enforces an offset, a positive window of at most seven days, two dispatches, five results per action, and bounded printed fields. Connector availability and authorization remain account-specific.
