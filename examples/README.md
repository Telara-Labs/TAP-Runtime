# Build a tool once, run it again

These packages show procedures you can turn into callable tools with TAP.
Each has a `primitive.yaml`, executable source, a defined JSON input, and a
README with its prerequisites. Read the source and declarations before running.
Change the input to reuse the same package; an agent can discover and invoke a
saved package through TAP's MCP interface.

## Start with an API tool

From the repository root:

```sh
tap manifest check examples/api-release-brief
tap --approve examples/api-release-brief '{"owner":"Telara-Labs","repo":"TAP-Runtime","ref":"main"}'
```

This reads GitHub's public API and returns release evidence. `--approve`
grants the declared effects for that invocation; review the manifest first.
No Telara account is required. GitHub rate limits and unavailable evidence
remain visible in the result. See the package README for all input fields.

## Choose a procedure

| Example | What you can build | Required host |
|---|---|---|
| [Browser smoke check](browser-smoke/) | Open a page and check its rendered heading | Playwright MCP and the local demo site |
| [Browser link check](browser-links/) | Follow a page link and inspect its destination | Playwright MCP and the local demo site |
| [Browser form review](browser-form-review/) | Exercise a disposable form and inspect the rendered result | Playwright MCP and the local demo site |
| [Browser keyboard check](browser-keyboard-check/) | Repeat keyboard interactions and inspect their result | Playwright MCP and the local demo site |
| [Desktop file inventory](desktop-file-inventory/) | List a folder's immediate files with size and modification time | macOS host programs |
| [Desktop image inspection](desktop-image-inspect/) | Inspect local image properties without uploading the image | macOS host programs |
| [Desktop document text](desktop-document-text/) | Convert a local document into text | macOS host programs |
| [Desktop open for review](desktop-open-review/) | Reveal a local file for a person to review | macOS host programs; effect approval |
| [API release brief](api-release-brief/) | Summarize public GitHub release evidence | Declared GitHub API origin |
| [Connector meeting brief](connector-meeting-brief/) | Combine a calendar window and a bounded mail search | The documented connected Calendar and Gmail tools |
| [Connector draft reply](connector-draft-reply/) | Create an email draft for review | The documented connected Gmail tool; effect approval |
| [Connector issue triage](connector-issue-triage/) | Inspect a bounded Jira queue | The documented connected Jira tool |

The browser packages run against a disposable localhost site. They demonstrate
real browser tool calls, including navigation and input, without using your
open browser or a production account. Follow [browser setup](browser-support/)
to run them. Adapt the code, target checks and declarations before applying
the procedure to your own site.

The desktop packages invoke declared programs on macOS. They are useful
examples of local computer automation, not claims of Windows or Linux desktop
support. Follow [desktop setup](desktop-support/) for small disposable files
you can use in the examples. Host commands have their own operating-system access; the program's
path validation is not an operating-system security boundary.

Connector packages declare operations and parameter/result contracts without
pinning a gateway. The host binds a compatible direct tool or discovers an
operation advertised by the connected Telara gateway, then wraps its arguments
for that route. They borrow the host's existing authorized connections.
Installing TAP does not install a connector or
grant account access. A sample input is not a successful live run; package
READMEs distinguish tested behavior from remaining connected-account checks.

## Save your tool for an agent

These gallery folders can run directly. Saving through `discover save` also
requires an `AUTHORING.json` with real source and contract provenance; the
gallery does not supply invented history. Ask your agent to adapt the example
to your recurring task using its installed `tap-author` skill,
then review and test the authored package before saving it:

```sh
tap discover save <your-authored-package> --client detected
```

This saves one executable package and creates supported client pointers.
Then ask your agent the ordinary task. Which tool it selects depends on the
agent and its connected host; registration is separate from successful reuse.

## Small building blocks

[The language showcase](languages/) runs the same useful file-summary tool in
Bash, Python, JavaScript and TypeScript, with the same declared read and JSON
interface. Its support table distinguishes these runnable source languages
from the compiled Go/C++ execution path still being added.

The existing [hello Python](hello-py/), [hello TypeScript](hello-ts/) and
[hello shell](hello-sh/) packages show the smallest executable package.
[Recent mail](recent-mail/) shows optional connector bindings,
[fan-out](fan-out/) shows parallel calls, [draft-gate](draft-gate/) shows a
write gate, and [versioning](versioning/) shows package changes.

For the API and declaration format, see
[writing a primitive](../docs/writing-a-primitive.md). TAP runs a callable
program; exposing that program as an HTTP service requires a separate host.

See [verification](VERIFICATION.md) for the checks actually performed and
commands to repeat the unit suites.
