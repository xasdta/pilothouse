// Unit tests for pilothouse.js with a fake daemon and a fake OpenCode
// client. Run: node --test internal/adapters/opencode/plugin/
// (TestPluginJS in the Go package runs them when node is installed.)
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

let loads = 0;

// load imports a fresh copy of the plugin wired to a fake daemon.
// answer(event, payload) is what the daemon answers to a hook (an object;
// a thrown error means the daemon is unreachable).
async function load({ answer = () => ({}), env = {} } = {}) {
  const daemon = { hooks: [], acks: [], polls: [], queue: [], wake: null };
  const t = {
    env: { HOME: "/home/x", ...env },
    transport: async (method, pathname, body, signal) => {
      if (pathname === "/v1/plugin/hook") {
        const e = JSON.parse(body);
        daemon.hooks.push(e);
        const a = await answer(e.event, e.payload, signal);
        if (a instanceof Error) throw a;
        return { status: 200, text: JSON.stringify(a && a.action ? { stdout: JSON.stringify(a) } : {}) };
      }
      if (pathname === "/v1/plugin/commands") {
        daemon.polls.push(JSON.parse(body));
        if (daemon.down) throw new Error("ECONNREFUSED");
        if (!daemon.queue.length) {
          await new Promise((resolve) => {
            daemon.wake = resolve;
            signal.addEventListener("abort", resolve, { once: true });
            setTimeout(resolve, 50);
          });
        }
        const commands = daemon.queue.splice(0);
        return { status: 200, text: JSON.stringify({ commands }) };
      }
      if (pathname.startsWith("/v1/plugin/commands/")) {
        daemon.acks.push({ id: decodeURIComponent(pathname.split("/")[4]), ...JSON.parse(body) });
        return { status: 204, text: "" };
      }
      throw new Error("unexpected " + pathname);
    },
  };
  globalThis.__PILOTHOUSE_TEST__ = t;
  const mod = await import("./pilothouse.js?load=" + ++loads);
  delete globalThis.__PILOTHOUSE_TEST__;
  const client = fakeClient();
  const hooks = await mod.default.server({ client, directory: "/work/repo" });
  daemon.send = (cmd) => {
    daemon.queue.push(cmd);
    daemon.wake?.();
  };
  return { mod, hooks, client, daemon, internals: t.internals, dispose: () => hooks.dispose() };
}

function fakeClient() {
  const c = {
    posts: [],
    prompts: [],
    aborts: [],
    toasts: [],
    _client: {
      post: async (o) => {
        c.posts.push({ url: o.url, path: o.path, body: o.body });
        return { data: true };
      },
    },
    session: {
      promptAsync: async (o) => {
        c.prompts.push(o);
        if (o.path.id === "ses_gone") return { error: { name: "NotFoundError", data: { message: "Session not found" } } };
        return { data: undefined };
      },
      abort: async (o) => {
        c.aborts.push(o.path.id);
        return { data: true };
      },
    },
    tui: { showToast: async (o) => c.toasts.push(o.body.message) },
  };
  return c;
}

const settle = (ms = 30) => new Promise((r) => setTimeout(r, ms));

async function until(what, cond, ms = 2000) {
  const end = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > end) throw new Error("timed out waiting for " + what);
    await settle(5);
  }
}

const event = (type, properties) => ({ event: { type, properties } });

test("policy deny throws, no opinion lets the call run", async () => {
  const p = await load({ answer: (ev, pl) => (pl.args?.command === "rm -rf /" ? { action: "deny", message: "No." } : {}) });
  await assert.rejects(p.hooks["tool.execute.before"]({ tool: "bash", sessionID: "ses_a", callID: "c1" }, { args: { command: "rm -rf /" } }), /^Error: No\.$/);
  await p.hooks["tool.execute.before"]({ tool: "bash", sessionID: "ses_a", callID: "c2" }, { args: { command: "ls" } });
  const h = p.daemon.hooks.find((x) => x.event === "tool.execute.before");
  assert.equal(h.agent, "opencode");
  assert.equal(h.payload.v, 1);
  assert.equal(h.payload.session_id, "ses_a");
  assert.equal(h.payload.call_id, "c1");
  assert.equal(h.payload.directory, "/work/repo");
  assert.equal(h.ancestors[0].pid, process.pid);
  await p.dispose();
});

