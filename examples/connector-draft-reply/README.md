# Create an unsent Gmail draft

Create exactly one reviewable draft from caller-supplied recipient, subject, and body. There is no send capability in this manifest. The primitive declares the create-draft action as `write`; a governed interactive client asks for approval before dispatch. A non-interactive invocation without an approval grant refuses the write.

## Run

```sh
tap --client codex examples/connector-draft-reply '{"to":"recipient@example.com","subject":"Following up","body":"Thanks for the conversation."}'
```

Prerequisites: Codex must have a Telara MCP server configured under the exact alias `telara`, with the Google Workspace Gmail create-draft action and an account authorized to create drafts. Invoke it through an interactive client that shows TAP's per-write approval and review the exact recipient and content before approving. A direct non-interactive run without an approval grant refuses the write; `--approve` bypasses the prompt, so review the input first if using that mode. The example deliberately has no test recipient and should not be run with the sample `.invalid` address.

This package has been schema-checked against the current action descriptor: the action requires a string `message`; the program sends a JSON-encoded structured message. Creating a draft does not deliver it. The runtime only prints success if Gmail returns a draft id; it never retries an uncertain write or claims a draft after an error.
