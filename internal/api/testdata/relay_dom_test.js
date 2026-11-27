// A test of the GUI side of session attach (P.5): the Watch and Prompt
// controls of §G.2 and the live transcripts over the session relay
// (internal/api/web/watch.js). It loads the real bundle into a fake browser
// (fakedom.js) whose WebSocket the test drives as the relay would, and
// checks that the project view's strip and the sandbox card read a live
// session over the relay instead of polling, that Watch and Prompt link to
// the watch page, that Prompt sends only {"t":"prompt"} and {"t":"cancel"}
// into a persistent session, that an observer sends nothing, that a
// dropped connection resumes from its last offset and a refused one falls
// back to polling, and that a watched one-shot session ends as it would
// unwatched. Run by TestProjectViewModel (web_test.go):
//
//   node internal/api/testdata/relay_dom_test.js internal/api/web
"use strict";

const assert = require("node:assert/strict");
const path = require("node:path");
const { boot, buttons, label, SCRIPTS } = require("./fakedom.js");

const webDir = path.resolve(process.argv[2] || path.join(__dirname, "..", "web"));

const task = (name, status, extra) =>
  Object.assign(
    {
      project: "p_0.gui-api", name, status, attempts: 0, failure_reason: "", blocked_reason: "",
      depends_on: [], touches: null, areas: [], exclusive: false, worktree: "/w/" + name, etag: '"1"',
      capabilities: { model: "default", resolved_model: "claude-opus-5-5", harness: "claude-acp", static_mcps: ["github"], policies: [], save_sandbox: false },
      source: null, base: [], succeeded_at: null, completed_at: null, queue_position: 0, waiting_on: [], integration: null,
      output: { summary: "", commits: [], created_files: [], touches_actual: [] },
      task_branch: "wm/gui-api/" + name, sandbox: "gui-api-" + name, commit_sandbox: "gui-api-" + name + "-commit", session: "",
    },
    extra
  );

function fixture(scopes) {
  const tasks = [task("session-relay", "running", { session: "task-session-relay" })];
  const project = {
    id: "p_0.gui-api", work_stream: "gui-api", status: "working", branch: "gui-api", max_parallel_tasks: 1, etag: '"p1"',
    flags: {}, commit_queue: [], task_counts: {}, task_total: 1, pull_requests: [],
    live: { integrating: "", running_tasks: ["session-relay"] },
  };
  const state = {
    sessions: [
      {
        id: "task-session-relay", kind: "task", project: "p_0.gui-api", task: "session-relay", started_at: new Date(Date.now() - 60000).toISOString(),
        prompt_count: 1, persistent: false, status: "running", acp: { attachable: true, observers: 0 },
        sandbox: { name: "gui-api-session-relay", live: true, image: "img:1", workspaces: [] }, gateway: { requested: ["github"], servers: ["github"], dynamic: false },
      },
      {
        id: "wolf-gui-api", kind: "wolf", project: "p_0.gui-api", started_at: new Date(Date.now() - 60000).toISOString(),
        prompt_count: 1, persistent: true, status: "running", acp: { attachable: true, observers: 0 },
        sandbox: { name: "gui-api-wolf", live: true, image: "img:1", workspaces: [] }, gateway: null,
      },
      {
        id: "step-up-agent", kind: "step-up", started_at: new Date(Date.now() - 60000).toISOString(),
        prompt_count: 1, persistent: true, status: "running", acp: { attachable: true, observers: 0 },
        sandbox: { name: "step-up", live: true, image: "img:1", workspaces: [] }, gateway: null,
      },
    ],
    log: "",
  };
  const byID = (id) => () => {
    const s = state.sessions.find((x) => x.id === id);
    if (!s) throw Object.assign(new Error("no session"), { status: 404, code: "not_found" });
    return s;
  };
  const routes = {
    "/api/v1/session": () => ({ auth: "cookie", csrf: "csrf-1", scopes }),
    "/api/v1/status": () => ({ daemon: { roots: ["/Users/me/orch"] }, cursor: 10 }),
    "/api/v1/projects": () => ({ items: [project], cursor: 10 }),
    "/api/v1/projects/p_0.gui-api": () => project,
    "/api/v1/projects/p_0.gui-api/tasks": () => ({ items: tasks, cursor: 10 }),
    "/api/v1/projects/p_0.gui-api/tasks/session-relay": () => tasks[0],
    "/api/v1/projects/p_0.gui-api/graph": () => ({ nodes: tasks.map((t) => ({ name: t.name, status: t.status })), edges: [], commit_queue: [], errors: [], max_attempts: 3 }),
    "/api/v1/sessions": () => ({ items: state.sessions, cursor: 10 }),
    "/api/v1/sessions/task-session-relay": byID("task-session-relay"),
    "/api/v1/sessions/wolf-gui-api": byID("wolf-gui-api"),
    "/api/v1/sessions/step-up-agent": byID("step-up-agent"),
    "/api/v1/sessions/task-session-relay/prompt": () => ({ instructions_md: "You are a task agent.\n", context_yaml: "kind: task\n" }),
    "/api/v1/sandboxes": () => ({ items: [], cursor: 10 }),
    "/api/v1/audit": () => ({ items: [], cursor: 10 }),
    "/api/v1/messages": () => ({ items: [], cursor: 10 }),
  };
  // The transcript endpoint, for the preview's start and for polling.
  const polls = [];
  const raw = (u) => {
    if (!u.pathname.endsWith("/transcript")) return null;
    polls.push(u.search);
    const log = state.log;
    const after = Number(u.searchParams.get("after") || 0);
    const head = u.searchParams.get("limit") === "1";
    const body = head ? log.slice(0, log.indexOf("\n") + 1) : log.slice(after);
    return { ok: true, status: 200, headers: new Map([["X-Transcript-Size", String(log.length)], ["X-Transcript-Next", String(head ? body.length : log.length)]]), text: async () => body };
  };
  return { state, routes, raw, polls };
}

