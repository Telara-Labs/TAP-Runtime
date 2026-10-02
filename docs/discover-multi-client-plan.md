# tap discover: many clients in, many clients out

Status: decisions taken 2026-10-02 (§1.1). Epic **TENG-3107**: P1 TENG-3108, P2 TENG-3109, P3r TENG-3110, P4a TENG-3111, R1 TENG-3112 (§8).

Implemented 2026-10-02 on main: P1 `30043b9`, P4a `07d45dc`, R1 `76b2d99`, P2 `fc52c6c` + `96077f9` + `04bd56b` (root pin bump and runner end-to-end test). P3r: `docs/bridge-research.md`.
Written 2026-10-02 against tap-runtime `060f037`.

## 1. Goal

- **In:** find recurring work in the session history of every agent the
  person uses, not only Claude Code, Codex and Cursor.
- **Out:** a primitive saved once is findable and runnable from every agent
  the person uses.

We make **primitives**, not skills. A primitive is a TAP package (manifest,
program, `.tap-primitive.json` marker), and agents reach it through the TAP
MCP server (`tap_search`, `tap_load`, `tap_run`). A `SKILL.md` is only a
pointer, so an agent notices the primitive without first calling `tap_search`.

### 1.1 Decisions (Luis, 2026-10-02)

| # | Decision | Effect on the plan |
|---|---|---|
| D1 | Discover reads **all detected agents** by default | `--client` defaults to `client.Detected(home)`. Evaluations and parity checks stay Claude-only unless told otherwise |
| D2 | **Collection + `SKILL.md` pointers.** A primitive is saved once to the TAP collection `<userConfigDir>/tap/primitives/<name>`; each chosen agent gets a small pointer `SKILL.md` folder, never the package | Replaces "canonical copy + symlinks" (§3.2) |
| D3 | **Research a bridge per agent.** The runner runs a tool-calling primitive only where it can borrow the agent's connections (`host/tools.go:74` `openBridge`, ruling 13). Today that is Claude, Codex, VS Code and Gemini | New research ticket; a bridge is built per agent that has a viable mechanism (§3.3) |
| D4 | Zed decompression **calls the `zstd` command** | Same pattern as `sqlite3` (`history/cursor.go:22`). No new Go dependency; the reader reports "zstd not installed" when it is missing |
| D5 | **New epic, linked** to TENG-3054 (readers), TENG-3059 (save), TENG-3050 (npm setup) and blocked by TENG-3084 (the discover subpackage split) | §8 |

## 2. Where things stand

Each of the three per-client steps keeps its own hardcoded client list, and
the lists don't match:

| Step | claude-code | codex | cursor | gemini | Code |
|---|---|---|---|---|---|
| Read history | yes | yes | yes (IDE store only) | no | `discover/history/readers.go:15` |
| Save the primitive | yes | yes | no | no | `discover/pack/install.go:30` `SkillsDir` |
| Wire the runner (MCP) | yes (`claude`) | yes | no | yes (+ AfterTool hook) | `host/install.go:202` |

Other call sites that switch on the client name and must follow the registry:

- `discover/author/author.go:278` `FindSession`: claude/codex/cursor; re-reads files per client
- `discover/primitive/handoff.go:89` `findTranscript`: claude-code only; other clients silently get `""`
- `discover/primitive/menu.go:673` `agentCommand`: claude/codex launch lines
- `discover/command.go:76,86,180`: `--client` and `--save-client` defaults and help text
- `discover/genreview/program_command.go:33` and `discover/author/save.go:199`: `--save-client` / `--client`, "claude-code or codex"
- `discover/cmd/discover-eval/main.go:79`: fixed reader list
- `host/main.go:240`: usage string `claude|codex|gemini`
- `npm/lib/runner.cjs:47`: the `@telara/tap` npm postinstall registers the
  runner MCP with `['claude', 'codex']`. This is a JS copy of the list. It is
  **in flight under TENG-3050** in another session (`npm/` is untracked).
  Coordinate on that ticket before P3. The postinstall should call
  `tap install --client all` (detected clients) rather than keep its own list.

Naming split: `host` calls it `claude`, `discover` calls it `claude-code`.

