// Tests for the workload screens' model, internal/api/web/workload.js. Run
// by TestProjectViewModel (web_test.go) with node, or by hand:
//
//   node internal/api/testdata/workload_test.js internal/api/web/workload.js
"use strict";

const assert = require("node:assert/strict");
const path = require("node:path");

const wl = require(path.resolve(process.argv[2] || path.join(__dirname, "..", "web", "workload.js")));

const tests = [];
const test = (name, fn) => tests.push({ name, fn });

// The wireframe's workload (docs/proposals/workingman-gui/sandbox-card.svg):
// the running session-relay task of gui-api.
const ROOT = "/Users/me/orch";
const WT = "/Users/me/dev/workspaces/gui-api/.worktrees/session-relay";
function fixture() {
  const task = {
    project: "p_0.gui-api", name: "session-relay", status: "running", attempts: 0,
    description: "Implement the WS relay described in §A.5.2.",
    depends_on: ["snapshot-model"], touches: ["internal/api/relay.go"], areas: ["internal/api"], exclusive: false,
    capabilities: {
      model: "default", resolved_model: "claude-opus-5-5", harness: "claude-acp", resolved_harness: "slimslenderslacks/claude-code-acp:0.90.0",
      static_mcps: ["github"], policies: [{ action: "deny", type: "network", resource: "**" }, { action: "allow", type: "network", resource: "api.github.com" }], save_sandbox: false,
    },
    source: null, worktree: WT, task_branch: "wm/gui-api/session-relay", base: [{ repo: "workingman", hash: "8be04d1aaaa" }],
    queue_position: 0, waiting_on: [], integration: null, output: { summary: "", commits: [], created_files: [], touches_actual: [] },
    session: "task-session-relay", sandbox: "gui-api-session-relay", commit_sandbox: "gui-api-session-relay-commit",
  };
  const tasks = new Map([
    ["session-relay", task],
    ["snapshot-model", { name: "snapshot-model", status: "committed" }],
  ]);
  const session = {
    id: "task-session-relay", kind: "task", project: "p_0.gui-api", task: "session-relay", started_at: "2026-11-25T09:24:00Z",
    persistent: false, status: "running", acp: { attachable: true },
    sandbox: { name: "gui-api-session-relay", live: true, kit: "/Users/me/kits/acp", image: "slimslenderslacks/claude-code-acp:0.90.0", workspaces: [WT] },
    gateway: { requested: ["github"], servers: ["github"], dynamic: true },
  };
  const project = { id: "p_0.gui-api", work_stream: "gui-api", branch: "gui-api", verify: "go build ./...", live: { integrating: "" } };
  const prompt = { instructions_md: "You are a task agent for the orchestrator.\n\nYour task: session-relay\n", context_yaml: "kind: task\ntask_base: workingman@8be04d1\n" };
  return { task, tasks, session, project, prompt };
}

