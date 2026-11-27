// A smoke test of the GUI's task actions and host tabs
// (internal/api/web/taskactions.js, P.4, last slice): it loads the real
// bundle into a fake browser (fakedom.js) over a fake API and checks that,
// for a session with the write scope, the project view's selected-task
// strip offers Retry… for a failed task, asks first (with "reset attempts"
// and what the retry does to the blocked project), and sends POST
// …/tasks/{name}/actions/retry with the task's etag as If-Match; that a 412
// takes the task as it is now and the next write uses its etag; that Keep
// sandbox and Edit task… (a merge-patch of only what changed) write the
// same way, and a 422 shows the task-graph refusal; that the host tabs
// (project and task) send open-session / open-shell and show where the tab
// opened; that the sandbox card offers the same controls; and that a
// read-only session gets them all disabled and sends nothing. Run by
// TestProjectViewModel (web_test.go):
//
//   node internal/api/testdata/task_actions_dom_test.js internal/api/web
"use strict";

const assert = require("node:assert/strict");
const path = require("node:path");
const { boot, buttons, label, SCRIPTS } = require("./fakedom.js");

const webDir = path.resolve(process.argv[2] || path.join(__dirname, "..", "web"));

function fixture(scopes) {
  const project = {
    id: "p_0.gui-api", work_stream: "gui-api", status: "blocked", blocked_reason: 'task "auth-token" failed after 3 attempts: go test failed',
    branch: "gui-api", max_parallel_tasks: 1, etag: '"pj-1111"', flags: {}, commit_queue: [], task_counts: { failed: 1, ready: 1 }, task_total: 2,
    pull_requests: [], live: { sessions: [], running_tasks: [] },
  };
  const caps = { model: "default", harness: "claude-acp", static_mcps: [], policies: [], save_sandbox: false };
  const auth = {
    project: "p_0.gui-api", name: "auth-token", status: "failed", attempts: 3, failure_reason: "go test failed", blocked_reason: "", etag: '"tk-1111"',
    description: "Add the token.", depends_on: [], touches: ["workingman/internal/api/**"], areas: [], exclusive: false, capabilities: Object.assign({}, caps),
    worktree: "/ws/.worktrees/auth-token", task_branch: "wm/gui-api/auth-token", base: [], waiting_on: [], integration: null, sandbox: "gui-api-auth-token",
    output: { summary: "", commits: [], created_files: [], touches_actual: [] }, queue_position: 0,
  };
  const relay = Object.assign({}, auth, { name: "session-relay", status: "ready", attempts: 0, failure_reason: "", etag: '"tk-2222"', depends_on: ["auth-token"], worktree: "", sandbox: "gui-api-session-relay", capabilities: Object.assign({}, caps) });
  const tasks = { "auth-token": auth, "session-relay": relay };
  const state = { project, tasks, stale: false, cycle: false };
  const routes = {
    "/api/v1/session": () => ({ auth: "cookie", csrf: "csrf-1", scopes }),
    "/api/v1/status": () => ({ daemon: { roots: ["/o"] }, cursor: 10 }),
    "/api/v1/projects": () => ({ items: [project], cursor: 10 }),
    "/api/v1/projects/p_0.gui-api": () => project,
    "/api/v1/projects/p_0.gui-api/tasks": () => ({ items: Object.values(tasks), cursor: 10 }),
    "/api/v1/projects/p_0.gui-api/graph": () => ({
      nodes: Object.values(tasks).map((t) => ({ name: t.name, status: t.status })), edges: [{ from: "auth-token", to: "session-relay" }],
      ready: [], runnable: [], commit_queue: [], errors: [], max_attempts: 3,
    }),
    "/api/v1/sessions": () => ({ items: [], cursor: 10 }),
    "/api/v1/sandboxes": () => ({ items: [], cursor: 10 }),
    "/api/v1/audit": () => ({ items: [], cursor: 10 }),
    "/api/v1/messages": () => ({ items: [], cursor: 10 }),
  };
  for (const name of Object.keys(tasks)) {
    const t = tasks[name];
    const base = "/api/v1/projects/p_0.gui-api/tasks/" + name;
    const stale = () => {
      if (!state.stale) return;
      state.stale = false;
      t.capabilities = Object.assign({}, t.capabilities, { save_sandbox: true });
      t.etag = '"tk-9999"';
      throw Object.assign(new Error("task " + name + " changed since you loaded it"), { status: 412, code: "precondition_failed", current: Object.assign({}, t) });
    };
    routes[base] = (u, init) => {
      if (!init.method || init.method === "GET") return t;
      stale();
      const patch = JSON.parse(init.body);
      if (state.cycle) {
        state.cycle = false;
        throw Object.assign(new Error("the edit would break the task graph: taskgraph: dependency cycle: auth-token -> session-relay -> auth-token"), { status: 422, code: "unprocessable", current: Object.assign({}, t) });
      }
      Object.assign(t, patch, { etag: '"tk-e' + Math.random().toString(16).slice(2, 6) + '"' });
      return { task: Object.assign({}, t), write: { path: "/o/gui-api/tasks/" + name + ".yaml", fields: patch }, expect: ["task.updated"] };
    };
    routes[base + "/actions/retry"] = (u, init) => {
      stale();
      const b = JSON.parse(init.body);
      Object.assign(t, { status: "ready", failure_reason: "", attempts: b.reset_attempts ? 0 : t.attempts, etag: '"tk-r1"' });
      Object.assign(project, { status: "working", blocked_reason: "", etag: '"pj-2222"' });
      return { task: Object.assign({}, t), dispatch: "unblocked", write: { path: "/o/gui-api/tasks/" + name + ".yaml" }, expect: ["task.status", "project.status", "session.started"] };
    };
    routes[base + "/actions/save-sandbox"] = (u, init) => {
      stale();
      const b = JSON.parse(init.body);
      t.capabilities = Object.assign({}, t.capabilities, { save_sandbox: b.save });
      t.etag = '"tk-s' + (b.save ? 1 : 0) + '"';
      return { task: Object.assign({}, t), write: { path: "/o/gui-api/tasks/" + name + ".yaml", fields: { save_sandbox: b.save } }, expect: ["task.updated"] };
    };
    routes[base + "/actions/open-session"] = () => ({ kind: "task session", target: "session-gui-api-" + name, note: "opened task session in a new tab of the daemon's terminal → session-gui-api-" + name });
    routes[base + "/actions/open-shell"] = () => ({ kind: "worktree", target: "worktree-gui-api-" + name, note: "opened worktree in a new tab of the daemon's terminal → worktree-gui-api-" + name });
  }
  routes["/api/v1/projects/p_0.gui-api/actions/open-shell"] = (u, init) => {
    const where = JSON.parse(init.body).where;
    return { kind: where, target: where + "-gui-api", note: "opened " + where + " in a new tab of the daemon's terminal → " + where + "-gui-api" };
  };
  routes["/api/v1/projects/p_0.gui-api/actions/open-session"] = () => ({ kind: "session", target: "session-gui-api", already_open: true, note: "the session tab for gui-api is already open; switched to it", noop: true });
  return { state, routes };
}

