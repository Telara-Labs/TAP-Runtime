// The TAP extension for VS Code. It lets the TAP runner borrow the editor's
// tools: the language model tools VS Code knows, including every MCP server
// the user connected for Copilot. The editor makes each call with its own
// connection, and shows its own confirmation where a tool asks for one.
//
// It does two things:
//  - registers the runner with VS Code as the MCP server "TAP Runtime", so
//    Copilot can call tap_run like any other tool;
//  - listens on a private local socket, which it hands the runner as an
//    argument, and answers two requests on it: list the tools, call one.
"use strict";
const vscode = require("vscode");
const net = require("net");
const fs = require("fs");
const os = require("os");
const path = require("path");
const crypto = require("crypto");
const { execFile } = require("child_process");

let server;
let socketPath;

function cacheDir() {
  if (process.platform === "darwin") return path.join(os.homedir(), "Library", "Caches", "tap-runtime", "vscode");
  if (process.platform === "win32") return path.join(process.env.LOCALAPPDATA || os.tmpdir(), "tap-runtime", "vscode");
  return path.join(process.env.XDG_CACHE_HOME || path.join(os.homedir(), ".cache"), "tap-runtime", "vscode");
}

// The runner, from the setting, then its install folder, then PATH. It is
// called tap; an install from before the rename is still found as
// tap-runtime.
function runtimePath() {
  const set = vscode.workspace.getConfiguration("tapRuntime").get("path");
  if (set) return set;
  const win = process.platform === "win32";
  const candidates = win
    ? [path.join(process.env.LOCALAPPDATA || "", "Programs", "tap", "tap.exe"),
       path.join(process.env.LOCALAPPDATA || "", "Programs", "tap-runtime", "tap-runtime.exe")]
    : [path.join(os.homedir(), ".local", "bin", "tap"),
       path.join(os.homedir(), ".local", "bin", "tap-runtime")];
  for (const c of candidates) if (fs.existsSync(c)) return c;
  return win ? "tap.exe" : "tap";
}

// A tool result is a list of parts. Text parts are joined; a data part whose
// type is text or JSON is decoded. Anything else is left out, and said so.
function resultText(result) {
  const out = [];
  for (const part of result.content || []) {
    if (part instanceof vscode.LanguageModelTextPart) out.push(part.value);
    else if (vscode.LanguageModelDataPart && part instanceof vscode.LanguageModelDataPart) {
      if (/^(text\/|application\/json)/.test(part.mimeType)) out.push(Buffer.from(part.data).toString("utf8"));
      else out.push(`[${part.mimeType} content left out]`);
    }
  }
  return out.join("");
}

function describe(t) {
  return { name: t.name, description: t.description, inputSchema: t.inputSchema || {}, tags: [...(t.tags || [])], source: t.source && t.source.label ? t.source.label : undefined };
}

async function answer(req) {
  switch (req.op) {
    case "hello":
      return { version: vscode.version };
    case "tools":
      return { tools: vscode.lm.tools.map(describe) };
    case "rules": {
      // chat.tools.eligibleForAutoApproval: a tool set to false is never
      // approved automatically, so VS Code asks the person before every use.
      const eligible = vscode.workspace.getConfiguration("chat.tools").get("eligibleForAutoApproval") || {};
      return { ask: Object.keys(eligible).filter((k) => eligible[k] === false) }; // keys are reference names: "tool", "server/tool" or "server/*"
    }
    case "call": {
      const cts = new vscode.CancellationTokenSource();
      // VS Code asks the person to confirm a tool that is not marked read-only,
      // in a chat. A call made from here has no chat to show it in, so it waits
      // (seen in VS Code 1.140 with an MCP tool). Say so instead of waiting ten
      // minutes.
      const limit = 2 * 60 * 1000;
      let timedOut = false;
      const timer = setTimeout(() => { timedOut = true; cts.cancel(); }, limit);
      try {
        const r = await vscode.lm.invokeTool(req.name, { input: req.input || {}, toolInvocationToken: undefined }, cts.token);
        return { text: resultText(r) };
      } catch (e) {
        if (timedOut) throw new Error(`VS Code did not run ${req.name} within 2 minutes. It is most likely waiting for a person to confirm it in a chat, and no chat is showing the question.`);
        throw e;
      } finally {
        clearTimeout(timer);
      }
    }
  }
  return { error: `unknown request ${req.op}` };
}

