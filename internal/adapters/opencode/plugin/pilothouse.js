// Pilothouse for OpenCode — https://github.com/xasdta/pilothouse
// @pilothouse-managed: `pilothouse hooks install` writes this file and
// `pilothouse hooks uninstall` removes it. Local edits are overwritten.
//
// The plugin forwards OpenCode's tool calls, permission prompts, questions
// and session events to the Pilothouse daemon over its Unix socket, applies
// the daemon's answers through OpenCode's in-process client, and delivers
// the replies a person sends from the Pilothouse inbox. When the daemon is
// down it stays out of the way: OpenCode behaves as if the plugin were not
// installed (unless approvals.fail_closed is on in Pilothouse's config).
//
// docs/AGENTS.md (OpenCode) describes the protocol.

import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

// PROTOCOL is the payload version the daemon's adapter reads.
const PROTOCOL = 1;

const TOKEN_HEADER = "X-Pilothouse-Plugin";
const SHORT_TIMEOUT = 2000; // events and policy: the daemon answers in milliseconds
const MAX_SHORT_TIMEOUT = 20000; // big payloads (a large write) get 1 s more per MB
const HOLD_TIMEOUT = 65 * 60 * 1000; // a held request; the daemon ends holds itself
const POLL_WAIT = 25; // seconds the daemon keeps a command poll open
const MAX_BODY = 32 << 20; // larger hooks are treated like a daemon that is down
const MAX_TEXT = 4096; // last assistant message kept per session
const MAX_QUEUE = 256; // events waiting for the daemon; more are dropped

const FAIL_CLOSED_REASON =
  "Pilothouse is not running and approvals.fail_closed is on, so tool calls are blocked. Start it with `pilothouse start`, or set approvals.fail_closed: false.";
const DEFAULT_DENY = "Blocked by Pilothouse policy.";
const ASK_UNANSWERED =
  "Pilothouse: a person must approve this, and nobody did — ask the user to approve it in Pilothouse (inbox, phone or `pilothouse approve`), or to run it themselves";

const test = globalThis.__PILOTHOUSE_TEST__ || null;
const env = (test && test.env) || process.env;

// ---------- transport ----------

function socketPath() {
  const home = env.PILOTHOUSE_HOME || path.join(os.homedir(), ".pilothouse");
  return path.join(env.PILOTHOUSE_RUN_DIR || path.join(home, "run"), "pilothouse.sock");
}

// One random token per OpenCode process. It binds this process's sessions
// to its command channel: the daemon hands a session's replies only to the
// channel whose token first reported that session. It is only ever sent to
// the daemon's socket.
const TOKEN = crypto.randomBytes(32).toString("base64url");

async function bunTransport(method, pathname, body, signal) {
  const res = await fetch("http://pilothouse" + pathname, {
    method,
    unix: socketPath(),
    headers: { "Content-Type": "application/json", [TOKEN_HEADER]: TOKEN },
    body,
    signal,
  });
  return { status: res.status, text: await res.text() };
}

const transport = (test && test.transport) || bunTransport;

// call sends one request to the daemon and resolves to {status, text}; it
// rejects when the daemon can't be reached in time.
function call(method, pathname, body, { timeout = SHORT_TIMEOUT, signal } = {}) {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(new Error("pilothouse: timed out")), timeout);
  timer.unref?.();
  const onAbort = () => ctrl.abort(signal.reason);
  if (signal) {
    if (signal.aborted) ctrl.abort(signal.reason);
    else signal.addEventListener("abort", onAbort, { once: true });
  }
  return transport(method, pathname, body, ctrl.signal).finally(() => {
    clearTimeout(timer);
    signal?.removeEventListener("abort", onAbort);
  });
}

const ANCESTORS = [{ pid: process.pid, ppid: process.ppid, comm: path.basename(process.execPath || "opencode") }];
const ENV_KEYS = ["TERM_PROGRAM", "TMUX", "PILOTHOUSE_SESSION_ID", "PILOTHOUSE_TASK_ID", "SSH_CONNECTION"];

function allowedEnv() {
  const out = {};
  for (const k of ENV_KEYS) if (env[k]) out[k] = env[k];
  return out;
}