const upd = (u) => ({ jsonrpc: "2.0", method: "session/update", params: { sessionId: "s1", update: u } });
const text = (t) => upd({ sessionUpdate: "agent_message_chunk", content: { type: "text", text: t } });
const tool = (id, title) => upd({ sessionUpdate: "tool_call", toolCallId: id, title, kind: "read", status: "completed" });

function page(scopes, url, opts) {
  const f = fixture(scopes);
  const p = boot(Object.assign({ webDir, scripts: SCRIPTS, url, routes: f.routes, raw: f.raw, skip: (fn) => String(fn).includes("pollTranscript(tr)") }, opts || {}));
  return Object.assign(p, f);
}

const ALL = ["read", "write", "answer", "attach", "interact"];
const writes = (requests) => requests.filter((r) => r.method !== "GET" && r.method !== "HEAD");
const byLabel = (main, l) => buttons(main).find((b) => label(b) === l);
const linkTo = (main, href) => main.find((e) => e.tagName === "A" && e.getAttribute("href") === href);
const q = (ws) => new URL(ws.url).searchParams;

function noStray(main, where) {
  const t = main.textContent;
  assert.ok(!/null|undefined|\[object|NaN/.test(t), where + ": stray value rendered: " + t);
  return t;
}

async function start(pg) {
  pg.start();
  await pg.settle();
  pg.es.source.send({ seq: 10, type: "hello", data: { cursor: 10, epoch: "1" } });
  await pg.settle();
}

// relayHello plays the relay's opening: hello, the replay, live, turn.
function relayHello(ws, sess, frames, mode) {
  ws.message({ t: "hello", session: sess.id, kind: sess.kind, persistent: sess.persistent, interactive_allowed: sess.persistent, mode: mode || "observe", prompt_count: 1, replay: "all" });
  let off = 0;
  for (const f of frames) {
    off += 100;
    ws.message({ t: "replay", offset: off, frame: f });
  }
  ws.message({ t: "live" });
  ws.message({ t: "turn", state: "running" });
  return off;
}

(async () => {
  // ---- the project view's strip: live over the relay, Watch is a link ----
  {
    const pg = page(ALL, "http://127.0.0.1:7078/projects/p_0.gui-api?task=session-relay");
    pg.state.log = JSON.stringify(tool("t0", "Read go.mod")) + "\n";
    await start(pg);
    const { main } = pg;
    assert.equal(pg.sockets.length, 1, "the strip's preview attaches to the relay");
    const ws = pg.sockets[0];
    assert.equal(new URL(ws.url).pathname, "/api/v1/sessions/task-session-relay/acp");
    assert.equal(new URL(ws.url).protocol, "ws:");
    assert.equal(q(ws).get("mode"), "observe");
    assert.equal(q(ws).get("csrf"), "csrf-1", "a browser's upgrade proves it read the CSRF token");
    assert.equal(JSON.stringify(ws.protocols), JSON.stringify(["workingman.acp.v1"]));
    // The preview starts near the end of the log, as polling did: from the
    // start here, since the log is short.
    assert.equal(q(ws).get("replay"), "all");
    relayHello(ws, pg.state.sessions[0], [tool("t1", "Read internal/acpwrapper/bridge.go")]);
    await pg.settle();
    let t = noStray(main, "strip");
    assert.match(t, /▸ Read internal\/acpwrapper\/bridge\.go/);
    assert.match(t, /transcript \(live over the session relay\)/);
    assert.match(t, /observe over the session relay, live/);
    assert.match(t, /turn running…/);
    // A live frame is drawn as it arrives.
    ws.message({ t: "frame", offset: 300, frame: text("ok  internal/api") });
    await pg.settle();
    assert.match(main.textContent, /ok {2}internal\/api/);
    // Watch is a link to the watch page; Prompt stays refused for a task.
    assert.ok(linkTo(main, "/sessions/task-session-relay"), "Watch links to the watch page");
    assert.ok(!byLabel(main, "Watch (ACP)"), "Watch is not a disabled button any more");
    assert.ok(byLabel(main, "Prompt (persistent only)").disabled);
    assert.ok(!pg.polls.some((s) => s.includes("after=")), "the relay replaces polling");
    // The turn ends; then the session ends, and the relay says so.
    ws.message({ t: "frame", offset: 400, frame: { jsonrpc: "2.0", id: 3, result: { stopReason: "end_turn" } } });
    ws.message({ t: "turn", state: "idle", stop_reason: "end_turn" });
    await pg.settle();
    assert.match(main.textContent, /idle · end_turn/);
    pg.es.source.send({ seq: 11, type: "session.ended", project: "p_0.gui-api", data: { id: "task-session-relay", kind: "task", outcome: "ended" } });
    await pg.settle();
    ws.message({ t: "ended", reason: "exited" });
    ws.serverClose(1000);
    await pg.settle();
    t = main.textContent;
    assert.match(t, /session ended — transcript lost/);
    assert.match(t, /ok {2}internal\/api/, "what was relayed stays");
    assert.equal(ws.sent.length, 0, "an observer sends nothing");
    assert.equal(pg.sockets.length, 1, "no reconnect after the session ended");
    assert.equal(writes(pg.requests).length, 0);
  }

  // ---- the sandbox card: the same, and leaving the page detaches ----
  {
    const pg = page(ALL, "http://127.0.0.1:7078/projects/p_0.gui-api/tasks/session-relay");
    await start(pg);
    const { main } = pg;
    assert.equal(pg.sockets.length, 1, "the card's transcript attaches to the relay");
    const ws = pg.sockets[0];
    relayHello(ws, pg.state.sessions[0], [tool("t1", "Read relay.go")]);
    await pg.settle();
    assert.match(main.textContent, /▸ Read relay\.go/);
    assert.ok(linkTo(main, "/sessions/task-session-relay"), "the card's Watch links to the watch page");
    // A card redraw (task.updated) keeps the one connection.
    pg.es.source.send({ seq: 11, type: "task.updated", project: "p_0.gui-api", data: { project: "p_0.gui-api", name: "session-relay" } });
    await pg.settle();
    assert.equal(pg.sockets.length, 1, "a redraw does not re-attach");
    assert.match(main.textContent, /▸ Read relay\.go/);
    await pg.navigate("/timeline");
    assert.equal(ws.closed && ws.closed.by, "page", "leaving the card closes its relay connection");
  }

  // ---- the watch page: a dropped connection resumes from its offset ----
  {
    const pg = page(ALL, "http://127.0.0.1:7078/sessions/task-session-relay");
    await start(pg);
    const { main } = pg;
    const ws = pg.sockets[0];
    assert.equal(q(ws).get("replay"), "all", "the full view replays from the start");
    assert.equal(q(ws).get("mode"), "observe");
    const off = relayHello(ws, pg.state.sessions[0], [tool("t1", "Read a.go"), text("halfway")]);
    await pg.settle();
    // The relay drops a slow client: the page re-attaches after the frames it has.
    ws.message({ t: "error", code: "too_slow", message: "this connection fell too far behind the session" });
    ws.serverClose(1008);
    await pg.settle();
    assert.equal(pg.sockets.length, 2, "the page re-attaches");
    assert.equal(q(pg.sockets[1]).get("replay"), "after:" + off);
    relayHello(pg.sockets[1], pg.state.sessions[0], []);
    pg.sockets[1].message({ t: "frame", offset: off + 100, frame: text(" and done") });
    await pg.settle();
    const t = noStray(main, "watch page");
    assert.match(t, /▸ Read a\.go ✓/);
    assert.match(t, /halfway and done/, "no gap and no repeat across the reconnect");
    assert.ok(!byLabel(main, "Watch (ACP)"), "the watch page is Watch");
    assert.ok(byLabel(main, "Prompt (persistent only)").disabled, "a task session takes no prompts");
    assert.equal(main.find((e) => e.getAttribute("aria-label") === "prompt"), null, "no composer while observing");
    pg.sockets.forEach((s) => assert.equal(s.sent.length, 0));
  }

  // ---- a refused upgrade falls back to polling ----
  {
    const pg = page(ALL, "http://127.0.0.1:7078/sessions/task-session-relay");
    pg.state.log = JSON.stringify(tool("t1", "Read b.go")) + "\n";
    await start(pg);
    pg.sockets[0].refuse();
    await pg.settle();
    assert.ok(pg.polls.some((s) => s.includes("after=0")), "the page polls when the relay refuses");
    assert.match(pg.main.textContent, /▸ Read b\.go/);
    assert.match(pg.main.textContent, /read-only \(observe\), replayed from stream\.log/);
    // A later session.updated does not retry a relay that refused.
    pg.es.source.send({ seq: 11, type: "session.updated", data: { id: "task-session-relay", session: pg.state.sessions[0] } });
    await pg.settle();
    assert.equal(pg.sockets.length, 1);
  }

  // ---- without the attach scope, or without WebSockets: polling, as in P.3 ----
  for (const [scopes, opts, why] of [[["read"], {}, "no attach scope"], [ALL, { noWebSocket: true }, "no WebSocket"]]) {
    const pg = page(scopes, "http://127.0.0.1:7078/projects/p_0.gui-api?task=session-relay", opts);
    await start(pg);
    assert.equal((pg.sockets || []).length, 0, why + ": no relay");
    assert.ok(pg.polls.some((s) => s.includes("after=")), why + ": polls");
    if (why === "no attach scope") assert.ok(byLabel(pg.main, "Watch (ACP)").disabled, "Watch needs the attach scope");
  }

  // ---- Prompt: a persistent session, interactive over the relay ----
  {
    const pg = page(ALL, "http://127.0.0.1:7078/sessions/wolf-gui-api");
    await start(pg);
    const { main } = pg;
    // The watch page of a wolf observes; its Prompt links to ?prompt=1.
    assert.equal(q(pg.sockets[0]).get("mode"), "observe");
    assert.ok(linkTo(main, "/sessions/wolf-gui-api?prompt=1"), "Prompt links to the interactive watch page");
    await pg.navigate("/sessions/wolf-gui-api?prompt=1");
    assert.equal(pg.sockets[0].closed.by, "page");
    const ws = pg.sockets[1];
    assert.equal(q(ws).get("mode"), "interactive");
    const box = main.find((e) => e.getAttribute("aria-label") === "prompt");
    assert.ok(box, "the composer is shown");
    const send = () => main.find((e) => e.tagName === "BUTTON" && label(e) === "Send");
    const cancel = () => main.find((e) => e.tagName === "BUTTON" && label(e) === "Cancel turn");
    assert.ok(send().disabled, "Send waits for the relay's hello");
    relayHello(ws, pg.state.sessions[1], [text("What should I do about the block?")], "interactive");
    ws.message({ t: "turn", state: "idle", stop_reason: "end_turn" });
    await pg.settle();
    assert.match(main.textContent, /interactive over the session relay/);
    assert.ok(!send().disabled && cancel().disabled, "idle: Send on, Cancel off");
    assert.ok(linkTo(main, "/sessions/wolf-gui-api"), "a way back to observe only");
    box.value = "Retry the task with a longer timeout.";
    send().dispatch("click");
    await pg.settle();
    assert.deepEqual(ws.sent, [{ t: "prompt", text: "Retry the task with a longer timeout." }]);
    let t = main.textContent;
    assert.match(t, /you \(from this page\)Retry the task with a longer timeout\./);
    assert.equal(box.value, "");
    assert.ok(send().disabled && !cancel().disabled, "running: Send off, Cancel on");
    ws.message({ t: "turn", state: "running" });
    ws.message({ t: "frame", offset: 200, frame: text("On it.") });
    await pg.settle();
    cancel().dispatch("click");
    await pg.settle();
    assert.deepEqual(ws.sent[1], { t: "cancel" });
    // A refusal from the relay shows by the composer.
    ws.message({ t: "error", code: "conflict", message: "a turn is running; wait for it to end or cancel it" });
    await pg.settle();
    assert.match(main.textContent, /conflict: a turn is running/);
    ws.message({ t: "turn", state: "idle", stop_reason: "cancelled" });
    await pg.settle();
    t = noStray(main, "prompt");
    assert.match(t, /On it\./);
    assert.match(t, /idle · cancelled/);
    // The relay is the only path a prompt takes: no HTTP write.
    assert.equal(writes(pg.requests).length, 0);
    const saved = new Set(ws.sent.map((m) => m.t));
    assert.deepEqual([...saved].sort(), ["cancel", "prompt"], "the page sends prompt and cancel, nothing else");
  }

  // ---- Prompt refused: interactive is refused, the page observes ----
  {
    const pg = page(ALL, "http://127.0.0.1:7078/sessions/wolf-gui-api?prompt=1");
    await start(pg);
    assert.equal(q(pg.sockets[0]).get("mode"), "interactive");
    pg.sockets[0].refuse();
    await pg.settle();
    assert.equal(pg.sockets.length, 2);
    assert.equal(q(pg.sockets[1]).get("mode"), "observe", "a refused interactive attach falls back to observe");
    assert.equal(pg.main.find((e) => e.getAttribute("aria-label") === "prompt"), null);
  }

  // ---- no interact scope, and step-up: no composer ----
  {
    const pg = page(["read", "attach"], "http://127.0.0.1:7078/sessions/wolf-gui-api?prompt=1");
    await start(pg);
    assert.equal(q(pg.sockets[0]).get("mode"), "observe", "without the interact scope the page observes");
    assert.equal(pg.main.find((e) => e.getAttribute("aria-label") === "prompt"), null);
    await pg.navigate("/sessions/step-up-agent?prompt=1");
    assert.equal(q(pg.sockets[pg.sockets.length - 1]).get("mode"), "observe", "step-up takes no typed turns");
    const p = byLabel(pg.main, "Prompt (persistent only)");
    assert.ok(p && p.disabled);
    assert.match(p.getAttribute("title"), /escalation path/);
  }

  console.log("ok   Watch and Prompt over the session relay: live transcripts in the strip, card and watch page; prompt and cancel only into persistent sessions; resume, fallback and session end");
})().catch((e) => {
  console.log("FAIL " + (e && e.stack ? e.stack : e));
  process.exit(1);
});
