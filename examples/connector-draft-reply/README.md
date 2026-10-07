# Create an unsent Gmail draft

Create exactly one reviewable draft from caller-supplied recipient, subject, and body. There is no send capability in this manifest. The primitive declares the create-draft action as `write`; a governed interactive client asks for approval before dispatch. A non-interactive invocation without an approval grant refuses the write.

## Run

```sh
tap --client codex examples/connector-draft-reply '{"to":"recipient@example.com","subject":"Following up","body":"Thanks for the conversation."}'
```

Prerequisites: the client must have a Gmail connection authorized to create drafts. The manifest declares `gmail.drafts.create` without a server pin. TAP binds a compatible direct tool or discovers the operation through the connected Telara gateway and wraps the arguments for its dispatcher. Invoke it through an interactive client that shows TAP's per-write approval and review the exact recipient and content before approving. A direct non-interactive run without an approval grant refuses the write; `--approve` bypasses the prompt, so review the input first if using that mode. The sample `.invalid` recipient is for input validation and refusal checks, not an approved account write.

This package has been schema-checked against the current action descriptor: the action requires a string `message`; the program sends a JSON-encoded structured message. Creating a draft does not deliver it. The runtime only prints success if Gmail returns a draft id; it never retries an uncertain write or claims a draft after an error.
