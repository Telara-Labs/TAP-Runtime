# Jira issue triage

Run one caller-supplied JQL query and print at most ten compact issue summaries for a human to review. This example is read-only: it does not create, transition, comment on, or edit Jira issues.

## Run

```sh
tap --client codex examples/connector-issue-triage '{"jql":"key = TENG-3259","max_results":5}'
```

Prerequisites: Codex must have a Telara MCP server configured under the exact alias `telara`, with the Jira `search_issues` action and access to the requested project. The current action schema requires `jql`; `max_results` and `mode` are supported. TAP pins the action specifically and checks its current catalog effect before dispatch. The primitive limits one call, ten rows, and field lengths. Search results are hints for review, not duplicate verdicts or resolution recommendations.
