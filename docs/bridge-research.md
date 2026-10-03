# Bridges: which agents can lend the runner their connections

TENG-3110 (P3r of epic TENG-3107, plan `discover-multi-client-plan.md` §3.3,
decision D3). Researched 2026-10-02.

## The question

A primitive that calls tools runs only where the runner can borrow the
agent's own connections (`host/tools.go` `openBridge`, ruling 13). For each
agent: **can a program make it execute one specific tool call (server, tool,
arguments) and get the result back, through the agent's own connection,
credentials and approval prompt, with no model turn?** Asking the model to
make the call does not count: a run spends zero model tokens.

A hook that can only allow, deny or observe a call is not a bridge. Neither
is an Agent Client Protocol (ACP) server: ACP sessions take prompts, and
prompts are model turns.

## Answer

| Agent | Mechanism | Verdict | Evidence |
|---|---|---|---|
| Claude Code | stream-json control channel | **bridge** (built) | `bridge/claude.go` |
| Codex | app-server protocol | **bridge** (built) | `bridge/codex.go` |
| VS Code (GitHub Copilot Chat) | our extension calls `vscode.lm.invokeTool` | **bridge** (built) | `bridge/vscode.go` |
| Gemini CLI | `AfterTool` hook returns a tail tool call | **bridge** (built, experimental; live-tested on Gemini CLI 0.62.0, TENG-3058: the model's record holds only the primitive's output) | `host/hook.go`, `host/gemini_live_test.go` |
| Antigravity | `PreInvocation` / `PostInvocation` hooks return `injectSteps: [{"toolCall": {"name", "args"}}]`; each hook gets `transcriptPath`, where the injected call's result is written | **candidate: verify live** | Hook contract embedded in `Antigravity.app/Contents/Resources/bin/language_server` ("Lifecycle Hooks (`hooks.json`)", sections 3 and 4) |
| Cursor IDE | none. Hooks (`preToolUse`, `beforeMCPExecution`, …) answer only `permission`, `user_message`, `agent_message`, `updated_input` (rewrites the arguments of a call the model already made), `additional_context`; `stop` can send a `followup_message`, which is a model turn. Its extension host stubs `vscode.lm`: `invokeTool` rejects with "Language model tools are not available" and `tools` is always empty, so our VS Code extension cannot reach Cursor's MCP servers | **no** | `Cursor.app/…/workbench.desktop.main.js` (`agent.v1.PreToolUseRequestResponse`, `agent.v1.PostToolUseRequestResponse`); `…/api/node/extensionHostProcess.js` |
| Cursor CLI (`cursor-agent`) | same hooks as the IDE. `cursor-agent acp` is an ACP server; its `initialize` offers `loadSession`, `mcpCapabilities`, `promptCapabilities`, `sessionCapabilities` and no tool call | **no** | `cursor-agent acp` initialize, run here 2026-10-02 (CLI 2026.08.25) |
| Goose | ACP custom request `_goose/unstable/tools/call` `{sessionId, extensionName, name, arguments}` runs one extension tool through Goose's own extension manager (`dispatch_app_tool_call`) with no model turn and returns `{content, structuredContent, isError}`. It refuses unless the session is in **auto mode**, so Goose shows no approval of its own; the runner's effect gate is the only one. Tools whose `_meta.ui.visibility` excludes `app` are refused. Marked unstable | **built (TENG-3116)**: live-tested on Goose 1.53.0; the runner's gate is the only approval (docs/install.md) | `aaif-goose/goose` `crates/goose-sdk-types/src/custom_requests.rs` (`GooseToolCallRequest`), `crates/goose/src/acp/server/tools.rs` (`on_call_tool`), `crates/goose/src/agents/reply_parts.rs` (`is_tool_visible_to_app`), read 2026-10-02 |
| GitHub Copilot CLI | Hooks: `preToolUse` returns `permissionDecision`, `permissionDecisionReason`, `modifiedArgs`; `postToolUse` returns `modifiedResult`, `additionalContext`; nothing makes a call. `copilot --acp` is plain ACP | **no** | docs.github.com/en/copilot/reference/hooks-configuration (checked 2026-10-02) |
| OpenCode | `opencode serve` routes (from `@opencode-ai/sdk` 1.18.34): `/mcp` and `/mcp/{name}/connect`, `/auth`, `/disconnect` manage servers, `/experimental/tool` lists tools, `/session/{id}/shell` runs a shell command; no route runs a tool. Plugin hooks `tool.execute.before/after` rewrite arguments and output | **no** via documented APIs | SDK route list, read 2026-10-02; opencode.ai/docs/server |
| Windsurf (Devin Desktop) | Cascade hooks `pre_*`/`post_*` (`read_code`, `write_code`, `run_command`, `mcp_tool_use`, …): a pre hook blocks with exit code 2, everything else observes | **no** (vendor docs) | docs.devin.ai/desktop/cascade/hooks |
| Qwen Code | Hooks return `permissionDecision`, `updatedInput`, `additionalContext` (PreToolUse) and `decision`, `reason`, `additionalContext` (PostToolUse); no chained tool call like Gemini CLI's `tailToolCallRequest` | **no** (vendor docs; source not read) | QwenLM/qwen-code `docs/users/features/hooks.md` |
| Roo Code | Exported `RooCodeAPI`: `startNewTask`, `resumeTask`, `sendMessage`, button presses, settings. Every path goes through the model (`use_mcp_tool`) | **no** | RooCodeInc/Roo-Code `packages/types/src/api.ts` |
| Cline (extension and CLI) | Extension exports only `startNewTask`, `sendMessage`, `pressPrimaryButton`, `pressSecondaryButton` (model turns); its `cline.McpService` gRPC methods manage servers and call none. CLI hub commands (`session.*`, `run.*`, `task.*`, `approval.respond`, `capability.*`) have no MCP call; `--acp` has no extension methods; hooks return `cancel`, `review`, `contextModification`, `overrideInput`, `errorMessage` (rewrite or block, never add a call) | **no** | `saoudrizwan.claude-dev-4.1.22` `dist/extension.js`; `cline` 3.0.68 `@cline/core/dist/hub/index.js`, `@cline/core/dist/extensions/mcp/manager.d.ts`, read 2026-10-02 |
| Kilo CLI | `kilo serve` route `POST /experimental/mcp/call-tool` `{server, name, arguments}` (query `directory`) calls the server's own live MCP client and returns `{content, isError, structuredContent}`, with no model turn. 404 unless Kilo's own experimental flag (`KILO_EXPERIMENTAL_MCP_APPS`, or `KILO_EXPERIMENTAL`) is on in the serve process. It skips Kilo's permission prompt and plugin hooks, so as with Goose the runner's gate would be the only approval | **bridge candidate, verified live** (Kilo CLI 7.8.3, fixture tracker: search returned ABC-12/13, ABC-99 came back `isError`); build needs a decision (TENG-3131) | `@kilocode/cli` 7.8.3 binary (`McpHttpApi.callTool`); live probe 2026-10-02 |
| Kilo Code (extension) | Starts its own `kilo serve --port 0` with a random `KILO_SERVER_PASSWORD`, so no outside program knows the port or password; it is not documented to turn the experimental flag on | **no** (from Kilo's docs, extension not installed here) | Kilo-Org/kilocode `packages/kilo-vscode/AGENTS.md` |
| Zed | As an ACP client Zed answers file, terminal, permission and elicitation requests only; external agents get the MCP server configs and connect themselves; the MCP extension API only supplies a launch command. No method runs one of Zed's context-server tools | **no** (source read; Zed not installed here) | zed-industries/zed `crates/agent_servers/src/acp.rs`; zed.dev/docs/extensions/mcp-extensions, read 2026-10-02 |
| Crush | One `PreToolUse` hook returning `decision`, `updated_input`, `context` | **no** (vendor docs) | charmbracelet/crush `docs/hooks/README.md` |
| Amp | `amp tools use [--only output] <tool> --<param> <value>` runs a tool with no model turn; documented only for toolbox tools (`tb__…`), not shown for MCP tools. Plugin hook `tool.call` approves, rejects, rewrites or synthesizes a result for a model call, never adds one. Amp asks no approval by default | **candidate, unconfirmed for MCP tools** (Amp not installed here; it needs a sign-in, TENG-3121) | ampcode.com/news/more-tools-for-the-agent, ampcode.com/docs/customize/mcp, ampcode.com/docs/plugin-api, read 2026-10-02 |
| Continue (extension and `cn` CLI) | `cn serve` routes are `GET /state`, `POST /message` (a model turn), `/permission`, `/pause`, `/diff`, `/exit`; `MCPService.runTool` is reachable only from the model's tool wrapper; the extension exports only `registerCustomContextProvider` | **no** | `@continuedev/cli` 1.5.47 `dist/index.js`; continuedev/continue `extensions/vscode/src/activation/activate.ts`, read 2026-10-02 |
| Aider | No MCP at all (issue 3314, RFC 4506 open) | **no** | Aider-AI/aider#4506 |


"vendor docs" rows rest on the vendor's published documentation, read
through a summarizer; the Cursor, Antigravity, Goose and OpenCode rows were
checked in the shipped binary, app bundle, source or SDK.

## What to build (P3b)

Three agents have a mechanism worth a build ticket, each starting with a live
test in the shape of `bridge/live_test.go`:

1. **Antigravity**: hook-injected tool calls (below). It keeps
   Antigravity's own approval (`PreToolUse` still runs on the injected step).
2. **Goose**: `_goose/unstable/tools/call` over ACP. Simplest protocol of
   all, but it requires auto mode, so a run has no Goose approval prompt and
   relies on the runner's effect gate alone; the method is unstable. Built
   with that approval model (decided 2026-10-02, TENG-3116): `bridge/goose.go`.
   Live facts that differ from the source reading: a tool is named
   `<extension>__<tool>` in both tools/list and tools/call, the session
   lists a connected stdio server as extension type `mcp`, Goose's MCP
   client calls itself `goose-cli` and sends `server/discover` before
   `initialize`.

3. **Kilo CLI**: `kilo serve` `POST /experimental/mcp/call-tool`, verified
   live. Same approval model as Goose (the runner's gate only), and the serve
   process needs Kilo's experimental flag: TENG-3131 waits on that decision.

Amp's `amp tools use` may be a fourth once it is shown to reach MCP tools;
that needs an Amp sign-in.

Everything else has no bridge today. Primitives saved for those agents get
a pointer that says so (TENG-3109), and they can still run the primitive
from an agent that has one.

## Antigravity: how a bridge would work

1. The model calls `tap_run` (Antigravity's `call_mcp_tool` with
   `ServerName: tap`).
2. The runner answers with the first tool call the primitive needs, as the
   Gemini bridge does (`tailToolCallRequest`).
3. A `PostInvocation` hook (runs after the tool calls finish) reads that
   answer from `transcriptPath` and returns
   `injectSteps: [{"toolCall": {"name": "call_mcp_tool", "args": {"ServerName": …, "ToolName": …, "Arguments": …}}}]`.
4. Antigravity runs the injected call with its own connection and its own
   `PreToolUse` approval; its result lands at step k+1 of the transcript (see
   the R1 reader). The next hook passes it to the runner, and the loop
   repeats until the runner returns the primitive's output.

**To verify live before building:** whether an injected `toolCall` step
runs without a model call, whether it goes through the normal approval
prompt, and whether the result is in the transcript before the next hook
fires. The Antigravity CLI uses the same contract under `antigravity-cli/`.