test("daemon down: fail open, or closed when approvals.fail_closed is on", async () => {
  let p = await load({ answer: () => new Error("ECONNREFUSED") });
  await p.hooks["tool.execute.before"]({ tool: "bash", sessionID: "s", callID: "c" }, { args: { command: "ls" } });
  await p.dispose();

  const home = fs.mkdtempSync(path.join(os.tmpdir(), "ph-"));
  fs.writeFileSync(path.join(home, "config.yaml"), "web:\n  addr: 127.0.0.1:7517\napprovals:\n  hold_limit: 9m\n  fail_closed: true # strict\n");
  p = await load({ answer: () => new Error("ECONNREFUSED"), env: { PILOTHOUSE_HOME: home } });
  await assert.rejects(p.hooks["tool.execute.before"]({ tool: "bash", sessionID: "s", callID: "c" }, { args: { command: "ls" } }), /fail_closed is on/);
  await p.dispose();
  fs.rmSync(home, { recursive: true });
});

test("policy ask is held through pilothouse.ask", async () => {
  let verdict = { action: "allow" };
  const p = await load({
    answer: (ev) => (ev === "tool.execute.before" ? { action: "ask", message: "Pushes need a person.", rule: "ask-push", summary: "Run: git push" } : ev === "pilothouse.ask" ? verdict : {}),
  });
  const call = () => p.hooks["tool.execute.before"]({ tool: "bash", sessionID: "s", callID: "c" }, { args: { command: "git push" } });
  await call();
  const ask = p.daemon.hooks.find((x) => x.event === "pilothouse.ask");
  assert.equal(ask.payload.rule, "ask-push");
  assert.equal(ask.payload.args.command, "git push");
  assert.match(p.client.toasts[0], /Waiting for a person to approve in Pilothouse: Run: git push/);
  verdict = { action: "deny", message: "Not today." };
  await assert.rejects(call(), /^Error: Not today\.$/);
  verdict = {}; // hold expired or nobody answered
  await assert.rejects(call(), /a person must approve this, and nobody did .*\(Pushes need a person\)\.$/);
  await p.dispose();
});

test("permission prompts: enriched, answered through the client", async () => {
  const answers = { per_1: { action: "allow" }, per_2: { action: "deny", message: "Use a dry run." }, per_3: {} };
  const p = await load({ answer: (ev, pl) => (ev === "permission.asked" ? answers[pl.request_id] : {}) });
  await p.hooks["tool.execute.before"]({ tool: "bash", sessionID: "ses_a", callID: "call_1" }, { args: { command: "make deploy" } });
  for (const id of ["per_1", "per_2", "per_3"]) {
    p.hooks.event(event("permission.asked", { id, sessionID: "ses_a", permission: "bash", patterns: ["make deploy"], metadata: { command: "make deploy" }, always: ["make *"], tool: { messageID: "m", callID: "call_1" } }));
  }
  await until("two replies", () => p.client.posts.length === 2);
  await settle();
  const h = p.daemon.hooks.find((x) => x.event === "permission.asked");
  assert.equal(h.payload.tool, "bash");
  assert.deepEqual(h.payload.args, { command: "make deploy" });
  assert.equal(h.payload.permission.permission, "bash");
  const byId = Object.fromEntries(p.client.posts.map((x) => [x.path.requestID, x]));
  assert.equal(byId.per_1.url, "/permission/{requestID}/reply");
  assert.deepEqual(byId.per_1.body, { reply: "once" });
  assert.deepEqual(byId.per_2.body, { reply: "reject", message: "Use a dry run." });
  assert.equal(byId.per_3, undefined, "no opinion leaves OpenCode's dialog alone");
  await p.dispose();
});