test("card: the wireframe's three columns from live resources", () => {
  const f = fixture();
  const c = wl.card({ task: f.task, tasks: f.tasks, project: f.project, session: f.session, sandbox: null, prompt: f.prompt, audit: [], roots: [ROOT], maxAttempts: 3 });
  assert.equal(c.name, "gui-api-session-relay");
  assert.equal(c.kind, "task");
  assert.equal(c.header.live, "live (sbx ls)");
  // ① inputs
  assert.deepEqual(c.inputs.prompt.instructions.lines, ["You are a task agent for the orchestrator.", "Your task: session-relay"]);
  const rows = Object.fromEntries(c.inputs.task.rows);
  assert.equal(rows.status, "running · attempts 0/3");
  assert.equal(rows.touches, "internal/api/relay.go");
  assert.equal(rows.source, "— (planned, not a PR fix)");
  assert.equal(rows.base, "workingman 8be04d1");
  assert.equal(rows["chosen by"], "planning agent (planning.tmpl)");
  assert.deepEqual(c.inputs.task.deps, [{ name: "snapshot-model", status: "committed", mark: "✓" }]);
  assert.deepEqual(c.inputs.mounts.rows, [{ mode: "rw", path: WT, open: false, note: "" }]);
  assert.deepEqual(c.inputs.mounts.absent, ["orch roots", "~/.ssh", "~/.workingman"]);
  // ② capabilities
  assert.equal(c.caps.harness.image, "slimslenderslacks/claude-code-acp:0.90.0");
  assert.equal(c.caps.harness.kit, "acp");
  assert.deepEqual(c.caps.model, { model: "default", resolved: "claude-opus-5-5" });
  assert.deepEqual(c.caps.mcp.chips.map((x) => x.name + ":" + x.state), ["github:granted", "post-office:absent", "logseq:absent", "mcp-add:dynamic"]);
  assert.deepEqual(c.caps.network.map((r) => r.action + " " + r.resource + " " + r.cls), [
    "deny ** deny",
    "allow api.github.com allow",
    "allow host.docker.internal:<gateway port> baseline",
    "allow llm proxy baseline",
  ]);
  assert.equal(c.caps.skills.startsWith("none"), true);
  // The wireframe's count: github MCP, mcp-add, api.github.com, the rw
  // worktree mount; mcp-add is the open-ended one.
  assert.equal(c.header.grants.count, 4);
  assert.equal(c.header.grants.open, 1);
  // ③ output
  assert.equal(c.output.branch, "wm/gui-api/session-relay");
  assert.match(c.output.result, /^running → success \| failed \| blocked/);
  assert.equal(c.output.commitSandbox, "gui-api-session-relay-commit");
  assert.equal(c.output.landsOn, "gui-api");
  assert.equal(c.output.stepper.steps[2].label, "verify: go build ./...");
  assert.ok(c.output.stepper.steps.every((s) => s.state === "todo"));
  assert.equal(c.output.after.now, "live session: the sandbox is in use");
  assert.equal(c.output.touches, "");
  // Actions: every one carries a TUI command; the shell needs the pty scope.
  assert.deepEqual(c.actions.map((a) => a.label), ["Watch (ACP)", "Prompt (persistent only)", "Open in my terminal", "Shell in sandbox (pty scope)"]);
  // On a task's card Open in my terminal is its open-session (§A.4.6).
  assert.equal(c.actions[2].action, "open-session");
  assert.equal(c.inputActions.find((a) => a.label === "Edit task…").action, "edit");
  for (const a of c.actions) assert.ok(a.command, a.label);
  assert.equal(c.actions[0].refusal, "");
  assert.match(c.actions[1].refusal, /^persistent sessions only: task sessions are one-shot/);
  assert.equal(c.inputActions.find((a) => a.label === "Edit task…").refusal, "the task is running: its inputs cannot change now");
});

test("card: absence and open-ended grants", () => {
  const f = fixture();
  // No task rules: sbx's balanced policy, which is open-ended.
  f.task.capabilities.policies = [];
  let c = wl.card({ task: f.task, tasks: f.tasks, project: f.project, session: f.session, roots: [ROOT] });
  assert.equal(c.caps.network[0].cls, "open");
  assert.equal(c.header.grants.open, 2);
  // An allow ** is open-ended; a static gateway has no mcp-add.
  f.task.capabilities.policies = [{ action: "allow", type: "network", resource: "**" }];
  f.session.gateway = { requested: ["github", "post-office"], servers: ["github", "post-office"], dynamic: false };
  c = wl.card({ task: f.task, tasks: f.tasks, project: f.project, session: f.session, roots: [ROOT] });
  assert.equal(c.caps.network[0].cls, "open");
  assert.deepEqual(c.caps.mcp.chips.map((x) => x.name + ":" + x.state), ["github:granted", "post-office:granted", "logseq:absent"]);
  assert.ok(c.caps.mcp.notes.includes("static gateway: no mcp-find / mcp-add"));
  // A writable orch root is open-ended; a read-only one is not.
  f.session.sandbox.workspaces = [ROOT + "/gui-api", "/Users/me/.workingman/state:ro"];
  c = wl.card({ task: f.task, tasks: f.tasks, project: f.project, session: f.session, roots: [ROOT] });
  assert.deepEqual(c.inputs.mounts.rows.map((r) => r.mode + " " + r.open), ["rw true", "ro false"]);
  assert.deepEqual(c.inputs.mounts.absent, ["~/.ssh"]);
});

test("card: without a session, the task's request stands in", () => {
  const f = fixture();
  f.task.status = "failed";
  f.task.session = "";
  f.task.capabilities.save_sandbox = true;
  const sb = { name: "gui-api-session-relay", managed: true, retained: true, image: "img:1", workspaces: [WT], gateway: null, role: "task", project: "p_0.gui-api", task: "session-relay" };
  const c = wl.card({ task: f.task, tasks: f.tasks, project: f.project, session: null, sandbox: sb, prompt: null, roots: [ROOT] });
  assert.equal(c.inputs.prompt, null);
  assert.equal(c.caps.mcp.source, "static_mcps (no gateway now: requested, not live)");
  assert.deepEqual(c.caps.mcp.granted, ["github"]);
  assert.equal(c.output.after.now, "retained now (save_sandbox)");
  assert.equal(c.output.after.keep, true);
  assert.match(c.actions[0].refusal, /no live ACP session/);
});