Module boundary: `discover` is its own module (kept small because telara-cli
imports it). The root module (`host`) requires it by pseudo-version
(`go.mod:11`). Anything `host` uses from a new `discover` package ships in two
steps: publish discover, then bump the pin. No `replace` directives.

## 3. Design

### 3.1 One client registry: `discover/client`

A new leaf package that holds **data only**: no imports of `history`, `pack`
or `trace`, so every other package can import it without a cycle.

```go
package client

type Client struct {
    ID      string   // "claude-code": the one name used everywhere
    Aliases []string // "claude"
    Name    string   // "Claude Code", for menus

    // Detect reports that the agent is installed for this user: a marker
    // directory or binary, never a network call.
    Markers []string // paths under home, e.g. ".claude"; any one existing counts

    Skills SkillsPaths // where pointer SKILL.md folders go; empty = no pointers
    MCP    MCPConfig   // how the TAP MCP server is connected, or none
    Bridge bool        // the runner can borrow this agent's connections (mirrors host openBridge; a test keeps them equal)
    Launch []string    // argv prefix to start the agent on a prompt, or nil
}

type SkillsPaths struct {
    Global  string // under home, e.g. ".claude/skills"
    Project string // under the project, e.g. ".claude/skills"
}

func All() []Client
func Lookup(nameOrAlias string) (Client, bool)
func Detected(home string) []Client
```

The **history reader is not a field.** `history` keeps
`map[client.ID]func(home string) trace.Reader`. A test (§6.1) requires every
registry entry marked `HasHistory` to have a reader there, and every reader
to have a registry entry. That way the registry stays data-only and neither
side can drift without a red test.

Each call site in §2 changes from `switch client` to `client.Lookup` plus a
capability check, and returns one shared error naming the supported IDs. That
error text is derived from `All()`, never typed by hand.

### 3.2 Out: save once, point every agent at it (D2)

**Where the primitive goes.** `pack.Install` unpacks into the TAP collection
`<userConfigDir>/tap/primitives/<name>` (macOS `~/Library/Application
Support/tap/primitives`). `host/catalog.go:26` already scans that root first,
so `tap_search` finds the primitive from every agent that has the TAP MCP
server connected, with no per-agent copy.

**Pointers.** For each chosen agent, write `<agent skills dir>/<name>/SKILL.md`
and nothing else:

- The pointer names the primitive (`publisher/name@version` and its run
  digest, the one `tap_search` lists and `tap_run` checks: sha256 of
  `primitive.yaml` + entrypoint, not the archive digest in the marker) and
  says to run it with `tap_run`. It holds no package and no
  `.tap-primitive.json`, so the catalog never lists it twice ("ordinary
  `SKILL.md` folders are not treated as TAP primitives",
  `docs/install.md`).
- Ownership comes from a front-matter field (`tap-pointer: <ref>`). A folder
  without it is never touched (the existing `ErrNotSaved` rule).
- Agents that share a folder (`.agents/skills` at project scope) get one
  pointer.
- Pointers go only to agents where the primitive can run (a bridge exists,
  §3.3) or that the person explicitly picks. For a picked agent with no
  bridge, the pointer says the primitive can't run there yet.

**Flags.** `--save-client all|<list>` (default: detected agents),
`--project` for project-scope pointers, and a multi-select in the menu.

**Migration.** Primitives saved before this change live as full packages in
`~/.claude/skills/<name>` and `~/.codex/skills/<name>`. On the next save or a
one-off `tap discover --migrate-saved`, move each one into the collection and
leave a pointer behind. The catalog keeps scanning the old roots until the
migration ships, so nothing disappears in between.

**Report.** `InstallResult` gains `Collection` (path) and `Pointers
[]{Client, Path, Mode (written|unchanged|skipped), Reason}`.

### 3.3 Out: run in every agent: connect the TAP MCP server and build a bridge (D3)

Two separate requirements per agent:

1. **The TAP MCP server is connected**, so `tap_search` and `tap_run` exist in
   that agent. This is a config edit: `claude mcp add`, `codex mcp add`, or one
   entry merged into the agent's MCP JSON file (Cursor `~/.cursor/mcp.json`,
   Windsurf `~/.codeium/windsurf/mcp_config.json`, OpenCode
   `opencode.json[c]` key `mcp`, Gemini `settings.json`). Merge, keep a backup,
   and refuse a malformed file (the pattern `addGemini` already follows).
