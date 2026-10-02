# Synthetic fixtures

These fixtures were written by hand, not captured from a real run (decision,
2026-10-02, TENG-3117: the agents below need a sign-in this machine does not
have). Each follows the format its agent's own code writes, read from that
code, and exercises the edge cases of the format:

- gemini-cli/: Gemini CLI 0.62 session log (bundle loadConversationRecord):
  a message replaced in place by id, $rewindTo, $set, a torn last line.
- qwen-code/: Qwen Code 0.24.7 session (ChatRecordingService): a record tree
  with a branch abandoned by a rewind, usageMetadata, a .ledger.jsonl that
  is not a session.
- vscode-copilot/: VS Code 1.11x chat log (the log reader's _applySet,
  _applyPush): kinds 0-3 including a push with a cut (i), and a pre-1.109
  whole-document .json session.

Real-run fixtures replace these when a real session is available.

# Real-run fixtures (R3-R6)

opencode/, kilo/, goose/, crush/, continue/, cline-cli/ and aider/ are real
sessions, run on 2026-10-02 with the agent's own CLI against Fireworks
(deepseek-v4p1-flash) and the fixture MCP server (`discover-fixture mcp`),
on the scripted task of plan §6.4, then captured with cmd/discover-fixture
(redacted, databases as SQL). The *.json / config.yaml files beside them
are the agents' MCP configuration with the commands replaced.

# VS Code extension tasks (R3)

vscode-ext/ holds synthetic Cline, Roo Code and Kilo Code extension tasks
(api_conversation_history.json): native tool_use with use_mcp_tool and
execute_command, and one older Cline task with the XML tool form. No
extension is signed in on this machine; the Cline CLI fixture (cline-cli/)
is a real run.