// hook sends one event to the daemon and resolves to its answer (an
// object, {} for "no opinion"). It rejects when the daemon is unreachable,
// slow or the payload is too large to check.
async function hook(event, data, opts = {}) {
  const body = JSON.stringify({
    agent: "opencode",
    event,
    payload: { v: PROTOCOL, event, ...data },
    shim_pid: process.pid,
    ancestors: ANCESTORS,
    cwd: data.directory || "",
    env: allowedEnv(),
    pilothouse_session_id: env.PILOTHOUSE_SESSION_ID || undefined,
    received_at: new Date().toISOString(),
  });
  if (body.length > MAX_BODY) throw new Error("pilothouse: hook payload too large to check");
  const timeout = opts.timeout || Math.min(SHORT_TIMEOUT + Math.floor(body.length / (1 << 20)) * 1000, MAX_SHORT_TIMEOUT);
  const res = await call("POST", "/v1/plugin/hook", body, { timeout, signal: opts.signal });
  if (res.status !== 200) throw new Error("pilothouse: daemon answered " + res.status);
  const resp = JSON.parse(res.text || "{}");
  if (!resp.stdout) return {};
  return JSON.parse(resp.stdout);
}

// ---------- fail closed ----------

let failClosedCache = { at: 0, value: false };

// failClosed reads approvals.fail_closed from Pilothouse's config.yaml, the
// way the hook shim does when the daemon is down. Unreadable means false.
function failClosed() {
  const now = Date.now();
  if (now - failClosedCache.at < 10000) return failClosedCache.value;
  let value = false;
  try {
    const home = env.PILOTHOUSE_HOME || path.join(os.homedir(), ".pilothouse");
    value = yamlFailClosed(fs.readFileSync(path.join(home, "config.yaml"), "utf8"));
  } catch {}
  failClosedCache = { at: now, value };
  return value;
}

