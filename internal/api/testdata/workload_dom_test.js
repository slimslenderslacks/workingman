// A smoke test of the workload screens (internal/api/web/sandboxes.js and
// agents.js): it loads the real bundle into a fake browser (fakedom.js) over
// a fake API, renders the sandbox card of a running task, the Sandboxes list
// and the agents map, checks each wireframe's parts are there with every
// button disabled and no write sent, and that the card follows a stream
// event. Run by TestProjectViewModel (web_test.go):
//
//   node internal/api/testdata/workload_dom_test.js internal/api/web
"use strict";

const assert = require("node:assert/strict");
const path = require("node:path");
const { boot, buttons, label } = require("./fakedom.js");

const webDir = path.resolve(process.argv[2] || path.join(__dirname, "..", "web"));

const WT = "/Users/me/dev/workspaces/gui-api/.worktrees/session-relay";
const task = {
  project: "p_0.gui-api", name: "session-relay", status: "running", attempts: 0, failure_reason: "", blocked_reason: "",
  description: "Implement the WS relay described in §A.5.2.",
  depends_on: ["snapshot-model"], touches: ["internal/api/relay.go"], areas: ["internal/api"], exclusive: false,
  capabilities: {
    model: "default", resolved_model: "claude-opus-5-5", harness: "claude-acp", resolved_harness: "slimslenderslacks/claude-code-acp:0.90.0",
    static_mcps: ["github"], policies: [{ action: "deny", type: "network", resource: "**" }, { action: "allow", type: "network", resource: "api.github.com" }], save_sandbox: false,
  },
  source: null, worktree: WT, task_branch: "wm/gui-api/session-relay", base: [{ repo: "workingman", hash: "8be04d1aaaa" }],
  succeeded_at: null, completed_at: null, queue_position: 0, waiting_on: [], integration: null,
  output: { summary: "", commits: [], created_files: [], touches_actual: [] },
  session: "task-session-relay", sandbox: "gui-api-session-relay", commit_sandbox: "gui-api-session-relay-commit",
};
const tasks = [task, Object.assign({}, task, { name: "snapshot-model", status: "committed", depends_on: [], session: "", sandbox: "gui-api-snapshot-model" }),
  Object.assign({}, task, { name: "auth-token", status: "failed", depends_on: [], session: "", sandbox: "gui-api-auth-token", capabilities: Object.assign({}, task.capabilities, { save_sandbox: true }) })];
const project = { id: "p_0.gui-api", work_stream: "gui-api", status: "working", branch: "gui-api", verify: "go build ./...", live: { integrating: "" }, flags: {}, task_counts: {}, task_total: 3 };
const session = {
  id: "task-session-relay", kind: "task", project: "p_0.gui-api", task: "session-relay", started_at: new Date(Date.now() - 6 * 60000).toISOString(),
  persistent: false, status: "running", prompt_count: 1, acp: { attachable: true },
  sandbox: { name: "gui-api-session-relay", live: true, kit: "/Users/me/kits/acp", image: "slimslenderslacks/claude-code-acp:0.90.0", workspaces: [WT] },
  gateway: { requested: ["github"], servers: ["github"], dynamic: true },
};
const stepUp = { id: "step-up-agent", kind: "step-up", persistent: true, status: "running", acp: { attachable: true }, started_at: new Date().toISOString(), sandbox: { name: "step-up", live: true, workspaces: ["/Users/me/.workingman/step-up/requests"] }, gateway: { requested: ["post-office"], servers: ["post-office"], dynamic: false } };
let sessions = [session, stepUp];
const sandboxes = [
  { name: "gui-api-session-relay", managed: true, retained: false, session: "task-session-relay", kind: "task", project: "p_0.gui-api", task: "session-relay", role: "task", task_status: "running", image: "slimslenderslacks/claude-code-acp:0.90.0", workspaces: [WT], gateway: session.gateway, mcp_dynamic: true },
  { name: "gui-api-auth-token", managed: true, retained: true, project: "p_0.gui-api", task: "auth-token", role: "task", task_status: "failed", image: "img:1", workspaces: [], gateway: null, mcp_dynamic: false, created_at: "2026-11-25T07:00:00Z" },
  { name: "session-gui-api", managed: true, retained: true, project: "p_0.gui-api", role: "session", workspaces: [], gateway: null, mcp_dynamic: false },
  { name: "step-up", managed: true, retained: false, session: "step-up-agent", kind: "step-up", role: "step-up", workspaces: [], gateway: stepUp.gateway, mcp_dynamic: false },
  { name: "someone-elses", managed: false, retained: false, workspaces: [], gateway: null, mcp_dynamic: false },
];
const audit = [{ seq: 5, time: "2026-11-25T08:59:00Z", event: "step_up_op", fields: { id: "op_1", op: "sandbox_network_allow", target: "gui-api-session-relay", resource: "proxy.golang.org", status: "applied", detail: "sbx policy allow network proxy.golang.org; added to policies:" } }];