test("card: a helper sandbox shows its kind's facts", () => {
  const s = { id: "step-up-agent", kind: "step-up", persistent: true, acp: { attachable: true }, sandbox: { name: "step-up", live: true, workspaces: ["/Users/me/.workingman/step-up/requests"] }, gateway: { servers: ["post-office"], requested: ["post-office"], dynamic: false } };
  const c = wl.card({ session: s, roots: [ROOT] });
  assert.equal(c.output, null);
  assert.equal(c.facts.title, "step-up");
  assert.match(c.outputNote, /typed change requests/);
  assert.match(c.caps.policyNote, /stepUpPlan/);
  assert.equal(c.actions[1].label, "Prompt");
  assert.equal(c.actions[3].refusal, "never for workingman-agent or step-up");
});

test("runtime changes: step_up_op events for this sandbox only", () => {
  const ev = (seq, target, op, status) => ({ seq, time: "2026-11-25T08:59:00Z", event: "step_up_op", fields: { id: "op" + seq, op, target, resource: "proxy.golang.org", status, detail: "sbx policy allow network" } });
  const got = wl.runtimeChanges([
    ev(1, "gui-api-session-relay", "sandbox_network_allow", "applied"),
    ev(2, "other", "sandbox_network_allow", "applied"),
    ev(3, "gui-api-session-relay", "list_sandbox_policy", "applied"),
    { seq: 4, event: "task_running", fields: { target: "gui-api-session-relay" } },
  ], "gui-api-session-relay");
  assert.deepEqual(got.map((r) => [r.seq, r.op, r.status]), [[1, "network allow", "applied"]]);
});

test("stepper: queued, integrating, landed, failed", () => {
  const f = fixture();
  const states = (t, p) => wl.stepper(t, p || f.project).steps.map((s) => s.state).join(",");
  f.task.status = "success";
  f.task.queue_position = 2;
  assert.equal(states(f.task), "active,todo,todo,todo,todo");
  assert.equal(wl.stepper(f.task, f.project).steps[0].label, "queued #2 (succeeded_at)");
  assert.equal(states(f.task, Object.assign({}, f.project, { live: { integrating: "session-relay" } })), "done,active,active,active,todo");
  f.task.status = "committed";
  assert.equal(states(f.task), "done,done,done,done,done");
  f.task.status = "ready";
  f.task.integration = { result: "verify_failed" };
  assert.equal(states(f.task), "done,done,failed,todo,todo");
  f.task.integration = { result: "conflict", files: ["a.go"] };
  assert.equal(states(f.task), "done,failed,todo,todo,todo");
});

test("inputs: chosen by, source, touches_exceeded", () => {
  assert.equal(wl.chosenBy({ source: { kind: "pr-comment", ref: "PRRT_1", pr: 7 } }), "review agent (review.tmpl), from pr-comment PRRT_1 on PR #7");
  assert.equal(wl.chosenBy({ seed: true }).startsWith("a human"), true);
  assert.equal(wl.sourceText({ source: { kind: "pr-check", ref: "ci", repo: "o/r", pr: 3 } }), "pr-check ci · o/r #3");
  assert.equal(wl.touchesWarning({ output: { touches_actual: ["workingman/x.go"] } }), "touches_exceeded: wrote outside its declared area: workingman/x.go");
  assert.deepEqual(wl.depStates({ depends_on: ["a", "b", "c"] }, new Map([["a", { status: "committed" }], ["b", { status: "failed" }]])).map((d) => d.mark), ["✓", "✗", "✗"]);
  assert.deepEqual(wl.promptPreview("a\n\nb\nc\n", 2), { lines: ["a", "b"], more: true, total: 3 });
});