const writes = (requests) => requests.filter((r) => r.method !== "GET" && r.method !== "HEAD");
const byLabel = (main, l) => buttons(main).find((b) => label(b) === l);
const input = (main, aria) => main.find((e) => e.getAttribute("aria-label") === aria);

function page(scopes, url) {
  const { state, routes } = fixture(scopes);
  const p = boot({ webDir, scripts: SCRIPTS, url, routes });
  return Object.assign(p, { state });
}

(async () => {
  // ---- the project view's selected-task strip, with the write scope ----
  const pg = page(["read", "write", "answer"], "http://127.0.0.1:7078/projects/p_0.gui-api?task=auth-token");
  const { main, requests, settle, state } = pg;
  pg.start();
  await settle();
  pg.es.source.send({ seq: 10, type: "hello", data: { cursor: 10, epoch: "1" } });
  await settle();
  let text = main.textContent;
  assert.ok(!/null|undefined|\[object|NaN/.test(text), "stray value: " + text);

  for (const l of ["Retry…", "Keep sandbox ☐", "Edit task…", "Open in my terminal", "Shell: worktree"]) {
    assert.ok(byLabel(main, l) && !byLabel(main, l).disabled, l + " is not enabled");
  }
  assert.ok(byLabel(main, "Watch (ACP)").disabled, "Watch waits for the session relay (P.5)");

  // Retry… asks first: nothing is sent until "Retry task".
  const n0 = writes(requests).length;
  byLabel(main, "Retry…").dispatch("click");
  await settle();
  assert.equal(writes(requests).length, n0, "Retry… sent before the confirm");
  assert.match(main.textContent, /Retry auth-token: back to ready/);
  assert.match(main.textContent, /unblocks gui-api \(status: working, blocked_reason cleared\): its block names this task alone/);
  assert.ok(input(main, "reset attempts").checked, "reset attempts is not the default");
  input(main, "retry note").value = " flaky runner ";
  byLabel(main, "Retry task").dispatch("click");
  await settle();
  let w = writes(requests);
  assert.equal(w.length, n0 + 1);
  assert.equal(w[n0].method, "POST");
  assert.equal(w[n0].path, "/api/v1/projects/p_0.gui-api/tasks/auth-token/actions/retry");
  assert.equal(w[n0].headers["If-Match"], '"tk-1111"');
  assert.equal(w[n0].headers["X-WM-CSRF"], "csrf-1");
  assert.match(w[n0].headers["Idempotency-Key"], /^[0-9a-f]{32}$/);
  assert.deepEqual(w[n0].body, { reset_attempts: true, note: "flaky runner" });
  assert.match(main.textContent, /retry: ready again \(attempts 0\); the project is unblocked · watch for task\.status, project\.status, session\.started/);
  assert.ok(!byLabel(main, "Retry task"), "the confirm stayed open");
  // The task is ready now: no Retry… to offer.
  assert.ok(!byLabel(main, "Retry…"), "Retry… is still offered on a ready task");

  // Keep sandbox, then a concurrent write: 412, the task as it is now.
  state.stale = true;
  byLabel(main, "Keep sandbox ☐").dispatch("click");
  await settle();
  assert.match(main.textContent, /save-sandbox: the task changed since it was loaded/);
  assert.ok(byLabel(main, "Keep sandbox ☑"), "the 412's current task is not drawn");
  byLabel(main, "Keep sandbox ☑").dispatch("click");
  await settle();
  w = writes(requests);
  assert.equal(w[w.length - 1].path, "/api/v1/projects/p_0.gui-api/tasks/auth-token/actions/save-sandbox");
  assert.deepEqual(w[w.length - 1].body, { save: false });
  assert.equal(w[w.length - 1].headers["If-Match"], '"tk-9999"', "save-sandbox did not use the etag from the 412's current");
  assert.match(main.textContent, /save-sandbox: save_sandbox cleared/);

  // Edit task…: the form starts from the task; only what changed is sent.
  byLabel(main, "Edit task…").dispatch("click");
  await settle();
  assert.equal(input(main, "description").value, "Add the token.");
  assert.equal(input(main, "touches").value, "workingman/internal/api/**");
  const n1 = writes(requests).length;
  byLabel(main, "Save").dispatch("click");
  await settle();
  assert.equal(writes(requests).length, n1, "an unchanged edit was sent");
  assert.match(main.textContent, /edit: nothing changed/);
  byLabel(main, "Edit task…").dispatch("click");
  await settle();
  input(main, "description").value = "Add the token, then test it.";
  input(main, "areas").value = "api, go-deps";
  byLabel(main, "Save").dispatch("click");
  await settle();
  w = writes(requests);
  const edit = w[w.length - 1];
  assert.equal(edit.method, "PATCH");
  assert.equal(edit.path, "/api/v1/projects/p_0.gui-api/tasks/auth-token");
  assert.deepEqual(edit.body, { description: "Add the token, then test it.", areas: ["api", "go-deps"] });
  assert.equal(edit.headers["If-Match"], '"tk-s0"');
  assert.match(main.textContent, /edit: saved tasks\/auth-token\.yaml/);

  // A cycle is the server's 422, shown with its taskgraph text.
  byLabel(main, "Edit task…").dispatch("click");
  await settle();
  input(main, "depends_on").value = "session-relay";
  state.cycle = true;
  byLabel(main, "Save").dispatch("click");
  await settle();
  w = writes(requests);
  assert.deepEqual(w[w.length - 1].body, { depends_on: ["session-relay"] });
  assert.match(main.textContent, /edit: 422 unprocessable: the edit would break the task graph: taskgraph: dependency cycle/);
  assert.ok(byLabel(main, "Save"), "the form closed on a refusal");

  // Host tabs: the task's worktree shell, the project's orch shell, and an
  // already-open session tab.
  byLabel(main, "Shell: worktree").dispatch("click");
  await settle();
  w = writes(requests);
  assert.equal(w[w.length - 1].path, "/api/v1/projects/p_0.gui-api/tasks/auth-token/actions/open-shell");
  assert.match(main.textContent, /open-shell: opened worktree in a new tab of the daemon's terminal → worktree-gui-api-auth-token/);
  byLabel(main, "Shell: orch dir").dispatch("click");
  await settle();
  w = writes(requests);
  assert.equal(w[w.length - 1].path, "/api/v1/projects/p_0.gui-api/actions/open-shell");
  assert.deepEqual(w[w.length - 1].body, { where: "orch" });
  assert.match(main.textContent, /shell orch: opened orch in a new tab/);
  byLabel(main, "Open session").dispatch("click");
  await settle();
  assert.match(main.textContent, /open-session: the session tab for gui-api is already open; switched to it/);

  // session-relay has no worktree: its tabs are refused with why.
  await pg.navigate("/projects/p_0.gui-api?task=session-relay");
  assert.ok(byLabel(main, "Shell: worktree").disabled);
  assert.match(byLabel(main, "Shell: worktree").getAttribute("title"), /no worktree/);

  // ---- the sandbox card ----
  state.tasks["auth-token"].status = "failed";
  await pg.navigate("/projects/p_0.gui-api/tasks/auth-token");
  text = main.textContent;
  assert.ok(!/null|undefined|\[object|NaN/.test(text), "card: stray value: " + text);
  for (const l of ["Retry…", "Edit task…", "Open in my terminal"]) assert.ok(byLabel(main, l) && !byLabel(main, l).disabled, "card: " + l + " is not enabled");
  assert.ok(buttons(main).some((b) => label(b).startsWith("Keep sandbox") && !b.disabled), "card: Keep sandbox is not enabled");
  assert.ok(byLabel(main, "Shell in sandbox (pty scope)").disabled, "card: the pty shell is enabled");
  const n2 = writes(requests).length;
  byLabel(main, "Open in my terminal").dispatch("click");
  await settle();
  w = writes(requests);
  assert.equal(w.length, n2 + 1);
  assert.equal(w[n2].path, "/api/v1/projects/p_0.gui-api/tasks/auth-token/actions/open-session");
  assert.match(main.textContent, /open-session: opened task session/);
  byLabel(main, "Retry…").dispatch("click");
  await settle();
  input(main, "reset attempts").checked = false;
  byLabel(main, "Retry task").dispatch("click");
  await settle();
  w = writes(requests);
  assert.deepEqual(w[w.length - 1].body, { reset_attempts: false });
  assert.match(main.textContent, /retry: ready again/);

  // ---- read-only: all disabled with their TUI command, nothing sent ----
  const ro = page(["read"], "http://127.0.0.1:7078/projects/p_0.gui-api?task=auth-token");
  ro.start();
  await ro.settle();
  for (const l of ["Retry…", "Keep sandbox ☐", "Edit task…", "Open in my terminal", "Shell: worktree", "Shell: orch dir"]) {
    const b = byLabel(ro.main, l);
    assert.ok(b && b.disabled, "read-only: " + l + " is enabled");
    assert.match(b.getAttribute("title"), /in the TUI: /);
    b.dispatch("click");
  }
  await ro.navigate("/projects/p_0.gui-api/tasks/auth-token");
  for (const b of buttons(ro.main)) assert.ok(b.disabled, "read-only card: " + label(b) + " is enabled");
  assert.equal(writes(ro.requests).length, 0, "a read-only session sent a write");

  console.log("ok   task retry, keep sandbox, edit and the host tabs write from the GUI with the task's etag; 412 and 422 show what changed; read-only sessions send nothing");
})().catch((e) => {
  console.log("FAIL " + (e && e.stack ? e.stack : e));
  process.exit(1);
});