// yamlFailClosed finds `fail_closed: true` under the top-level `approvals:`
// key (block or flow style).
function yamlFailClosed(text) {
  const truthy = (v) => /^["']?(true|yes|on)["']?$/i.test(v.trim());
  let inApprovals = false;
  for (const raw of String(text).split(/\r?\n/)) {
    const line = raw.replace(/(^|\s)#.*$/, "");
    if (!line.trim()) continue;
    const indent = line.length - line.trimStart().length;
    if (indent === 0) {
      const flow = line.match(/^approvals\s*:\s*\{(.*)\}\s*$/);
      if (flow) {
        const m = flow[1].match(/(?:^|,)\s*fail_closed\s*:\s*([^,}]+)/);
        if (m) return truthy(m[1]);
      }
      inApprovals = /^approvals\s*:\s*$/.test(line);
      continue;
    }
    if (inApprovals) {
      const m = line.trim().match(/^fail_closed\s*:\s*(.+)$/);
      if (m) return truthy(m[1]);
    }
  }
  return false;
}

// ---------- bounded state ----------

class Bounded extends Map {
  constructor(max) {
    super();
    this.max = max;
  }
  set(k, v) {
    if (this.has(k)) this.delete(k);
    super.set(k, v);
    while (this.size > this.max) this.delete(this.keys().next().value);
    return this;
  }
}

const sessions = new Bounded(1000); // OpenCode session id → {client, directory}
const parents = new Bounded(2000); // child (subagent) session id → parent id
const calls = new Bounded(500); // tool call id → {tool, args}, for permission prompts
const roles = new Bounded(2000); // message id → role
const lastText = new Bounded(1000); // session id → last assistant text this turn
const lastError = new Bounded(1000); // session id → error this turn
const titles = new Bounded(1000); // session id → title last reported
const holds = new Map(); // permission/question id → AbortController of its held call
const instances = new Set();

// root maps a subagent's session to the session a person sees: OpenCode
// runs the task tool's subagents in child sessions.
function root(id) {
  let cur = id;
  for (let i = 0; i < 16 && parents.has(cur); i++) cur = parents.get(cur);
  return cur;
}

function remember(inst, id, directory) {
  if (!id) return;
  const prev = sessions.get(id);
  sessions.set(id, { client: inst.client, directory: directory || prev?.directory || inst.directory });
}

function directoryOf(inst, id) {
  return sessions.get(id)?.directory || inst.directory || "";
}

// ---------- event queue ----------

// Events reach the daemon in order: each waits for the one before it.
let chain = Promise.resolve();
let queued = 0;

function enqueue(fn) {
  if (queued >= MAX_QUEUE) return chain;
  queued++;
  chain = chain.then(fn).catch(() => {}).finally(() => queued--);
  return chain;
}

// afterQueue runs fn once the events queued before it were sent, without
// holding up the events queued after it (used for held requests).
function afterQueue(fn, maxWait = 1000) {
  const ready = Promise.race([chain, sleep(maxWait)]);
  return ready.then(fn).catch(() => {});
}

function sleep(ms) {
  return new Promise((resolve) => {
    const t = setTimeout(resolve, ms);
    t.unref?.();
  });
}

function send(event, data) {
  enqueue(() => hook(event, data));
}

// ---------- applying answers ----------

function errText(e) {
  if (!e) return "";
  if (typeof e === "string") return e;
  return e.message || e.data?.message || e.name || JSON.stringify(e).slice(0, 200);
}

// post calls an OpenCode server route through the plugin's in-process
// client (OpenCode's TUI has no listening server for plain fetch).
async function post(client, url, pathParams, body) {
  const raw = client?._client;
  if (!raw?.post) throw new Error("this OpenCode client cannot call " + url);
  const r = await raw.post({ url, path: pathParams, body, headers: { "Content-Type": "application/json" } });
  if (r?.error) throw new Error(errText(r.error));
  return r?.data;
}

async function replyPermission(inst, p, reply, message) {
  const client = sessions.get(p.sessionID)?.client || inst.client;
  try {
    await post(client, "/permission/{requestID}/reply", { requestID: p.id }, message ? { reply, message } : { reply });
    return;
  } catch {}
  // Older servers: the per-session route (no feedback message).
  try {
    await client.postSessionIdPermissionsPermissionId({
      path: { id: p.sessionID, permissionID: p.id },
      body: { response: reply },
    });
  } catch {}
}

async function replyQuestion(inst, q, answers) {
  const client = sessions.get(q.sessionID)?.client || inst.client;
  try {
    await post(client, "/question/{requestID}/reply", { requestID: q.id }, { answers });
  } catch {}
}

async function rejectQuestion(inst, q) {
  const client = sessions.get(q.sessionID)?.client || inst.client;
  try {
    await post(client, "/question/{requestID}/reject", { requestID: q.id }, {});
  } catch {}
}

function toast(inst, message, variant = "info") {
  try {
    const r = inst.client?.tui?.showToast?.({ body: { title: "Pilothouse", message, variant } });
    r?.catch?.(() => {});
  } catch {}
}

// ---------- hooks ----------

async function preTool(inst, input, output) {
  const sid = input.sessionID;
  remember(inst, sid);
  calls.set(input.callID, { tool: input.tool, args: output.args });
  const data = {
    session_id: root(sid),
    agent_session_id: sid,
    directory: directoryOf(inst, sid),
    tool: input.tool,
    call_id: input.callID,
    args: output.args,
  };
  await Promise.race([chain, sleep(1000)]);
  let out;
  try {
    out = await hook("tool.execute.before", data);
  } catch {
    if (failClosed()) throw new Error(FAIL_CLOSED_REASON);
    return;
  }
  if (out.action === "deny") throw new Error(out.message || DEFAULT_DENY);
  if (out.action !== "ask") return;

  // OpenCode can't show a prompt for a plugin, so a policy "ask" is held
  // here and routed to a person through Pilothouse, at the desk too.
  toast(inst, "Waiting for a person to approve in Pilothouse: " + (out.summary || input.tool), "warning");
  let v = null;
  try {
    v = await hook("pilothouse.ask", { ...data, rule: out.rule || "", message: out.message || "" }, { timeout: HOLD_TIMEOUT });
  } catch {}
  if (v?.action === "allow") return;
  if (v?.action === "deny") throw new Error(v.message || "Denied in Pilothouse.");
  const why = (out.message || "").trim().replace(/[.\s]+$/, "");
  throw new Error(ASK_UNANSWERED + (why ? " (" + why + ")" : "") + ".");
}

function onPermissionAsked(inst, p) {
  if (!p?.id) return;
  remember(inst, p.sessionID);
  const call = calls.get(p.tool?.callID);
  const ctrl = new AbortController();
  holds.set(p.id, ctrl);
  afterQueue(async () => {
    let out = null;
    try {
      out = await hook(
        "permission.asked",
        {
          session_id: root(p.sessionID),
          agent_session_id: p.sessionID,
          directory: directoryOf(inst, p.sessionID),
          request_id: p.id,
          permission: { permission: p.permission, patterns: p.patterns, metadata: p.metadata, always: p.always },
          tool: call?.tool || "",
          call_id: p.tool?.callID || "",
          args: call?.args,
        },
        { timeout: HOLD_TIMEOUT, signal: ctrl.signal },
      );
    } catch {}
    if (holds.get(p.id) !== ctrl) return; // answered in OpenCode meanwhile
    holds.delete(p.id);
    if (out?.action === "allow") await replyPermission(inst, p, "once");
    else if (out?.action === "deny") await replyPermission(inst, p, "reject", out.message || "Denied in Pilothouse.");
  });
}

function onQuestionAsked(inst, q) {
  if (!q?.id) return;
  remember(inst, q.sessionID);
  const ctrl = new AbortController();
  holds.set(q.id, ctrl);
  afterQueue(async () => {
    let out = null;
    try {
      out = await hook(
        "question.asked",
        {
          session_id: root(q.sessionID),
          agent_session_id: q.sessionID,
          directory: directoryOf(inst, q.sessionID),
          request_id: q.id,
          questions: q.questions,
          call_id: q.tool?.callID || "",
        },
        { timeout: HOLD_TIMEOUT, signal: ctrl.signal },
      );
    } catch {}
    if (holds.get(q.id) !== ctrl) return;
    holds.delete(q.id);
    if (out?.action === "answer" && Array.isArray(out.answers)) await replyQuestion(inst, q, out.answers);
    else if (out?.action === "deny") await rejectQuestion(inst, q);
  });
}

// onAnswered: the person answered OpenCode's own dialog. The daemon closes
// the request; a call still held for it is then released (or dropped).
function onAnswered(inst, event, p) {
  const id = p.requestID || p.id;
  if (!id) return;
  const ctrl = holds.get(id);
  holds.delete(id);
  const reply = event === "permission.replied" ? p.reply : event === "question.rejected" ? "reject" : "answer";
  enqueue(async () => {
    try {
      await hook(event, {
        session_id: root(p.sessionID),
        agent_session_id: p.sessionID,
        directory: directoryOf(inst, p.sessionID),
        request_id: id,
        reply: reply || "",
      });
    } finally {
      // The daemon released the hold; this only frees a call that lost
      // its race with the answer.
      setTimeout(() => ctrl?.abort(new Error("answered in OpenCode")), 2000).unref?.();
    }
  });
}

function onEvent(inst, event) {
  const p = event?.properties || {};
  switch (event?.type) {
    case "message.part.updated": {
      const part = p.part;
      if (part?.type === "text" && !part.synthetic && !part.ignored && roles.get(part.messageID) === "assistant") {
        lastText.set(part.sessionID, String(part.text || "").slice(-MAX_TEXT));
      }
      return;
    }
    case "message.updated":
      if (p.info?.id) roles.set(p.info.id, p.info.role);
      return;
    case "session.created":
    case "session.updated": {
      const info = p.info || {};
      if (!info.id) return;
      if (info.parentID) parents.set(info.id, info.parentID);
      remember(inst, info.id, info.directory);
      if (info.parentID) return; // subagents report through their parent
      const data = { session_id: info.id, agent_session_id: info.id, directory: directoryOf(inst, info.id), title: info.title || "" };
      if (event.type === "session.created") {
        titles.set(info.id, info.title || "");
        send("session.created", data);
      } else if (info.title && titles.get(info.id) !== info.title) {
        titles.set(info.id, info.title);
        send("session.updated", data);
      }
      return;
    }
    case "session.deleted": {
      const id = p.info?.id;
      if (!id || parents.has(id)) return;
      send("session.deleted", { session_id: id, agent_session_id: id, directory: directoryOf(inst, id) });
      sessions.delete(id);
      return;
    }
    case "session.error":
      if (p.sessionID) lastError.set(p.sessionID, errText(p.error).slice(0, MAX_TEXT));
      return;
    case "session.idle": {
      const id = p.sessionID;
      if (!id || parents.has(id)) return;
      const err = lastError.get(id);
      const text = lastText.get(id) || (err ? "Error: " + err : "");
      lastText.delete(id);
      lastError.delete(id);
      send("session.idle", { session_id: id, agent_session_id: id, directory: directoryOf(inst, id), text });
      return;
    }
    case "permission.asked":
      onPermissionAsked(inst, p);
      return;
    case "question.asked":
      onQuestionAsked(inst, p);
      return;
    case "permission.replied":
    case "question.replied":
    case "question.rejected":
      onAnswered(inst, event.type, p);
      return;
  }
}

function promptText(parts) {
  return (parts || [])
    .filter((x) => x?.type === "text" && !x.synthetic && !x.ignored)
    .map((x) => x.text || "")
    .join("\n")
    .slice(0, MAX_TEXT);
}

// ---------- replies from Pilothouse ----------

let polling = false;
let pollCtrl = null;

async function runCommand(cmd) {
  const s = sessions.get(cmd.session_id);
  let err = "";
  if (!s) {
    err = "OpenCode no longer has this session open";
  } else {
    try {
      if (cmd.interrupt) await s.client.session.abort({ path: { id: cmd.session_id } });
      if (cmd.text) {
        const r = await s.client.session.promptAsync({
          path: { id: cmd.session_id },
          body: { parts: [{ type: "text", text: cmd.text }] },
        });
        if (r?.error) err = errText(r.error);
      }
    } catch (e) {
      err = errText(e) || "OpenCode refused the prompt";
    }
  }
  try {
    await call("POST", "/v1/plugin/commands/" + encodeURIComponent(cmd.id) + "/ack", JSON.stringify({ ok: !err, error: err.slice(0, 300) }));
  } catch {}
}

// pollLoop keeps the command channel open: one long poll at a time,
// backing off while the daemon is down.
async function pollLoop() {
  let backoff = 500;
  while (polling) {
    try {
      const r = await call("GET", "/v1/plugin/commands?wait=" + POLL_WAIT, undefined, {
        timeout: (POLL_WAIT + 10) * 1000,
        signal: pollCtrl.signal,
      });
      if (r.status === 200) {
        backoff = 500;
        const { commands } = JSON.parse(r.text || "{}");
        for (const c of Array.isArray(commands) ? commands : []) runCommand(c);
        continue;
      }
    } catch {}
    if (!polling) return;
    await sleep(backoff);
    backoff = Math.min(backoff * 2, 10000);
  }
}

function startPolling() {
  if (polling) return;
  polling = true;
  pollCtrl = new AbortController();
  pollLoop();
}

function stopPolling() {
  polling = false;
  pollCtrl?.abort(new Error("plugin disposed"));
  for (const c of holds.values()) c.abort(new Error("plugin disposed"));
  holds.clear();
}

// ---------- plugin ----------

async function server(input) {
  const inst = { client: input?.client, directory: input?.directory || input?.worktree || "" };
  instances.add(inst);
  startPolling();
  return {
    event: async ({ event }) => {
      try {
        onEvent(inst, event);
      } catch {}
    },
    "chat.message": async (input, output) => {
      try {
        const sid = input.sessionID;
        remember(inst, sid);
        if (parents.has(sid)) return;
        send("chat.message", {
          session_id: root(sid),
          agent_session_id: sid,
          directory: directoryOf(inst, sid),
          text: promptText(output?.parts),
        });
      } catch {}
    },
    "tool.execute.before": async (input, output) => preTool(inst, input, output),
    "tool.execute.after": async (input) => {
      try {
        const sid = input.sessionID;
        send("tool.execute.after", {
          session_id: root(sid),
          agent_session_id: sid,
          directory: directoryOf(inst, sid),
          tool: input.tool,
          call_id: input.callID,
          args: calls.get(input.callID)?.args ?? input.args,
        });
      } catch {}
    },
    "experimental.session.compacting": async (input) => {
      try {
        const sid = input.sessionID;
        if (parents.has(sid)) return;
        send("session.compacting", { session_id: root(sid), agent_session_id: sid, directory: directoryOf(inst, sid) });
      } catch {}
    },
    dispose: async () => {
      instances.delete(inst);
      if (instances.size === 0) stopPolling();
    },
  };
}

export default { id: "pilothouse", server };

if (test) test.internals = { yamlFailClosed, root, sessions, holds, socketPath, TOKEN, stopPolling, Bounded };
