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
