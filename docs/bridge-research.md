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
| Gemini CLI | `AfterTool` hook returns a tail tool call | **bridge** (built, experimental) | `host/hook.go` |
| Antigravity | `PreInvocation` / `PostInvocation` hooks return `injectSteps: [{"toolCall": {"name", "args"}}]`; each hook gets `transcriptPath`, where the injected call's result is written | **candidate: verify live** | Hook contract embedded in `Antigravity.app/Contents/Resources/bin/language_server` ("Lifecycle Hooks (`hooks.json`)", sections 3 and 4) |
| Cursor IDE | none. Hooks (`preToolUse`, `beforeMCPExecution`, …) answer only `permission`, `user_message`, `agent_message`, `updated_input` (rewrites the arguments of a call the model already made), `additional_context`; `stop` can send a `followup_message`, which is a model turn. Its extension host stubs `vscode.lm`: `invokeTool` rejects with "Language model tools are not available" and `tools` is always empty, so our VS Code extension cannot reach Cursor's MCP servers | **no** | `Cursor.app/…/workbench.desktop.main.js` (`agent.v1.PreToolUseRequestResponse`, `agent.v1.PostToolUseRequestResponse`); `…/api/node/extensionHostProcess.js` |
| Cursor CLI (`cursor-agent`) | same hooks as the IDE. `cursor-agent acp` is an ACP server; its `initialize` offers `loadSession`, `mcpCapabilities`, `promptCapabilities`, `sessionCapabilities` and no tool call | **no** | `cursor-agent acp` initialize, run here 2026-10-02 (CLI 2026.08.25) |
| Goose | ACP custom request `_goose/unstable/tools/call` `{sessionId, extensionName, name, arguments}` runs one extension tool through Goose's own extension manager (`dispatch_app_tool_call`) with no model turn and returns `{content, structuredContent, isError}`. It refuses unless the session is in **auto mode**, so Goose shows no approval of its own; the runner's effect gate is the only one. Tools whose `_meta.ui.visibility` excludes `app` are refused. Marked unstable | **candidate: build after a live test**, approval caveat | `aaif-goose/goose` `crates/goose-sdk-types/src/custom_requests.rs` (`GooseToolCallRequest`), `crates/goose/src/acp/server/tools.rs` (`on_call_tool`), `crates/goose/src/agents/reply_parts.rs` (`is_tool_visible_to_app`), read 2026-10-02 |
| GitHub Copilot CLI | Hooks: `preToolUse` returns `permissionDecision`, `permissionDecisionReason`, `modifiedArgs`; `postToolUse` returns `modifiedResult`, `additionalContext`; nothing makes a call. `copilot --acp` is plain ACP | **no** | docs.github.com/en/copilot/reference/hooks-configuration (checked 2026-10-02) |
| OpenCode | `opencode serve` routes (from `@opencode-ai/sdk` 1.18.34): `/mcp` and `/mcp/{name}/connect`, `/auth`, `/disconnect` manage servers, `/experimental/tool` lists tools, `/session/{id}/shell` runs a shell command; no route runs a tool. Plugin hooks `tool.execute.before/after` rewrite arguments and output | **no** via documented APIs | SDK route list, read 2026-10-02; opencode.ai/docs/server |
| Windsurf (Devin Desktop) | Cascade hooks `pre_*`/`post_*` (`read_code`, `write_code`, `run_command`, `mcp_tool_use`, …): a pre hook blocks with exit code 2, everything else observes | **no** (vendor docs) | docs.devin.ai/desktop/cascade/hooks |
| Qwen Code | Hooks return `permissionDecision`, `updatedInput`, `additionalContext` (PreToolUse) and `decision`, `reason`, `additionalContext` (PostToolUse); no chained tool call like Gemini CLI's `tailToolCallRequest` | **no** (vendor docs; source not read) | QwenLM/qwen-code `docs/users/features/hooks.md` |
| Roo Code | Exported `RooCodeAPI`: `startNewTask`, `resumeTask`, `sendMessage`, button presses, settings. Every path goes through the model (`use_mcp_tool`) | **no** | RooCodeInc/Roo-Code `packages/types/src/api.ts` |
| Cline, Kilo Code | No exported invoke-tool API found; their MCP connections are private to the extension and not registered with `vscode.lm`. Kilo is a Roo fork | **no evidence** (not verified in source) | github.com/cline/cline |
| Zed | Zed passes its context servers to external agents over ACP; no way for an outside program to call one | **no evidence** | zed.dev/docs/ai/external-agents |
| Crush | One `PreToolUse` hook returning `decision`, `updated_input`, `context` | **no** (vendor docs) | charmbracelet/crush `docs/hooks/README.md` |
| Amp | Nothing in the manual; toolbox and plugin pages not read | **no evidence**, still open | ampcode.com/manual |
| Continue | Not researched | **open** | |
| Aider | No MCP at all (issue 3314, RFC 4506 open) | **no** | Aider-AI/aider#4506 |


"vendor docs" rows rest on the vendor's published documentation, read
through a summarizer; the Cursor, Antigravity, Goose and OpenCode rows were
checked in the shipped binary, app bundle, source or SDK.

## What to build (P3b)

Two agents have a mechanism worth a build ticket, each starting with a live
test in the shape of `bridge/live_test.go`:

1. **Antigravity**: hook-injected tool calls (below). It keeps
   Antigravity's own approval (`PreToolUse` still runs on the injected step).
2. **Goose**: `_goose/unstable/tools/call` over ACP. Simplest protocol of
   all, but it requires auto mode, so a run has no Goose approval prompt and
   relies on the runner's effect gate alone; the method is unstable. Decide
   whether that approval model is acceptable before building.

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