const routes = {
  "/api/v1/session": () => ({ auth: "cookie", csrf: "c", scopes: ["read"] }),
  "/api/v1/status": () => ({ daemon: { roots: ["/Users/me/orch"] }, cursor: 10 }),
  "/api/v1/projects": () => ({ items: [project], cursor: 10 }),
  "/api/v1/projects/p_0.gui-api": () => project,
  "/api/v1/projects/p_0.gui-api/tasks": () => ({ items: tasks, cursor: 10 }),
  "/api/v1/projects/p_0.gui-api/tasks/session-relay": () => task,
  "/api/v1/projects/p_0.gui-api/tasks/auth-token": () => tasks[2],
  "/api/v1/projects/p_0.gui-api/graph": () => ({ nodes: [], edges: [], max_attempts: 3 }),
  "/api/v1/sessions": () => ({ items: sessions, cursor: 10 }),
  "/api/v1/sessions/task-session-relay/prompt": () => ({ session: "task-session-relay", dir: WT, instructions_md: "You are a task agent for the orchestrator.\nYour task: session-relay\n", context_yaml: "kind: task\ntask_base: workingman@8be04d1\n" }),
  "/api/v1/sandboxes": () => ({ items: sandboxes, cursor: 10 }),
  "/api/v1/audit": () => ({ items: audit, cursor: 10 }),
  "/api/v1/agents": () => ({
    items: [
      { kind: "workingman", enabled: true, session: "workingman-agent", sandbox: "workingman-agent", mcps: ["github"], mcp_dynamic: true },
      { kind: "sleepy-head", enabled: true, mcps: [], mcp_dynamic: false, pending_catchups: ["nightly-deps"] },
      { kind: "step-up", enabled: true, session: "step-up-agent", sandbox: "step-up", mcps: ["post-office"], mcp_dynamic: false },
    ],
    cursor: 10,
  }),
  "/api/v1/messages": () => ({ items: [], cursor: 10 }),
};

const page = boot({
  webDir,
  scripts: ["app.js", "graph.js", "projects.js", "workload.js", "sandboxes.js", "agents.js"],
  url: "http://127.0.0.1:7078/projects/p_0.gui-api/tasks/session-relay",
  routes,
  skip: (fn) => String(fn).includes("pollTranscript(tr)"),
});
const { main, requests, settle } = page;

function checkButtons(where) {
  const bs = buttons(main);
  assert.ok(bs.length > 0, where + ": no buttons");
  for (const b of bs) {
    assert.ok(b.disabled, where + ": " + b.textContent + " is not disabled");
    assert.match(b.getAttribute("title"), /in the TUI: /, where + ": " + b.textContent);
  }
  return bs.map(label);
}