2. **A bridge exists**, so the runner can make the primitive's tool calls
   through the agent's own connections and approvals, with no credentials
   (`bridge/` package, doc 34 §13.10). Today:

| Agent | Bridge | Mechanism |
|---|---|---|
| Claude Code | yes | stream-json control channel (`bridge/claude.go`) |
| Codex | yes | app-server protocol (`bridge/codex.go`) |
| VS Code (Copilot) | yes | our extension (`bridge/vscode.go`, `vscode/`) |
| Gemini CLI | yes, experimental | `AfterTool` hook with `tailToolCallRequest` (`host/hook.go`). The hook makes Gemini run each call; it is not a recording hook |
| everything else | no | `openBridge` refuses: "client cannot lend its connections" |

**Bridge research ticket (one ticket, one table, per D3).** For each agent,
answer: can a program ask it to run a tool on the runner's behalf, with its
own connection and approval? Candidates to check:

- Cursor: hooks (`preToolUse`/`postToolUse`), and the cursor-agent CLI's
  stream/ACP modes.
- OpenCode: its plugin API (`tool.execute.*`) and `opencode serve` HTTP API.
- Copilot CLI: hooks (`preMcpToolCall`) and its ACP mode.
- Windsurf: hooks can only observe or block, so probably no bridge.
- Antigravity: `PreToolUse`-style hooks.
- Cline, Roo, Kilo: are their MCP connections reachable from our VS Code
  extension?
- Zed and Goose: ACP or extension hosts.

Each viable agent becomes its own build ticket, with a live test like the
existing `bridge/live_test.go`.

`host/install.go` and the npm postinstall (`npm/lib/runner.cjs:47`, TENG-3050)
take their client list from the registry, so `tap install --client all`
connects the TAP MCP server everywhere it is detected.

### 3.4 In: more transcript readers

**Split every reader into a decoder plus one shared assembler first.** Today
each reader both decodes its agent's format and assembles a `trace.Session`
itself. The assembly part is the same in every reader:

- pair a call with its result by id,
- fill `Outcome`, `OutIDs` and the other result fields, and `Output`,
- attribute calls to the current request,
- spread a turn's token usage over its calls.

(Compare `claude.go:105-128` with the equivalent blocks in `codex.go` and
`cursor.go`.) Pull it into `history/assemble.go`, which consumes a small
event stream:

```go
type Event interface{ event() }
type UserText   struct{ Time time.Time; Text, Role string }
type ToolCall   struct{ Time time.Time; ID, Name string; Args map[string]json.RawMessage; Turn string }
type ToolResult struct{ ID, Text string; IsError bool }
type TurnUsage  struct{ Turn string; Usage trace.Usage }
```

A decoder only turns its agent's records into these events. It also names
its agent's MCP tool-naming scheme: `mcp__server__tool` for Claude Code and
Cline, a namespace field for Codex, args for Cursor. Claude, Codex and Cursor
move onto it first. The existing reader tests must stay green unchanged,
which proves the refactor. Codex keeps its extra JS `exec` unpacking
(`codex.go:343-850`), which emits several `ToolCall`s from one script. That
is the only reason it is 862 lines.

#### 3.4.1 Standardized reader stack (research 2026-10-02, corrected)

**Every agent below has a readable history.** A first research pass marked
Windsurf, Antigravity, Zed, Aider and Amp "unreadable". That was wrong, and
every row below has now been checked against the agent's source, files on
this machine, or vendor docs.

