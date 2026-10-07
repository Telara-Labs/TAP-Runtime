# Calendar and email meeting brief

List up to five calendar events in a supplied RFC3339 window and count up to five Gmail message matches. It prints event title/time/id and a count; it never prints message content or attendee addresses.

## Run

```sh
tap --client codex examples/connector-meeting-brief '{"time_min":"2026-10-08T09:00:00-04:00","time_max":"2026-10-08T17:00:00-04:00","gmail_query":"newer_than:14d subject:planning"}'
```

Prerequisites: Codex must have a Telara MCP server configured under the exact alias `telara`, with the Google Workspace Calendar list-events and Gmail search-messages actions available. The action-specific pins route through TAP's Telara dispatcher and verify each read against the live action catalog. The account must already have the relevant Calendar and Gmail access. The user should choose the time window and Gmail query; the example does not infer a timezone.

The current action schemas accept RFC3339 `time_min`/`time_max`, `max_results`, and required Gmail `q`. Calendar returns events in `items`; Gmail returns them in `messages`. Guest code also enforces an offset, a positive window of at most seven days, two dispatches, five results per action, and bounded printed fields. Connector availability and authorization remain account-specific.