test("answered in OpenCode while held: reported, never answered twice", async () => {
  let release;
  const p = await load({
    answer: (ev, pl, signal) =>
      ev === "permission.asked"
        ? new Promise((resolve) => {
            release = () => resolve({ action: "allow" });
            signal.addEventListener("abort", () => resolve(new Error("aborted")), { once: true });
          })
        : {},
  });
  p.hooks.event(event("permission.asked", { id: "per_9", sessionID: "ses_a", permission: "bash", tool: { callID: "x" } }));
  await until("hold", () => release);
  p.hooks.event(event("permission.replied", { sessionID: "ses_a", requestID: "per_9", reply: "reject" }));
  await until("replied hook", () => p.daemon.hooks.some((x) => x.event === "permission.replied"));
  const r = p.daemon.hooks.find((x) => x.event === "permission.replied");
  assert.equal(r.payload.request_id, "per_9");
  assert.equal(r.payload.reply, "reject");
  release(); // the daemon's late "allow" must not reach OpenCode
  await settle(50);
  assert.equal(p.client.posts.length, 0);
  await p.dispose();
});

test("questions: answered in order, or rejected", async () => {
  const answers = { que_1: { action: "answer", answers: [["Blue"], ["S", "L"]] }, que_2: { action: "deny", message: "no" } };
  const p = await load({ answer: (ev, pl) => (ev === "question.asked" ? answers[pl.request_id] : {}) });
  const qs = [{ question: "Color?", header: "C", options: [{ label: "Red", description: "" }, { label: "Blue", description: "" }] }];
  p.hooks.event(event("question.asked", { id: "que_1", sessionID: "ses_a", questions: qs, tool: { callID: "c" } }));
  p.hooks.event(event("question.asked", { id: "que_2", sessionID: "ses_a", questions: qs }));
  await until("two answers", () => p.client.posts.length === 2);
  const byId = Object.fromEntries(p.client.posts.map((x) => [x.path.requestID, x]));
  assert.equal(byId.que_1.url, "/question/{requestID}/reply");
  assert.deepEqual(byId.que_1.body, { answers: [["Blue"], ["S", "L"]] });
  assert.equal(byId.que_2.url, "/question/{requestID}/reject");
  const h = p.daemon.hooks.find((x) => x.event === "question.asked");
  assert.deepEqual(h.payload.questions, qs);
  await p.dispose();
});

test("session events: start, prompt, last message, idle; subagents fold into their parent", async () => {
  const p = await load();
  p.hooks.event(event("session.created", { info: { id: "ses_a", title: "New session - x", directory: "/work/other" } }));
  p.hooks.event(event("session.created", { info: { id: "ses_child", parentID: "ses_a", title: "sub" } }));
  await p.hooks["chat.message"]({ sessionID: "ses_a" }, { parts: [{ type: "text", text: "Fix it" }, { type: "text", text: "ctx", synthetic: true }] });
  await p.hooks["tool.execute.before"]({ tool: "bash", sessionID: "ses_child", callID: "c" }, { args: { command: "ls" } });
  p.hooks.event(event("message.updated", { info: { id: "msg_1", role: "assistant", sessionID: "ses_a" } }));
  p.hooks.event(event("message.part.updated", { part: { type: "text", text: "Done", messageID: "msg_1", sessionID: "ses_a" } }));
  p.hooks.event(event("message.part.updated", { part: { type: "text", text: "Done: all green.", messageID: "msg_1", sessionID: "ses_a" } }));
  p.hooks.event(event("session.idle", { sessionID: "ses_child" }));
  p.hooks.event(event("session.idle", { sessionID: "ses_a" }));
  p.hooks.event(event("session.updated", { info: { id: "ses_a", title: "Fix the login bug" } }));
  p.hooks.event(event("session.updated", { info: { id: "ses_a", title: "Fix the login bug" } }));
  await until("idle", () => p.daemon.hooks.some((x) => x.event === "session.updated"));
  await settle();
  const evs = p.daemon.hooks.map((x) => x.event);
  assert.deepEqual(evs, ["session.created", "chat.message", "tool.execute.before", "session.idle", "session.updated"]);
  const [created, chat, tool, idle] = p.daemon.hooks;
  assert.equal(created.payload.directory, "/work/other");
  assert.equal(chat.payload.text, "Fix it");
  assert.equal(tool.payload.session_id, "ses_a", "a subagent's call belongs to its parent");
  assert.equal(tool.payload.agent_session_id, "ses_child");
  assert.equal(idle.payload.text, "Done: all green.");
  await p.dispose();
});

