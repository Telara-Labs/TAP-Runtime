// A TAP runner for a web page. The primitive runs inside QuickJS compiled to
// WebAssembly, with the runner's own guest SDK. Its only way out is the
// request protocol on stdin/stdout, and this host answers a tool call by
// calling a connector: in a claude.ai Artifact, the viewer's own.
//
// Connector calls are asynchronous and the guest reads its answers
// synchronously, so a run is replayed: when the program asks for something
// not yet answered, the run stops, the host makes the call, and the program
// is run again from the start with every answer so far on its stdin. This is
// how the runner resumes a stopped run from its record.

const ENOSYS = 52, EBADF = 8, ESUCCESS = 0;

class Pending extends Error {}
class Exit extends Error { constructor(code) { super("exit " + code); this.code = code; } }

function runOnce(module, argv, stdinBytes) {
  let memory;
  const view = () => new DataView(memory.buffer);
  const bytes = () => new Uint8Array(memory.buffer);
  const out = { 1: [], 2: [] };
  let stdinPos = 0;
  const enc = new TextEncoder();
  const args = argv.map((a) => enc.encode(a + "\0"));

  const sizes = (list, countPtr, sizePtr) => {
    view().setUint32(countPtr, list.length, true);
    view().setUint32(sizePtr, list.reduce((n, a) => n + a.length, 0), true);
    return ESUCCESS;
  };
  const fill = (list, ptrs, buf) => {
    for (const a of list) {
      view().setUint32(ptrs, buf, true); ptrs += 4;
      bytes().set(a, buf); buf += a.length;
    }
    return ESUCCESS;
  };
  const iovs = (ptr, len, fn) => {
    let total = 0;
    for (let i = 0; i < len; i++) {
      const base = view().getUint32(ptr + i * 8, true);
      const n = view().getUint32(ptr + i * 8 + 4, true);
      const done = fn(base, n);
      total += done;
      if (done < n) break;
    }
    return total;
  };

  const wasi = {
    args_sizes_get: (c, s) => sizes(args, c, s),
    args_get: (p, b) => fill(args, p, b),
    environ_sizes_get: (c, s) => sizes([], c, s),
    environ_get: () => ESUCCESS,
    clock_time_get: (_id, _prec, ptr) => { view().setBigUint64(ptr, BigInt(Date.now()) * 1000000n, true); return ESUCCESS; },
    fd_write: (fd, ptr, len, nptr) => {
      if (fd !== 1 && fd !== 2) return EBADF;
      const n = iovs(ptr, len, (b, k) => { out[fd].push(bytes().slice(b, b + k)); return k; });
      view().setUint32(nptr, n, true);
      return ESUCCESS;
    },
    fd_read: (fd, ptr, len, nptr) => {
      if (fd !== 0) return EBADF;
      if (stdinPos >= stdinBytes.length) throw new Pending();
      const n = iovs(ptr, len, (b, k) => {
        const chunk = stdinBytes.subarray(stdinPos, stdinPos + k);
        bytes().set(chunk, b); stdinPos += chunk.length; return chunk.length;
      });
      view().setUint32(nptr, n, true);
      return ESUCCESS;
    },
    fd_fdstat_get: (fd, ptr) => {
      if (fd > 2) return EBADF;
      bytes().fill(0, ptr, ptr + 24);
      view().setUint8(ptr, 2); // character device
      return ESUCCESS;
    },
    fd_fdstat_set_flags: () => ESUCCESS,
    fd_prestat_get: () => EBADF, // nothing is mounted: no file system at all
    fd_prestat_dir_name: () => EBADF,
    fd_close: () => ESUCCESS,
    fd_seek: () => ENOSYS,
    fd_readdir: () => ENOSYS,
    path_open: () => ENOSYS,
    path_create_directory: () => ENOSYS,
    path_filestat_get: () => ENOSYS,
    path_filestat_set_times: () => ENOSYS,
    path_remove_directory: () => ENOSYS,
    path_rename: () => ENOSYS,
    path_unlink_file: () => ENOSYS,
    poll_oneoff: () => ENOSYS,
    proc_exit: (code) => { throw new Exit(code); },
  };

  const instance = new WebAssembly.Instance(module, { wasi_snapshot_preview1: wasi });
  memory = instance.exports.memory;
  let pending = false, exit = 0;
  try {
    instance.exports._start();
  } catch (e) {
    if (e instanceof Pending) pending = true;
    else if (e instanceof Exit) exit = e.code;
    else throw e;
  }
  const dec = new TextDecoder();
  const join = (parts) => dec.decode(parts.reduce((a, p) => { const c = new Uint8Array(a.length + p.length); c.set(a); c.set(p, a.length); return c; }, new Uint8Array()));
  return { stdout: join(out[1]), stderr: join(out[2]), pending, exit };
}

// runPrimitive runs one primitive to completion.
//   module    a compiled QuickJS WebAssembly.Module
//   prelude   the runner's JavaScript guest SDK, with one %s for the program
//   script    the primitive's program
//   args      the arguments given to the program, as the runner gives them
//   bindings  alias -> {server, tool}: what each declared tool is bound to
//   callTool  async (server, tool, args) -> result object
//   onEvent   called with a line for each request, for the page's log
export async function runPrimitive({ module, prelude, script, args = [], bindings, callTool, onEvent = () => {}, maxRuns = 200 }) {
  const argv = ["qjs", "--module", "-e", prelude.replace("%s", JSON.stringify(script)), ...args.map(String)];
  const answers = [];
  const enc = new TextEncoder();
  let calls = 0, refused = 0;
  for (let runs = 1; runs <= maxRuns; runs++) {
    const stdin = enc.encode(answers.map((a) => JSON.stringify(a) + "\n").join(""));
    const r = runOnce(module, argv, stdin);
    const lines = r.stdout.split("\n").filter(Boolean).map((l) => JSON.parse(l));
    const done = lines.find((l) => l.method === "return");
    if (done) return { stdout: done.stdout, stderr: done.stderr, exit: done.exit, calls, refused, runs };
    if (!r.pending) return { stdout: "", stderr: r.stderr || "the primitive ended without a result", exit: r.exit || 1, calls, refused, runs };
    // Every request past the answers already given is new. call_many sends
    // several before reading, and they are answered together, in order.
    const fresh = lines.filter((l) => l.id).slice(answers.length);
    for (const rq of fresh) {
      if (rq.method === "tools") {
        answers.push({ id: rq.id, tools: Object.keys(bindings) });
      } else if (rq.method === "call") {
        const b = bindings[rq.alias];
        if (!b) {
          refused++;
          onEvent(`REFUSED  ${rq.alias}: not declared`);
          answers.push({ id: rq.id, refused: `tool ${rq.alias} is not declared in primitive.yaml` });
          continue;
        }
        onEvent(`call     ${rq.alias} -> ${b.server} / ${b.tool}`);
        try {
          const res = await callTool(b.server, b.tool, rq.arguments || {});
          calls++;
          answers.push({ id: rq.id, result: JSON.stringify(res.payload ?? res) });
        } catch (e) {
          answers.push({ id: rq.id, exit: 1, stderr: `${e.code || "error"}: ${e.message || e}` });
        }
      } else {
        // The browser has no machine: no host programs, files or fetch.
        refused++;
        onEvent(`REFUSED  ${rq.method}: not available in a web page`);
        answers.push({ id: rq.id, refused: `${rq.method} is not available when a primitive runs in a web page` });
      }
    }
  }
  return { stdout: "", stderr: `stopped after ${maxRuns} runs`, exit: 1, calls, refused, runs: maxRuns };
}