function listen() {
  const dir = cacheDir();
  fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
  try { fs.chmodSync(dir, 0o700); } catch {}
  const name = `${process.pid}-${crypto.randomBytes(4).toString("hex")}`;
  socketPath = path.join(dir, `${name}.sock`);
  server = net.createServer((conn) => {
    let buf = "";
    let chain = Promise.resolve();
    conn.on("data", (d) => {
      buf += d.toString("utf8");
      let i;
      while ((i = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, i);
        buf = buf.slice(i + 1);
        // Answer in order: the runner reads one answer per request.
        chain = chain.then(async () => {
          let req;
          try { req = JSON.parse(line); } catch { conn.write(JSON.stringify({ error: "unreadable request" }) + "\n"); return; }
          let res;
          try { res = await answer(req); } catch (e) { res = { error: String((e && e.message) || e) }; }
          conn.write(JSON.stringify({ id: req.id, ...res }) + "\n");
        });
      }
    });
    conn.on("error", () => {});
  });
  server.listen(socketPath);
}

function activate(context) {
  listen();
  context.subscriptions.push({ dispose: () => { if (server) server.close(); try { fs.unlinkSync(socketPath); } catch {} } });

  const changed = new vscode.EventEmitter();
  context.subscriptions.push(vscode.lm.registerMcpServerDefinitionProvider("tap-runtime.servers", {
    onDidChangeMcpServerDefinitions: changed.event,
    provideMcpServerDefinitions: () => [
      new vscode.McpStdioServerDefinition("TAP Runtime", runtimePath(), ["serve", "--vscode-socket", socketPath], {}, "0.1.0"),
    ],
  }));

  const out = vscode.window.createOutputChannel("TAP Runtime");
  context.subscriptions.push(out);

  // Runs a primitive from its folder, outside chat: the same path Copilot
  // takes, for trying a primitive or the editor's tools directly.
  context.subscriptions.push(vscode.commands.registerCommand("tapRuntime.runPrimitive", async () => {
    const pick = await vscode.window.showOpenDialog({ canSelectFolders: true, canSelectFiles: false, openLabel: "Run this primitive" });
    if (!pick || !pick.length) return;
    const pkg = pick[0].fsPath;
    out.show(true);
    out.appendLine(`> tap ${pkg}`);
    execFile(runtimePath(), ["--vscode-socket", socketPath, "--no-record", pkg], { cwd: vscode.workspace.workspaceFolders?.[0]?.uri.fsPath || os.homedir(), maxBuffer: 16 * 1024 * 1024 },
      (err, stdout, stderr) => {
        if (stderr) out.appendLine(stderr.trimEnd());
        if (stdout) out.appendLine(stdout.trimEnd());
        if (err && !stdout) out.appendLine(`The run failed: ${err.message}`);
      });
  }));

  // Where this window's socket is, for a test or a script that starts the
  // runner itself.
  context.subscriptions.push(vscode.commands.registerCommand("tapRuntime.socketPath", () => socketPath));

  context.subscriptions.push(vscode.commands.registerCommand("tapRuntime.listTools", () => {
    out.show(true);
    const tools = vscode.lm.tools;
    out.appendLine(`${tools.length} tool(s) VS Code can call for a primitive:`);
    for (const t of tools) out.appendLine(`  ${t.name}${t.tags && t.tags.length ? "  [" + t.tags.join(", ") + "]" : ""}`);
  }));
}

function deactivate() {
  if (server) server.close();
  try { fs.unlinkSync(socketPath); } catch {}
}

module.exports = { activate, deactivate };
