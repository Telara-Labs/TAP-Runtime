// TAP relay for OpenCode and Kilo. It runs inside the person's own session
// and lets the TAP runner that session started run a tool on an MCP server
// the person configured, with no model turn and no change to the client.
//
// The session's server has no route that runs a tool for another program,
// so the relay opens its own connection to the server, from the session's
// resolved configuration (GET /config, through the session's own client)
// and, for a server the person signed in to, the token the client stored.
// Those secrets stay in this process: the runner gets server names, status,
// tool lists and results only.
//
// It listens on a Unix socket in a directory only this user can open, and
// answers: configuration (without headers, environment or tokens), MCP
// status, health, each server's tools, and call-tool. Listing tools and
// calling one are let through only while a TAP run is in progress in this session:
// from the start of a tap_run or tap_result call until ten minutes after the
// last one ended.
import fs from "node:fs"
import http from "node:http"
import os from "node:os"
import path from "node:path"
import crypto from "node:crypto"
import { spawn } from "node:child_process"

const LEASE_MS = 10 * 60 * 1000
const CALL_MS = 120 * 1000
const PROTOCOL = "2025-06-18"

export const TapRelay = async (input) => {
  if (process.platform === "win32" || typeof process.getuid !== "function") return {}
  const uid = process.getuid()
  const dir = `/tmp/tap-relay-${uid}`
  try {
    fs.mkdirSync(dir, { recursive: true, mode: 0o700 })
    const st = fs.lstatSync(dir)
    if (!st.isDirectory() || st.uid !== uid) return {}
    fs.chmodSync(dir, 0o700)
  } catch {
    return {}
  }
  const instance = crypto.createHash("sha256").update(String(input.directory)).digest("hex").slice(0, 12)
  const base = path.join(dir, `${process.pid}-${instance}`)
  const sock = base + ".sock"
  const meta = base + ".json"

  let running = 0
  let leaseUntil = 0
  const inRun = () => running > 0 || Date.now() < leaseUntil
  const isTapCall = (tool) => /(^|_)tap_(run|result)$/.test(String(tool || ""))

  // The session's own server, through the client the session gave us.
  const cfg = input.client?._client?.getConfig?.() ?? {}
  const doFetch = cfg.fetch ?? fetch
  const baseUrl = String(cfg.baseUrl ?? input.serverUrl ?? "http://localhost:4096").replace(/\/$/, "")
  const sessionHeaders = {}
  if (cfg.headers instanceof Headers) cfg.headers.forEach((v, k) => (sessionHeaders[k] = v))
  else Object.assign(sessionHeaders, cfg.headers ?? {})
  const session = async (route) => {
    const r = await doFetch(new Request(baseUrl + route, { headers: sessionHeaders }))
    if (!r.ok) throw new Error(`${route}: ${r.status}`)
    return r.json()
  }

  // The token the client stored for a server it signed in to, matched by URL.
  const storedToken = (name, url) => {
    const dataHome = process.env.XDG_DATA_HOME || path.join(os.homedir(), ".local", "share")
    for (const app of ["kilo", "opencode"]) {
      try {
        const all = JSON.parse(fs.readFileSync(path.join(dataHome, app, "mcp-auth.json"), "utf8"))
        const e = all[name]
        if (!e?.tokens?.accessToken || (e.serverUrl && e.serverUrl !== url)) continue
        if (e.tokens.expiresAt && e.tokens.expiresAt * 1000 < Date.now()) return { expired: true }
        return { token: e.tokens.accessToken }
      } catch {}
    }
    return {}
  }

  const conns = new Map()
  const connect = async (name) => {
    if (conns.has(name)) return conns.get(name)
    const conf = (await session("/config")).mcp?.[name]
    if (!conf || conf.enabled === false) throw new Error(`no enabled MCP server named ${name} in this session's configuration`)
    const c = conf.type === "remote" ? remoteConn(name, conf) : localConn(conf)
    await c.request("initialize", { protocolVersion: PROTOCOL, capabilities: {}, clientInfo: { name: "tap-relay", version: "1" } })
    c.notify("notifications/initialized", {})
    conns.set(name, c)
    return c
  }

  const localConn = (conf) => {
    const [cmd, ...args] = conf.command
    const child = spawn(cmd, args, { cwd: input.directory, env: { ...process.env, ...(conf.environment ?? {}) }, stdio: ["pipe", "pipe", "ignore"] })
    const pending = new Map()
    let buf = ""
    let next = 1
    child.stdout.on("data", (d) => {
      buf += d
      let i
      while ((i = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, i).trim()
        buf = buf.slice(i + 1)
        if (!line) continue
        let m
        try { m = JSON.parse(line) } catch { continue }
        const p = m.id !== undefined && m.method === undefined ? pending.get(m.id) : undefined
        if (p) { pending.delete(m.id); m.error ? p.reject(new Error(m.error.message ?? JSON.stringify(m.error))) : p.resolve(m.result) }
      }
    })
    child.on("exit", () => { for (const p of pending.values()) p.reject(new Error("the MCP server exited")); pending.clear() })
    return {
      request: (method, params) => new Promise((resolve, reject) => {
        const id = next++
        pending.set(id, { resolve, reject })
        child.stdin.write(JSON.stringify({ jsonrpc: "2.0", id, method, params }) + "\n")
        setTimeout(() => { if (pending.delete(id)) reject(new Error(`${method}: no answer`)) }, CALL_MS)
      }),
      notify: (method, params) => child.stdin.write(JSON.stringify({ jsonrpc: "2.0", method, params }) + "\n"),
      close: () => child.kill(),
    }
  }

  const remoteConn = (name, conf) => {
    const headers = { "content-type": "application/json", accept: "application/json, text/event-stream", ...(conf.headers ?? {}) }
    if (!Object.keys(headers).some((k) => k.toLowerCase() === "authorization")) {
      const t = storedToken(name, conf.url)
      if (t.expired) throw new Error(`the sign-in to ${name} has expired; sign in to it again in this client`)
      if (t.token) headers.authorization = `Bearer ${t.token}`
    }
    let sessionId
    let next = 1
    const post = async (body) => {
      const h = { ...headers }
      if (sessionId) h["mcp-session-id"] = sessionId
      const r = await fetch(conf.url, { method: "POST", headers: h, body: JSON.stringify(body), signal: AbortSignal.timeout(CALL_MS) })
      sessionId = r.headers.get("mcp-session-id") ?? sessionId
      if (r.status === 401) throw new Error(`${name} needs a sign-in; sign in to it in this client`)
      if (!r.ok && r.status !== 202) throw new Error(`${name}: HTTP ${r.status}`)
      const text = await r.text()
      if ((r.headers.get("content-type") ?? "").includes("text/event-stream")) {
        const datas = text.split(/\r?\n/).filter((l) => l.startsWith("data:")).map((l) => l.slice(5).trim())
        for (const d of datas) {
          try { const m = JSON.parse(d); if (m.id === body.id) return m } catch {}
        }
        return undefined
      }
      return text ? JSON.parse(text) : undefined
    }
    return {
      request: async (method, params) => {
        const m = await post({ jsonrpc: "2.0", id: next++, method, params })
        if (!m) throw new Error(`${method}: no answer`)
        if (m.error) throw new Error(m.error.message ?? JSON.stringify(m.error))
        return m.result
      },
      notify: (method, params) => { post({ jsonrpc: "2.0", method, params }).catch(() => {}) },
      close: () => {},
    }
  }

  // The configuration the runner may see: no headers, environment or tokens.
  const publicConfig = (c) => {
    const mcp = {}
    for (const [name, s] of Object.entries(c.mcp ?? {})) {
      mcp[name] = { type: s.type, enabled: s.enabled, ...(s.url ? { url: s.url } : {}), ...(s.command ? { command: s.command } : {}) }
    }
    return { mcp, tools: c.tools, permission: c.permission }
  }

  const send = (res, code, body) => {
    res.writeHead(code, { "content-type": "application/json" })
    res.end(JSON.stringify(body))
  }

  const server = http.createServer(async (req, res) => {
    const route = `${req.method} ${String(req.url).split("?")[0]}`
    try {
      switch (route) {
        case "GET /global/health":
          return send(res, 200, await session("/global/health"))
        case "GET /mcp":
          return send(res, 200, await session("/mcp"))
        case "GET /config":
          return send(res, 200, publicConfig(await session("/config")))
        case "GET /tap/tools": {
          // Each enabled server's tools, with the annotations and schemas
          // the server gives, through this relay's own connections.
          if (!inRun()) return send(res, 409, { error: "no TAP run is in progress in this session" })
          const conf = (await session("/config")).mcp ?? {}
          const out = { servers: {}, unavailable: {} }
          for (const [name, s] of Object.entries(conf)) {
            if (s.enabled === false) continue
            try {
              const c = await connect(name)
              const tools = []
              let cursor
              for (let page = 0; page < 20; page++) {
                const r = await c.request("tools/list", cursor ? { cursor } : {})
                tools.push(...(r?.tools ?? []).map((t) => ({ name: t.name, description: t.description, inputSchema: t.inputSchema, annotations: t.annotations })))
                cursor = r?.nextCursor
                if (!cursor) break
              }
              out.servers[name] = tools
            } catch (e) {
              out.unavailable[name] = String(e?.message ?? e)
            }
          }
          return send(res, 200, out)
        }
        case "POST /experimental/mcp/call-tool": {
          if (!inRun()) return send(res, 409, { error: "no TAP run is in progress in this session" })
          const chunks = []
          for await (const c of req) chunks.push(c)
          const body = JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}")
          const c = await connect(String(body.server))
          const result = await c.request("tools/call", { name: String(body.name), arguments: body.arguments ?? {} })
          return send(res, 200, { content: result?.content ?? [], ...(result?.isError ? { isError: true } : {}), ...(result?.structuredContent ? { structuredContent: result.structuredContent } : {}) })
        }
      }
      return send(res, 404, { error: "not a TAP relay route" })
    } catch (e) {
      return send(res, 502, { error: String(e?.message ?? e) })
    }
  })
  try { fs.rmSync(sock, { force: true }) } catch {}
  await new Promise((resolve) => server.listen(sock, resolve))
  fs.chmodSync(sock, 0o600)
  fs.writeFileSync(meta, JSON.stringify({ pid: process.pid, directory: input.directory, worktree: input.worktree, socket: sock }), { mode: 0o600 })
  const cleanup = () => {
    for (const c of conns.values()) c.close()
    conns.clear()
    try { fs.rmSync(sock, { force: true }) } catch {}
    try { fs.rmSync(meta, { force: true }) } catch {}
  }
  process.once("exit", cleanup)

  return {
    "tool.execute.before": async (call) => {
      if (isTapCall(call?.tool)) running++
    },
    "tool.execute.after": async (call) => {
      if (isTapCall(call?.tool)) {
        running = Math.max(0, running - 1)
        leaseUntil = Date.now() + LEASE_MS
      }
    },
    dispose: async () => {
      server.close()
      cleanup()
    },
  }
}