**Reference implementation:** [vshulcz/deja-vu](https://github.com/vshulcz/deja-vu)
(MIT, Go) already reads 35 agents' local histories, with one registry entry
per agent (`internal/sources/registry.go`), per-agent format docs
(`docs/registry/<agent>.md`) and real fixtures. That is the same
registry-plus-decoder design as here. Port its readers (keeping the MIT
notice) instead of reverse-engineering 20 formats again. Its known
limits are tracked in its own issues (e.g. #4530, failed commands with no
exit code for roo/continue/amp/antigravity).

Every agent's history comes apart into **five layers**. Only L1 differs per
agent. L2–L4 are shared pieces chosen per agent, and L0 decides how the
history is obtained at all.

| Layer | Question | Shared implementations |
|---|---|---|
| **L0 Acquisition** | How do we get the history? | `LocalStore` (read files or a DB the agent already writes; almost every agent) · `CLIExport` (run the agent's own logged-in CLI to export; Amp) · `HookCapture` (the agent's hooks hand us a transcript path or each tool call; Windsurf, and a fallback for any agent) |
| **L1 Entry point** (per agent) | Where is it on each OS; how do I list sessions since T? | `Locate(home) []Source`: data plus a glob or SQL |
| **L2 Envelope** | How do I rebuild a session document? | `AppendLog` · `ReplayLog` (snapshot + patches) · `Rows` (DB rows) · `Document` (one JSON per session) · `Compressed` (zstd JSON in a DB column; Zed) · `Markdown` (Aider) |
| **L3 Message dialect** | Which record is a user turn, tool call, result or usage? | `AnthropicBlocks` · `OpenAIResponses` · `AISDKMessages` · `GeminiRecord` · `VSCodeChat` · `CursorBubbles` · `CascadeSteps` · `CopilotEvents` · `ZedThread` · `AiderMarkdown` |
| **L4 Tool-name dialect** | Which MCP server and tool? | `DoubleUnderscore` (`mcp__server__tool`) · `Dispatcher` (generic tool; args name server and tool) · `Namespaced` (separate server field) |

**Per agent (all confirmed readable):**

| Agent | L0 / L1 store | L2 · L3 · L4 | Evidence |
|---|---|---|---|
| claude-code | `~/.claude/projects/*/<id>.jsonl` | AppendLog · AnthropicBlocks · DoubleUnderscore | existing reader |
| codex | `~/.codex/sessions/**/*.jsonl` | AppendLog · OpenAIResponses (+ JS `exec`) · Namespaced | existing reader |
| cursor IDE | `Cursor/User/{globalStorage,workspaceStorage/*}/state.vscdb` | Rows (KV) · CursorBubbles · Dispatcher | existing reader. globalStorage holds every conversation: checked 2026-10-02 (TENG-3127), all 238 composers listed in the 70 workspaceStorage dbs are in globalStorage, and no workspace db has bubble rows (only composer metadata and aiService prompt text, no tool calls) |
| **cursor CLI** | `~/.cursor/chats/<ws>/<id>/store.db` (`blobs`: JSON messages + protobuf tree nodes; `meta.json` has `cwd`, `createdAtMs`); also `~/.cursor/projects/<path>/agent-transcripts/**/*.jsonl` (`{role,message:{content:[tool_use…]}}`, no results) | Rows · AISDKMessages (`tool-call{toolCallId,toolName,args}` / `tool-result{toolCallId,result}`) · Dispatcher (`CallMcpTool{server,toolName,arguments}`, `CallDynamicTool{namespace,toolName,arguments,mcpDetails}`) | **on disk**: 30 store.db sessions with 1185 paired calls; 729 agent-transcript files. Use store.db (it has the results) |
| **antigravity** | `~/.gemini/antigravity/brain/<id>/.system_generated/logs/transcript_full.jsonl` (also per-conversation SQLite `conversations/<id>.db` with protobuf `steps`, not needed) | AppendLog · CascadeSteps: `{step_index,source,type,status,created_at,content,tool_calls[{name,args}]}`; types USER_INPUT/PLANNER_RESPONSE/GENERIC/ERROR_MESSAGE; one call per step; its result is step k+1 (GENERIC, or ERROR_MESSAGE = failed). Some result steps are never written, so pair by index, not order: 131 of 136 calls here had k+1, 5 had none · Dispatcher (`call_mcp_tool{ServerName,ToolName,Arguments}`) | **on disk**: 4 conversations, 136 calls |
| **windsurf** (now Devin Desktop) | HookCapture (documented): with a `post_cascade_response_with_transcript` hook in `~/.codeium/windsurf/hooks.json`, Windsurf writes the full conversation to `~/.windsurf/transcripts/{trajectory_id}.jsonl` (0600, pruned to 100 newest files) and passes the path on stdin. Per-call hooks: `post_mcp_tool_use{mcp_server_name,mcp_tool_name,mcp_tool_arguments,mcp_result}`, `post_run_command{command_line,cwd}`. History from before the hook was installed lives only in `~/.codeium/windsurf/cascade/*.pb` (no public schema or export; users have asked for one) | AppendLog · CascadeSteps, lower-case variant: `{"type":"user_input","status":"done","user_input":{…}}`, one step per line, step data under a key named after its type · Namespaced (hook fields) | **vendor docs** (docs.devin.ai/desktop/cascade/hooks). Capture runs going forward only. Not installed here |
| **gemini-cli** | `~/.gemini/tmp/<project>/chats/session-*.jsonl` | ReplayLog (`$set`/`$patch`/`$rewindTo`) · GeminiRecord (`toolCalls[]{id,name,args,result,status}`, `tokens{…}`) · to check | **source** + **on disk** (1 session; header and `$set` match the source) |
| qwen-code | `~/.qwen/tmp/<project>/chats/` | as gemini-cli | deja-vu `qwen.go` + `docs/registry/qwen.md` |
| **vscode-copilot** (GitHub Copilot Chat in VS Code) | `Code/User/workspaceStorage/<ws>/chatSessions/<id>.jsonl` (VS Code ≥1.109; older or `chat.useLogSessionStorage=false`: `<id>.json`); also `globalStorage/emptyWindowChatSessions` | ReplayLog: `kind:0` full snapshot, `kind:1` set at path `k`, `kind:2` push (optional truncate), `kind:3` delete · VSCodeChat: `requests[].message` (user) and `requests[].response[]` parts; tool calls are `kind:"toolInvocationSerialized"{toolCallId,toolId,invocationMessage,resultDetails,resultError,isComplete,source,subAgentInvocationId}`; token usage in `kind:"usage"` parts · Namespaced (`source` = ToolDataSource, which names the MCP server) | **source** (microsoft/vscode `chatService.ts`) + **on disk** (222 files) + deja-vu PR #3088 |
| **copilot-cli** (+ Copilot agent in JetBrains) | `${COPILOT_HOME:-~/.copilot}/session-state/<id>/events.jsonl`; an index in `~/.copilot/session-store.db`. JetBrains Copilot agent sessions are reported to use the same store | AppendLog · CopilotEvents: envelope `{type,data,id,timestamp,parentId}`; `session.start{sessionId,copilotVersion,startTime,context.cwd}`, `user.message{content,transformedContent}`, `assistant.message`, `assistant.turn_start/end{turnId}`, `tool.execution_start{toolCallId,toolName,arguments}`, `tool.execution_complete{toolCallId,success,result}`, `subagent.*`, `skill.invoked`, `session.compaction_complete{…tokens}` · to check | community schema reference (RockNoggin gist) + github/copilot-cli issues #2649, #3366, #3551, #4098 + deja-vu fixture. Known defects to tolerate: result content with raw newlines splits a line, orphan calls with no complete event, interleaved subagent events. No sessions on this machine |
| **cline** | VS Code ext: `globalStorage/saoudrizwan.claude-dev/tasks/<id>/api_conversation_history.json`; CLI/SDK: `~/.cline/data/sessions/<id>/<id>.messages.json` | Document · AnthropicBlocks (native `tool_use`/`tool_result`; older tasks have XML tool tags in text, paired by order) · DoubleUnderscore/Dispatcher (`use_mcp_tool{server_name,tool_name,arguments}`) | deja-vu `cline.go` (3 kinds) + docs |
| roo / kilo | `globalStorage/rooveterinaryinc.roo-cline/tasks/<id>/api_conversation_history.json` (+ `history_item.json`); Kilo: same task tree + an OpenCode-schema DB for its CLI | Document · AnthropicBlocks (`ApiMessage = Anthropic.MessageParam & {ts…}`); Kilo CLI = the opencode decoder · as cline | Roo **source** (`task-persistence/apiMessages.ts`) + deja-vu `roo.go`, `kilo.go`. Hosts: Code, Insiders, VSCodium, Cursor, Windsurf |
| **opencode** | `~/.local/share/opencode/opencode.db`: `session`, `message`, `part` (v1); `session_message{session_id,type,seq,data}` (v2) | Rows · own parts (tool part with `state`) · to check | schema **on disk** (0 rows here) + deja-vu `opencode.go` |
| **zed** | macOS `~/Library/Application Support/Zed/threads/threads.db`; Linux `~/.local/share/zed/threads/threads.db` | Compressed (`data_type` zstd or json) · ZedThread (v0.3.0 `{"User"|"Agent":{content:[Text|ToolUse…], tool_results:{id→…}}}`; legacy v0.2.0 `segments`) · to check | Zed **source** (`crates/agent/src/db.rs`) + deja-vu docs. Needs a zstd decoder: `klauspost/compress/zstd` is pure Go, but check it against `discover`'s small-deps rule |
| **goose** | `~/.local/share/goose/sessions/sessions.db` (≥1.10; Windows `%APPDATA%\Block\goose\data\sessions`); legacy `sessions/*.jsonl` | Rows (`messages{role,content_json,created_timestamp}`) · own (`toolRequest`/`toolResponse` with `id`) · to check | goose **source** (`session_manager.rs`) |
| **crush** | `<project>/.crush/crush.db`, projects listed in `~/.local/share/crush/projects.json` | Rows (`messages{role,parts JSON}`) · own parts · to check | crush **source** (initial migration) + deja-vu `crush.go` |
| continue | `~/.continue/sessions/*.json` | Document · own · to check | deja-vu registry |
| **amp** | CLIExport: since build 0.0.1774963753 (2026-03-31) threads live on ampcode.com; `amp threads list --json` then `amp threads export <id>` (full JSON, the user's own login). Older builds: `~/.local/share/amp/threads/T-*.json` | Document · own · to check | deja-vu `amp.go` comments + Amp docs |
| **aider** | `.aider.chat.history.md` at each repo root (+ `~/.aider.chat.history.md`; `.aider.input.history`) | Markdown · AiderMarkdown (`# aider chat started at <time>`, user turns `#### `, command and tool output `> `). Aider has no MCP or tool calls; shell commands and edits are recovered from the text | aider **source** (`io.py`). Feeds discover shell/edit patterns only, not MCP primitives |

**What standardizing buys:**

- `ReplayLog` serves Gemini CLI, Qwen and VS Code Copilot.
- `AnthropicBlocks` serves Claude Code, Cline, Roo and Kilo.
- `AISDKMessages` serves the Cursor CLI, and likely OpenCode.
- `CascadeSteps` serves Antigravity and Windsurf.
- `Dispatcher` unwrapping uses the same rule as the runner's TENG-3054 (a
  dispatcher call takes the identity of the operation it names). Cursor
  (both), Antigravity and Cline use it.
- `HookCapture` is the fallback whenever an agent's store is unreadable or
  missing. The runner already installs hooks (`tap hook gemini`), so the
  same mechanism can record calls for discover.

Each reader implements `trace.Reader` and must fill, per call: `Tool`,
`MCPServer`/`MCPTool`, `Args`, `Output`, `Outcome`, `Time`, `Request` index,
plus `Session.Requests`, `RequestRoles` and `SourceDigest`. Token usage is
optional (`Measured=false` when absent).

Build order (real data on this machine first, then by shared pieces):

| # | Agents | Shared pieces it brings | Fixture source |
|---|---|---|---|
| R1 | cursor CLI, antigravity | AISDKMessages, CascadeSteps, Dispatcher | real sessions here |
| R2 | vscode-copilot, gemini-cli, qwen | ReplayLog, VSCodeChat, GeminiRecord | sessions here (Gemini: run one with tools) |
| R3 | cline, roo, kilo | AnthropicBlocks reuse, XML-tag fallback | deja-vu fixtures + a real install |
| R4 | opencode, goose, crush, continue | Rows decoders | deja-vu fixtures + a real run each |
| R5 | copilot-cli, zed | CopilotEvents, Compressed/ZedThread | deja-vu fixtures + a real run |
| R6 | windsurf, amp, aider | HookCapture, CLIExport, Markdown | a hook install, an `amp` login, an aider run |

Each row is its own ticket and lands only with fixtures from a real run. Every
format here is undocumented and versioned by the vendor, so each reader must:

- return `(nil, nil)` when the store is absent (the existing contract),
- skip a record it can't parse and count it, never fail the whole read, and
  report the skipped count in `discover --stats`,
- set `SourceDigest` so frozen corpora (`history/frozen.go`) keep working.

Default `--client` is all detected agents (D1). Fix `command.go:76` and `:180`,
which hardcode two different lists today.

## 4. Phases

| Phase | Scope | Size | Depends on |
|---|---|---|---|
| P1 | `discover/client` registry; move every §2 call site onto it; `--client` default = detected (D1); `handoff.go` resolves transcripts for every agent | 1–2 d | TENG-3084 (the discover split lands first) |
| P2 | Save to the collection + pointers (D2), flags, report, migration of existing saved primitives | 2 d | P1 |
| P3 | Publish discover; bump the root pin; `tap install --client all` from the registry; MCP config writers for Cursor, Windsurf and OpenCode; npm postinstall delegates (with TENG-3050) | 2 d | P1, TENG-3050 |
| P3r | Bridge research: one table for every agent (§3.3) | 2–3 d | — |
| P3b | One bridge per viable agent | per agent | P3r |
| P4a | Shared assembler; Claude, Codex and Cursor move onto decoders with existing tests unchanged | 2 d | P1, TENG-3084 |
| P4b | Readers R1–R6 (§3.4.1) | ~1–2 d per row | P4a |

## 5. Out of scope

- Rules-only agents (formats other than `SKILL.md`). Every agent in the
  `npx skills` table reads a skills folder, so no adapter is needed now.
- Cloud-only agents with no local CLI to export from (e.g. Devin web sessions).
  Amp is in scope through `amp threads export` (§3.4.1).
- Remote or cloud installs. Everything stays local files.
- New environment variables: none. Paths come from the registry, and the only
  existing env read is `APPDATA` in `CursorStateDB`.

## 6. Testing

All tests are Go tests in the module that owns the code. No bash scripts.

### 6.1 Registry (P1), unit

- `TestRegistryIDsAndAliasesAreUnique`: no ID or alias belongs to two clients.
- `TestEveryHistoryClientHasAReaderAndViceVersa`: compares `client.All()`
  with the `history` reader map. This test is what keeps the two lists from
  drifting.
- `TestEveryCallSiteAcceptsEveryRegisteredClient`: for each `client.All()`,
  `history.DefaultReaders`, `pack.SkillsDir`, `author.FindSession` and
  `agentCommand` either succeed or return the shared "not supported for
  <capability>" error, never "unknown client".
- `TestAliasesResolve`: `claude` → `claude-code`, in both `discover` and
  (after P3) `host`.
- `TestDetectedUsesOnlyHome`: a temp home with `.cursor` and `.gemini`
  detects exactly those two.

### 6.2 Save once + pointers (P2), unit, temp home and temp project

- Saving puts exactly one package in `<config>/tap/primitives/<name>`, and
  `localCatalog` returns it once, even with pointers present in three agents'
  skills folders.
- Each pointer `SKILL.md` names the right `publisher/name@version` and
  digest, and carries the `tap-pointer` field; the pointer folder holds no
  `.tap-primitive.json`.
- Saving again reports `unchanged` for the collection and every pointer, and
  writes nothing (compare mtimes). A new version replaces the collection
  folder atomically and rewrites the pointers' digest.
- A foreign folder at a pointer path is left untouched and reported as
  skipped; the other pointers are still written.
- Two agents sharing `.agents/skills` get one pointer and two report lines.
- An agent with no bridge gets a pointer only when picked explicitly, and
  that pointer says it can't run there yet.
- Migration: a full package in `~/.claude/skills/x` moves to the collection
  and leaves a pointer; `tap_search` finds it once before and after.

### 6.3 MCP config and bridges (P3/P3b), unit and integration

- Unit, per MCP config writer: start from a fixture that already holds
  other servers, comments (OpenCode JSONC) and unknown keys. After install,
  exactly one entry is added and the rest is byte-identical; a backup exists;
  running it twice is a no-op; uninstall removes only our entry; a malformed
  file is refused, never overwritten.
- Integration: for each agent whose CLI is on the machine, run `tap install
  --client X` against a temp home, then ask the agent to list its MCP servers
  and assert `tap` appears. An absent binary is reported as not run, never as
  passed.
- Per new bridge: a live test in the shape of `bridge/live_test.go`. Run a
  primitive with one read call through the agent's own connection, and
  assert the run record shows the call and the agent's approval path.
- End to end, one agent per bridge: discover a pattern from that agent's
  history, save, run it from the same agent, and assert a `tap_run` call
  in the transcript that the agent writes, read back by its own reader.

### 6.4 Readers (P4), per client

- **Fixtures come from real runs**, captured with a fixed script of actions:
  one plain shell call, one MCP call that succeeds, one MCP call that fails,
  one call whose output holds ids, two user turns, and one mid-request
  "yes". Redact the fixture with the existing `discover/redact` package and
  check it into `history/testdata/<client>/`.
- Per-reader table test, the same shape as the existing
  `readers_test.go`: calls in order, `Tool`, `MCPServer`/`MCPTool` split,
  `Outcome` for ok and failed calls, `Request` attribution, `Approvals`,
  `OutIDs` from the id-bearing output.
- An absent store returns `(nil, nil)`. A corrupt record is skipped and
  counted, and the rest of the session survives.
- `ReplayLog` gets its own tests, written once and shared by both users:
  a `$rewindTo` drops the target and everything after it; a patch updating
  a tool result fills the matching call's `ToolResult`; `removeIds` and
  `orderIds` are applied; a torn last line (session still being written) is
  ignored without losing the earlier lines. The same cases are tested for the
  VS Code `kind:1/2` path patches.
- `Dispatcher` unwrapping uses one table shared with the runner's TENG-3054
  rule. Test: `CallMcpTool{server:"jira",toolName:"get_issue"}` and Claude's
  `mcp__jira__get_issue` produce the same `MCPServer`/`MCPTool`.
- A `SourceDigest` test confirms it is stable across reads and changes when
  the file changes.
- **Cross-client equivalence:** run the same scripted task in Claude Code
  and in the new client. After `trace` normalization, the two sessions give
  the same tool sequence and the same `discover` family fingerprint. This is
  the test that shows a reader is good enough to feed discover, not just
  parse files.
- Frozen corpus: add each new client's fixture to a frozen manifest. The
  eval (`cmd/discover-eval`) must stay byte-identical for the existing
  clients when a new reader lands.

### 6.5 Regression gate for every phase

`cd tap-runtime/discover && go test ./...` and `cd tap-runtime && go test
./host/... ./release/...`, both with `GOWORK=off` so a sibling's uncommitted
work can't make them pass. Paste the output on the ticket.

## 7. Risks

| Risk | Mitigation |
|---|---|
| Vendor transcript formats change without notice | A reader skips what it can't parse and reports the count; real-run fixtures get re-captured per client release; `discover --stats` shows a skip spike |
| Migration loses a saved primitive | Move then pointer, with the catalog scanning old roots until the migration ships; `tap_search` before/after test (§6.2) |
| A pointer in an agent with no bridge or no TAP MCP | Pointers go only where runnable unless picked; the pointer and report say why and give the `tap install` command (§3.2, §3.3) |
| JSON config writers damaging a user's settings | Merge, never rewrite; `.bak`; refuse malformed input; byte-identical tests (§6.3) |
| Registry and reader map drifting | `TestEveryHistoryClientHasAReaderAndViceVersa` |
| The paths table (from `npx skills`, fetched 2026-10-02) is wrong for some agent | Each registry entry is checked once against a real install before its client is marked `Detect`-default |

## 8. Tickets (TENG, via Telara MCP)

Epic TENG-3107 "tap discover: many agents in and out" (D5). Links: relates to
TENG-3054, TENG-3059 and TENG-3050; P1 and P4a are blocked by TENG-3084.

File now (settled, with testable acceptance):

1. TENG-3108 P1 registry and call-site migration (blocked by TENG-3084; blocks 3109, 3111).
2. TENG-3109 P2 save to the collection + pointers + migration (relates TENG-3059).
3. TENG-3110 P3r bridge research table.
4. TENG-3111 P4a shared assembler (blocked by TENG-3084, 3108; blocks 3112).
5. TENG-3112 R1 readers: cursor CLI + antigravity.

File later, when settled:

- P3: after TENG-3050's npm setup lands (same files).
- P3b: one per viable agent, after P3r.
- R2–R6: each once its fixture exists (deja-vu fixtures or a real run).
  Windsurf has no public fixture, so it needs a real hook-captured run.
