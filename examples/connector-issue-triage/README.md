# Jira issue triage

Run one caller-supplied JQL query and print at most ten compact issue summaries for a human to review. This example is read-only: it does not create, transition, comment on, or edit Jira issues.

## Run

```sh
tap --client codex examples/connector-issue-triage '{"jql":"key = TENG-3259","max_results":5}'
```

Prerequisites: the client must have Jira access for the requested project. The manifest declares `jira.issues.search` and its argument/result contract without a server pin. TAP binds a compatible direct tool or discovers the operation through a connected Telara gateway, regardless of its server alias. This contract requires `jql`, `max_results`, `mode`, and an `issues` array; a connector with different arguments is refused rather than guessed. The gateway adapter checks the live action effect before dispatch. The primitive limits one call, ten rows, and field lengths. Search results are hints for human review.