function noStray(where) {
  const text = main.textContent;
  assert.ok(!/null|undefined|\[object|NaN/.test(text), where + ": stray value rendered: " + text);
  return text;
}

(async () => {
  page.start();
  await settle();
  page.es.source.send({ seq: 10, type: "hello", data: { cursor: 10, epoch: "1" } });
  await settle();

  // ---- the sandbox card (§G.3) ----
  let text = noStray("card");
  assert.match(text, /gui-api-session-relay/);
  assert.match(text, /live \(sbx ls\)/);
  assert.match(text, /4 grants beyond baseline · 1 open-ended/);
  for (const part of ["① INPUTS", "② CAPABILITIES", "③ OUTPUT", "SANDBOX BOUNDARY — sbx microVM"]) assert.ok(text.includes(part), "missing " + part);
  // Inputs.
  assert.match(text, /You are a task agent for the orchestrator\./);
  assert.match(text, /task_base: workingman@8be04d1/);
  assert.match(text, /snapshot-model ✓/);
  assert.match(text, /internal\/api\/relay\.go/);
  assert.match(text, /planning agent \(planning\.tmpl\)/);
  assert.match(text, /orch roots, ~\/\.ssh, ~\/\.workingman/);
  // Capabilities.
  assert.match(text, /slimslenderslacks\/claude-code-acp:0\.90\.0/);
  assert.match(text, /default → claude-opus-5-5/);
  const chips = main.findAll((e) => e.className.split(" ").includes("chip") && !e.className.includes("card-legend"));
  const chip = (cls) => chips.filter((c) => c.className.includes("chip-" + cls)).map((c) => c.textContent);
  assert.ok(chip("granted").includes("github"));
  assert.ok(chip("absent").includes("post-office") && chip("absent").includes("logseq"));
  assert.ok(chip("dynamic").includes("⚠ dynamic: mcp-add"));
  assert.match(text, /deny network \*\*/);
  assert.match(text, /allow network api\.github\.com/);
  assert.match(text, /Skills.*none/s);
  assert.match(text, /network allow proxy\.golang\.org → applied/);
  // Output.
  assert.match(text, /wm\/gui-api\/session-relay/);
  assert.match(text, /verify: go build \.\/\.\.\./);
  assert.match(text, /gui-api-session-relay-commit/);
  assert.match(text, /gui-api {2}\(project branch\)/);
  assert.match(text, /removed on success; kept on failed\/blocked/);
  let labels = checkButtons("card");
  for (const l of ["Watch (ACP)", "Prompt (persistent only)", "Open in my terminal", "Shell in sandbox (pty scope)", "Queue intake", "Edit task…", "Keep sandbox ☐"]) {
    assert.ok(labels.includes(l), "card: missing button " + l + " in " + labels.join(", "));
  }
  assert.ok(requests.some((r) => r.path === "/api/v1/sessions/task-session-relay/prompt"));

  // Live: the task succeeds and its session ends; the card follows.
  task.status = "success";
  task.queue_position = 1;
  task.session = "";
  sessions = [stepUp];
  page.es.source.send({ seq: 11, type: "task.updated", project: "p_0.gui-api", data: { name: "session-relay", task } });
  page.es.source.send({ seq: 12, type: "session.ended", project: "p_0.gui-api", data: { id: "task-session-relay" } });
  await settle();
  text = noStray("card after");
  assert.match(text, /success: in the commit queue, not yet landed/);
  assert.match(text, /queued #1 \(succeeded_at\)/);
  assert.match(text, /no live session/);

  // ---- the Sandboxes list (§G.2) ----
  await page.navigate("/sandboxes");
  text = noStray("sandboxes");
  assert.match(text, /Sandboxes/);
  for (const n of ["gui-api-session-relay", "gui-api-auth-token", "session-gui-api", "step-up", "someone-elses"]) assert.ok(text.includes(n), "sandboxes: missing " + n);
  assert.match(text, /retained · save_sandbox/);
  assert.match(text, /retained · :session sandbox \(nothing removes these\)/);
  assert.match(text, /not made by workingman/);
  // Both ways: the task links to its sandbox card, and back to the project view.
  const links = main.findAll((e) => e.tagName === "A").map((a) => a.getAttribute("href"));
  assert.ok(links.includes("/projects/p_0.gui-api/tasks/auth-token"), links.join(" "));
  assert.ok(links.includes("/projects/p_0.gui-api?task=auth-token"), links.join(" "));
  assert.ok(links.includes("/sandboxes/step-up"));
  labels = checkButtons("sandboxes");
  assert.ok(labels.includes("Shell in sandbox (pty scope)"));
  const rows = main.findAll((e) => e.tagName === "TR" && e.className.includes("sb-row"));
  assert.equal(rows.length, 5);

  // The card of a retained sandbox, by name, resolves to its task.
  await page.navigate("/sandboxes/gui-api-auth-token");
  text = noStray("retained card");
  assert.match(text, /gui-api-auth-token/);
  assert.match(text, /retained now \(save_sandbox\)/);
  assert.match(text, /Keep sandbox ☑/);
  // A helper's card shows its kind's facts.
  await page.navigate("/sandboxes/step-up");
  text = noStray("step-up card");
  assert.match(text, /stepUpPlan/);
  assert.match(text, /typed change requests/);

  // ---- the agents map (§G.4) ----
  await page.navigate("/agents");
  text = noStray("agents");
  for (const part of ["Workload helpers", "workingman agent", "sleepy-head", "step-up", "Project workflow agents", "planning", "commit", "review", "archive", "wolf (interactive)", "project branch", "Gaps the GUI shows honestly"]) {
    assert.ok(text.includes(part), "agents: missing " + part);
  }
  assert.match(text, /orch roots rw/);
  assert.match(text, /behind: nightly-deps/);
  assert.match(text, /1 step-up op in the audit ring \(1 applied\)/);
  assert.match(text, /chooses per-task capabilities/);
  assert.match(text, /hard-coded: github \+ 2 hosts/);
  assert.match(text, /catchup-request → cron replan/);
  assert.match(text, /capability review gate/);
  // A live helper links to its sandbox card.
  assert.ok(main.findAll((e) => e.tagName === "A").some((a) => a.getAttribute("href") === "/sandboxes/step-up"));

  assert.ok(!requests.some((r) => r.method !== "GET" && r.method !== "HEAD"), "a screen sent a write");
  console.log("ok   sandbox card, Sandboxes list and agents map render from the API");
})().catch((e) => {
  console.log("FAIL " + (e && e.stack ? e.stack : e));
  process.exit(1);
});
