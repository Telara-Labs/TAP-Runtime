"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { EventEmitter } = require("node:events");

const source = fs.readFileSync(path.join(__dirname, "extension.js"), "utf8");

function deferred() {
  let resolve, reject;
  const promise = new Promise((a, b) => { resolve = a; reject = b; });
  return { promise, resolve, reject };
}

function harness(invokeTool) {
  const timers = [], tokens = [];
  let onConnection;
  class TextPart { constructor(value) { this.value = value; } }
  class CancellationTokenSource {
    constructor() {
      this.listeners = [];
      this.token = {
        isCancellationRequested: false,
        onCancellationRequested: (fn) => { this.listeners.push(fn); return { dispose() {} }; },
      };
      this.cancelled = 0;
      this.disposed = 0;
      tokens.push(this);
    }
    cancel() { this.cancelled++; this.token.isCancellationRequested = true; for (const fn of this.listeners) fn(); }
    dispose() { this.disposed++; }
  }
  const vscode = {
    version: "1.140.0", CancellationTokenSource, LanguageModelTextPart: TextPart,
    lm: { invokeTool, tools: [] },
    workspace: { getConfiguration: () => ({ get: () => ({}) }) },
  };
  const ctx = vm.createContext({
    require(name) {
      if (name === "vscode") return vscode;
      if (name === "net") return { createServer(fn) { onConnection = fn; return { listen() {}, close() {} }; } };
      if (name === "fs") return { mkdirSync() {}, chmodSync() {} };
      return require(name);
    },
    module: { exports: {} }, process, Buffer,
    setTimeout(fn, delay) { const t = { fn, delay, cleared: false }; timers.push(t); return t; },
    clearTimeout(t) { t.cleared = true; },
  });
  vm.runInContext(source, ctx, { filename: "extension.js" });
  return {
    answer: vm.runInContext("answer", ctx), timers, tokens, TextPart,
    connect() {
      vm.runInContext("listen()", ctx);
      const conn = new EventEmitter();
      conn.replies = [];
      conn.write = (line) => conn.replies.push(JSON.parse(line));
      onConnection(conn);
      return conn;
    },
  };
}

test("a completed invocation preserves its input and result and releases resources", async () => {
  let h;
  h = harness(async (name, options, token) => {
    assert.equal(name, "read_file");
    assert.deepEqual(options.input, { path: "owned.txt" });
    assert.equal(options.toolInvocationToken, undefined);
    assert.equal(token.isCancellationRequested, false);
    return { content: [new h.TextPart("read result")] };
  });
  const r = await h.answer({ op: "call", name: "read_file", input: { path: "owned.txt" } });
  assert.equal(r.text, "read result");
  assert.equal(h.timers[0].delay, 120000);
  assert.equal(h.timers[0].cleared, true);
  assert.equal(h.tokens[0].cancelled, 0);
  assert.equal(h.tokens[0].disposed, 1);
});

for (const synchronous of [true, false]) {
  test(`an invocation ${synchronous ? "throw" : "rejection"} before the deadline preserves its error`, async () => {
    const error = new Error("actual tool failure");
    const h = harness(() => { if (synchronous) throw error; return Promise.reject(error); });
    await assert.rejects(h.answer({ op: "call", name: "read_file" }), (e) => e === error);
    assert.equal(h.timers[0].cleared, true);
    assert.equal(h.tokens[0].disposed, 1);
    assert.equal(h.tokens[0].cancelled, 0);
  });
}

test("the two-minute answer settles even when invokeTool never honors cancellation", async () => {
  const h = harness(() => new Promise(() => {}));
  const pending = h.answer({ op: "call", name: "write_file" });
  h.timers[0].fn();
  await assert.rejects(pending, /within 2 minutes.*cancelled.*already running.*before retrying/);
  assert.equal(h.tokens[0].token.isCancellationRequested, true);
  assert.equal(h.tokens[0].cancelled, 1);
  assert.equal(h.tokens[0].disposed, 1);
  assert.equal(h.timers[0].cleared, true);
});

test("an SDK cancellation rejection cannot replace the deadline's explanation", async () => {
  const h = harness((_, __, token) => new Promise((resolve, reject) => {
    token.onCancellationRequested(() => reject(new Error("generic SDK cancellation")));
  }));
  const pending = h.answer({ op: "call", name: "write_file" });
  h.timers[0].fn();
  await assert.rejects(pending, /within 2 minutes.*cancelled/);
  assert.equal(h.tokens[0].disposed, 1);
});

test("a tool already executing may ignore cancellation, so the timeout never claims it did not run", async () => {
  const finish = deferred();
  let writes = 0;
  const h = harness(async () => {
    await finish.promise;
    writes++; // A provider that ignores the token is outside TAP's control.
    return { content: [] };
  });
  const pending = h.answer({ op: "call", name: "write_file" });
  h.timers[0].fn();
  await assert.rejects(pending, (e) => {
    assert.match(e.message, /already running/);
    assert.doesNotMatch(e.message, /did not run/);
    return true;
  });
  finish.resolve();
  await new Promise(setImmediate);
  assert.equal(writes, 1);
});

test("an approval arriving after timeout cannot dispatch through the VS Code pre-dispatch token guard", async () => {
  const confirmation = deferred();
  let writes = 0;
  const h = harness(async (_, __, token) => {
    await confirmation.promise;
    // The same ordering as VS Code 1.140's LanguageModelToolsService:
    // await dialog.confirm, check token, then invoke the implementation.
    if (token.isCancellationRequested) throw new Error("cancelled before dispatch");
    writes++;
    return { content: [] };
  });
  const pending = h.answer({ op: "call", name: "write_file" });
  h.timers[0].fn();
  await assert.rejects(pending, /within 2 minutes/);
  confirmation.resolve(true);
  await new Promise(setImmediate);
  assert.equal(writes, 0);
  assert.equal(h.tokens[0].token.isCancellationRequested, true);
});

for (const lateReject of [false, true]) {
  test(`a late SDK ${lateReject ? "rejection" : "result"} cannot replace the timeout response or stall the socket queue`, async () => {
    const late = deferred();
    const h = harness(() => late.promise);
    const conn = h.connect();
    conn.emit("data", Buffer.from('{"id":1,"op":"call","name":"write_file"}\n{"id":2,"op":"hello"}\n'));
    await new Promise(setImmediate);
    assert.equal(conn.replies.length, 0);
    h.timers[0].fn();
    await new Promise(setImmediate);
    assert.equal(conn.replies.length, 2);
    assert.equal(conn.replies[0].id, 1);
    assert.match(conn.replies[0].error, /within 2 minutes/);
    assert.equal(conn.replies[1].id, 2);
    assert.equal(conn.replies[1].version, "1.140.0");
    if (lateReject) late.reject(new Error("late SDK failure"));
    else late.resolve({ content: [new h.TextPart("late success")] });
    await new Promise(setImmediate);
    assert.equal(conn.replies.length, 2);
    assert.equal(h.tokens[0].disposed, 1);
  });
}