test("sandbox rows: task → sandbox both ways, retained and foreign", () => {
  const f = fixture();
  const sandboxes = [
    { name: "gui-api-session-relay", managed: true, retained: false, session: "task-session-relay", kind: "task", project: "p_0.gui-api", task: "session-relay", role: "task", image: "img:1", workspaces: [WT], gateway: f.session.gateway },
    { name: "gui-api-auth-token", managed: true, retained: true, project: "p_0.gui-api", task: "auth-token", role: "task", task_status: "failed", workspaces: [], gateway: null, created_at: "2026-11-25T07:00:00Z" },
    { name: "session-gui-api", managed: true, retained: true, project: "p_0.gui-api", role: "session", workspaces: [] },
    { name: "someone-elses", managed: false, retained: false, workspaces: [] },
  ];
  const gone = { id: "planning-gui-api", kind: "planning", project: "p_0.gui-api", started_at: "2026-11-25T09:00:00Z", sandbox: { name: "gui-api", live: false, workspaces: [] }, gateway: null };
  const tasks = new Map([["p_0.gui-api/session-relay", f.task]]);
  const rows = wl.sandboxRows(sandboxes, [f.session, gone], tasks, Date.parse("2026-11-25T09:30:00Z"));
  assert.deepEqual(rows.map((r) => r.name + ":" + r.cls), [
    "gui-api-session-relay:live",
    "gui-api:gone",
    "gui-api-auth-token:retained",
    "session-gui-api:retained",
    "someone-elses:foreign",
  ]);
  const live = rows[0];
  assert.equal(live.session, "task-session-relay");
  assert.equal(live.task, "session-relay");
  assert.equal(live.age, "6m");
  assert.equal(live.grants.count, 4);
  assert.equal(live.attachable, true);
  assert.equal(rows[2].state, "retained · task failed");
  assert.equal(rows[2].age, "2h30");
  assert.equal(rows[3].state, "retained · :session sandbox (nothing removes these)");
  assert.equal(rows[4].state, "not made by workingman");
  // sbx ls down: the sessions are still listed.
  const down = wl.sandboxRows(null, [f.session], tasks, Date.now());
  assert.deepEqual(down.map((r) => r.cls), ["unknown"]);
});

test("agents map: helpers above the workflow, live sessions placed", () => {
  const agents = [
    { kind: "workingman", enabled: true, session: "workingman-agent", sandbox: "workingman-agent", mcps: ["github"], mcp_dynamic: true },
    { kind: "sleepy-head", enabled: true, mcps: [], pending_catchups: ["nightly-deps"] },
    { kind: "step-up", enabled: true, session: "step-up-agent", sandbox: "step-up", mcps: ["post-office"], mcp_dynamic: false },
  ];
  const sessions = [
    { id: "task-session-relay", kind: "task", project: "p_0.gui-api", task: "session-relay", sandbox: { name: "gui-api-session-relay" } },
    { id: "task-other", kind: "task", project: "p_0.docs", task: "x", sandbox: { name: "docs-x" } },
    { id: "planning-gui-api", kind: "planning", project: "p_0.gui-api", sandbox: { name: "gui-api" } },
  ];
  const audit = [{ event: "step_up_op", fields: { status: "applied" } }, { event: "step_up_op", fields: { status: "rejected" } }];
  const m = wl.agentsMap(agents, sessions, { id: "p_0.gui-api", work_stream: "gui-api", branch: "gui-api" }, audit);
  assert.deepEqual(m.helpers.map((a) => a.kind + ":" + a.state), ["workingman:running", "sleepy-head:enabled, not running", "step-up:running"]);
  assert.ok(m.helpers[0].chips.some((c) => c.text === "orch roots rw" && c.cls === "open"));
  assert.ok(m.helpers[0].chips.some((c) => c.text === "dynamic MCPs"));
  assert.deepEqual(m.helpers[1].extra, ["behind: nightly-deps"]);
  assert.deepEqual(m.helpers[2].extra, ["2 step-up ops in the audit ring (1 applied)"]);
  assert.match(m.helpers[2].gap, /cannot apply a grant itself/);
  assert.deepEqual(m.workflow.map((a) => a.kind), ["project", "planning", "task", "commit", "review", "archive", "wolf"]);
  const wf = Object.fromEntries(m.workflow.map((a) => [a.kind, a]));
  assert.equal(wf.planning.sandbox, "gui-api");
  assert.equal(wf.task.sandbox, "gui-api-<task>");
  assert.equal(wf.commit.sandbox, "gui-api-<task>-commit");
  assert.match(wf.planning.highlight, /chooses per-task capabilities/);
  // Only the selected project's sessions.
  assert.deepEqual(wf.task.live.map((s) => s.task), ["session-relay"]);
  assert.equal(wf.planning.live.length, 1);
  assert.equal(m.branch, "gui-api");
  assert.ok(m.edges.some((e) => e.from === "sleepy-head" && /catchup-request/.test(e.via)));
  // Every project: every session, generic sandbox names.
  const all = wl.agentsMap(agents, sessions, null, []);
  assert.equal(all.workflow.find((a) => a.kind === "task").live.length, 2);
  assert.equal(all.workflow.find((a) => a.kind === "planning").sandbox, "<ws>");
  // A disabled helper.
  const off = wl.agentsMap([], [], null, []);
  assert.deepEqual(off.helpers.map((a) => a.state), ["off", "off", "off"]);
});

let failed = 0;
for (const t of tests) {
  try {
    t.fn();
    console.log("ok   " + t.name);
  } catch (e) {
    failed++;
    console.log("FAIL " + t.name + "\n" + (e && e.stack ? e.stack : e));
  }
}
console.log(`${tests.length - failed}/${tests.length} passed`);
process.exit(failed ? 1 : 0);