test("an error ends the turn's last message", async () => {
  const p = await load();
  p.hooks.event(event("session.error", { sessionID: "ses_a", error: { name: "APIError", data: { message: "rate limited" } } }));
  p.hooks.event(event("session.idle", { sessionID: "ses_a" }));
  await until("idle", () => p.daemon.hooks.length === 1);
  assert.equal(p.daemon.hooks[0].payload.text, "Error: rate limited");
  await p.dispose();
});

test("replies from Pilothouse run through promptAsync and are acknowledged", async () => {
  const p = await load();
  p.hooks.event(event("session.created", { info: { id: "ses_a" } }));
  p.hooks.event(event("session.created", { info: { id: "ses_gone" } }));
  p.hooks.event(event("session.created", { info: { id: "ses_kid", parentID: "ses_a" } }));
  await until("a poll announcing the sessions", () => p.daemon.polls.some((x) => x.sessions.includes("ses_a")));
  const last = p.daemon.polls.at(-1);
  assert.ok(!last.sessions.includes("ses_kid"), "subagents are not announced");
  p.daemon.send({ id: "pc_1", session_id: "ses_a", text: "Run the tests" });
  p.daemon.send({ id: "pc_2", session_id: "ses_a", interrupt: true });
  p.daemon.send({ id: "pc_3", session_id: "ses_nope", text: "hi" });
  p.daemon.send({ id: "pc_4", session_id: "ses_gone", text: "hi" });
  await until("acks", () => p.daemon.acks.length === 4);
  const acks = Object.fromEntries(p.daemon.acks.map((a) => [a.id, a]));
  assert.deepEqual(acks.pc_1, { id: "pc_1", ok: true, error: "" });
  assert.deepEqual(p.client.prompts[0], { path: { id: "ses_a" }, body: { parts: [{ type: "text", text: "Run the tests" }] } });
  assert.equal(acks.pc_2.ok, true);
  assert.deepEqual(p.client.aborts, ["ses_a"]);
  assert.equal(acks.pc_3.ok, false);
  assert.match(acks.pc_3.error, /no longer has this session/);
  assert.equal(acks.pc_4.ok, false);
  assert.match(acks.pc_4.error, /Session not found/);
  await p.dispose();
});

test("the poll backs off while the daemon is down and stops on dispose", async () => {
  const p = await load();
  p.daemon.down = true;
  await settle(200);
  const n = p.daemon.polls.length;
  assert.ok(n < 10, `polled ${n} times in 200 ms with the daemon down`);
  await p.dispose();
  const after = p.daemon.polls.length;
  await settle(1200);
  assert.equal(p.daemon.polls.length, after, "polling after dispose");
});

test("yamlFailClosed", async () => {
  const { internals } = await load();
  const f = internals.yamlFailClosed;
  assert.equal(f("approvals:\n  fail_closed: true\n"), true);
  assert.equal(f("approvals:\n  hold_limit: 9m\n  fail_closed: yes # strict\n"), true);
  assert.equal(f("approvals:\n  fail_closed: false\n"), false);
  assert.equal(f("approvals: {fail_closed: true, hold_limit: 9m}\n"), true);
  assert.equal(f("notify:\n  fail_closed: true\napprovals:\n  always_hold: true\n"), false);
  assert.equal(f("# approvals:\n#   fail_closed: true\n"), false);
  assert.equal(f(""), false);
  internals.stopPolling();
});

test("socket path follows PILOTHOUSE_RUN_DIR, then PILOTHOUSE_HOME", async () => {
  let p = await load({ env: { PILOTHOUSE_RUN_DIR: "/r" } });
  assert.equal(p.internals.socketPath(), "/r/pilothouse.sock");
  await p.dispose();
  p = await load({ env: { PILOTHOUSE_HOME: "/h" } });
  assert.equal(p.internals.socketPath(), "/h/run/pilothouse.sock");
  await p.dispose();
  assert.match(p.internals.TOKEN, /^[A-Za-z0-9_-]{43}$/);
});

test("the plugin module is what OpenCode 1.x loads", async () => {
  const p = await load();
  assert.equal(p.mod.default.id, "pilothouse");
  assert.equal(typeof p.mod.default.server, "function");
  assert.deepEqual(Object.keys(p.mod), ["default"], "named exports would be loaded as plugins too");
  await p.dispose();
});
