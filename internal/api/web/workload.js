// workingman GUI: the model of the workload screens
// (docs/proposals/workingman-gui.md §G.2–§G.4). Pure functions over the
// API's resources, with no DOM: the sandbox card's three columns (inputs →
// capabilities → output) and its grant count, the Sandboxes list (task →
// sandbox mapping, both ways), and the agents map with the capability facts
// of each agent kind. sandboxes.js and agents.js draw what these return.
//
// Like graph.js, the file runs in the browser, where it becomes wm.wl, and
// under node for its tests (internal/api/testdata/workload_test.js).
"use strict";

(function (factory) {
  const wl = factory();
  if (typeof module === "object" && module.exports) module.exports = wl;
  else window.wm.wl = wl;
})(function () {
  // KNOWN_MCPS are catalog servers the GUI knows by name (planning.tmpl
  // names them). The API cannot list the gateway's catalog, so a card
  // draws these, when not granted, as the visible absences of §G.0
  // principle 3.
  const KNOWN_MCPS = ["github", "post-office", "logseq"];

  // RO is runner.ReadOnlySuffix: a mount that ends with it is read-only.
  const RO = ":ro";

  const base = (p) => String(p || "").replace(/\/+$/, "").split("/").pop();
  const within = (path, dir) => path === dir || path.startsWith(dir.replace(/\/+$/, "") + "/");

  // ---- ① inputs ------------------------------------------------------------

  // mounts splits a sandbox's workspaces into the rows the card lists and
  // the sensitive host paths that are not mounted (§G.3). roots are the
  // daemon's orch roots. A writable mount inside an orch root, ~/.ssh or
  // ~/.workingman is open-ended and drawn amber.
  function mounts(workspaces, roots) {
    const rows = [];
    for (const w of workspaces || []) {
      const ro = w.endsWith(RO);
      const path = ro ? w.slice(0, -RO.length) : w;
      const root = (roots || []).find((r) => r && within(path, r));
      const secret = /\/\.ssh(\/|$)|\/\.workingman(\/|$)/.test(path);
      rows.push({ mode: ro ? "ro" : "rw", path, open: !ro && (!!root || secret), note: root ? "orch root" : secret ? "credentials / state" : "" });
    }
    const paths = rows.map((r) => r.path);
    const absent = [];
    const roots_ = (roots || []).filter(Boolean);
    if (roots_.length && !roots_.some((r) => paths.some((p) => within(p, r) || within(r, p)))) absent.push("orch roots");
    if (!paths.some((p) => /\/\.ssh(\/|$)/.test(p))) absent.push("~/.ssh");
    if (!paths.some((p) => /\/\.workingman(\/|$)/.test(p))) absent.push("~/.workingman");
    return { rows, absent };
  }

  // depStates is a task's depends_on with each dependency's state.
  function depStates(t, tasks) {
    return (t.depends_on || []).map((name) => {
      const d = tasks && tasks.get(name);
      const status = d ? d.status : "missing";
      const mark = status === "committed" ? "✓" : status === "failed" || status === "blocked" || status === "missing" ? "✗" : "⧗";
      return { name, status, mark };
    });
  }

  // chosenBy says who wrote the task's inputs (§G.3): the review agent for
  // PR fix and push tasks, a human for a seed, else the planning agent.
  function chosenBy(t) {
    if (t.seed) return "a human (seed task: the planner names it)";
    const src = t.source;
    if (src && src.kind) return `review agent (review.tmpl), from ${src.kind} ${src.ref || ""}${src.pr ? " on PR #" + src.pr : ""}`.replace(/\s+/g, " ");
    return "planning agent (planning.tmpl)";
  }

  // sourceText is the task's source line: which PR signal it addresses.
  function sourceText(t) {
    const src = t.source;
    if (!src || !src.kind) return "— (planned, not a PR fix)";
    return `${src.kind} ${src.ref || ""}${src.repo ? " · " + src.repo : ""}${src.pr ? " #" + src.pr : ""}`.replace(/\s+/g, " ").trim();
  }

  // promptPreview is the first n non-blank lines of a prompt file, and
  // whether more follow.
  function promptPreview(text, n) {
    const lines = String(text || "").split("\n").filter((l) => l.trim());
    return { lines: lines.slice(0, n), more: lines.length > n, total: lines.length };
  }

  // ---- ② capabilities ------------------------------------------------------

  // mcpChips is the MCP row: the granted servers, the known catalog servers
  // that were not granted, and the dynamic gateway's open-ended mcp-add.
  // gateway is the session's or sandbox's gateway record (null when there
  // is none); staticMcps the task's request.
  function mcpChips(gateway, staticMcps) {
    let granted, source;
    if (gateway) {
      granted = gateway.servers || [];
      source = "gateway";
    } else if (staticMcps && staticMcps.length) {
      granted = staticMcps;
      source = "static_mcps (no gateway now: requested, not live)";
    } else {
      granted = [];
      source = "no gateway now: a task without static_mcps gets the host's default set";
    }
    const chips = granted.map((name) => ({ name, state: "granted" }));
    for (const name of KNOWN_MCPS) if (!granted.includes(name)) chips.push({ name, state: "absent" });
    if (gateway && gateway.dynamic) chips.push({ name: "mcp-add", state: "dynamic" });
    const notes = [];
    if (staticMcps) notes.push("static_mcps: [" + staticMcps.join(", ") + "]");
    if (gateway && gateway.requested && gateway.requested.join() !== granted.join()) notes.push("requested: [" + gateway.requested.join(", ") + "]");
    if (gateway && gateway.dynamic) notes.push("dynamic gateway: the agent can mcp-add any catalog server at runtime, so static_mcps is a floor, not a ceiling");
    else if (gateway) notes.push("static gateway: no mcp-find / mcp-add");
    return { chips, granted, dynamic: !!(gateway && gateway.dynamic), source, notes };
  }

  const openResource = (r) => r === "**" || r === "*" || r === "*.*";

  // networkRules is the network policy in application order: the task's
  // rules (sbx applies them right after create), then the gateway's port
  // and the LLM proxy, which every sandbox gets. Without task rules the
  // sandbox runs on sbx's default "balanced" policy, which is open-ended.
  function networkRules(policies, hasGateway) {
    const net = (policies || []).filter((r) => r.type === "network");
    const rows = net.map((r) => ({
      action: r.action,
      resource: r.resource,
      cls: r.action === "deny" ? "deny" : openResource(r.resource) ? "open" : "allow",
      note: "",
    }));
    if (!net.length) rows.push({ action: "default", resource: "sbx \"balanced\" policy", cls: "open", note: "no task rules: many common hosts are allowed" });
    if (hasGateway) rows.push({ action: "allow", resource: "host.docker.internal:<gateway port>", cls: "baseline", note: "gateway" });
    rows.push({ action: "allow", resource: "llm proxy", cls: "baseline", note: "model traffic; credentials stay on the host" });
    return rows;
  }

  function fsRules(policies) {
    return (policies || []).filter((r) => r.type === "filesystem").map((r) => ({
      action: r.action,
      resource: r.resource,
      cls: r.action === "deny" ? "deny" : openResource(r.resource) ? "open" : "allow",
    }));
  }

  // grants counts what the sandbox holds beyond the baseline (harness,
  // model, gateway port, LLM proxy), and how much of it is open-ended
  // (§G.3). It is not a score: it lets two cards be compared at a glance.
  function grants(mcp, net, fs, mountRows) {
    const items = [];
    for (const m of mcp.granted) items.push({ text: m + " MCP", open: false });
    if (mcp.dynamic) items.push({ text: "mcp-add", open: true });
    for (const r of net) {
      if (r.cls === "allow" || r.cls === "open") items.push({ text: r.action === "default" ? "balanced network policy" : r.resource, open: r.cls === "open" });
    }
    for (const r of fs) if (r.cls !== "deny") items.push({ text: "fs " + r.resource, open: r.cls === "open" });
    for (const m of mountRows) if (m.mode === "rw") items.push({ text: "rw " + base(m.path), open: m.open });
    return { count: items.length, open: items.filter((i) => i.open).length, items };
  }

  // runtimeChanges are the step-up operations applied to (or refused for)
  // this sandbox: step_up_op audit events whose target is it.
  function runtimeChanges(events, sandbox) {
    return (events || [])
      .filter((e) => e.event === "step_up_op" && e.fields && e.fields.target === sandbox && e.fields.op !== "list_sandbox_policy")
      .map((e) => ({
        seq: e.seq,
        time: e.time,
        op: String(e.fields.op || "").replace(/^sandbox_/, "").replace("_", " "),
        resource: e.fields.resource || "",
        status: e.fields.status || "",
        detail: e.fields.detail || "",
      }));
  }

  // ---- ③ output ------------------------------------------------------------

  const STEPS = [
    ["queued", "queued (succeeded_at)"],
    ["rebase", "rebase onto the project branch tip"],
    ["verify", "verify"],
    ["ff", "ff-only merge"],
    ["committed", "committed"],
  ];

  // stepper is the integration stepper (§G.3 ③): each step done, active,
  // failed or todo. The commit agent does not report which of rebase,
  // verify and ff-only it is on, so all three are active while it runs.
  function stepper(t, project) {
    const live = (project && project.live) || {};
    const verify = project && project.verify;
    const steps = STEPS.map(([key, label]) => ({ key, label: key === "verify" ? "verify" + (verify ? ": " + verify : " (none set)") : label, state: "todo" }));
    const set = (keys, state) => steps.forEach((s) => keys.includes(s.key) && (s.state = state));
    const failedAt = { conflict: "rebase", verify_failed: "verify", partial: "ff" };
    let note = "";
    if (t.status === "committed") {
      set(["queued", "rebase", "verify", "ff", "committed"], "done");
    } else if (t.status === "success") {
      set(["queued"], live.integrating === t.name ? "done" : "active");
      if (live.integrating === t.name) {
        set(["rebase", "verify", "ff"], "active");
        note = "the commit agent is integrating it";
      } else {
        steps[0].label = "queued" + (t.queue_position ? " #" + t.queue_position : "") + " (succeeded_at)";
        note = "waiting for the commit agent";
      }
    } else if (t.integration) {
      const at = failedAt[t.integration.result] || "rebase";
      const order = steps.map((s) => s.key);
      set(order.slice(0, order.indexOf(at)), "done");
      set([at], "failed");
      note = "last integration failed: the task re-runs";
    } else {
      note = t.status === "running" ? "enters the queue when the task agent sets success" : "not queued";
    }
    return { steps, note };
  }

  // resultText is what the task status means for the output (§G.3 ③).
  function resultText(status) {
    switch (status) {
      case "success":
        return "success: in the commit queue, not yet landed";
      case "committed":
        return "committed: landed on the project branch";
      case "running":
        return "running → success | failed | blocked, set by the task agent";
      case "failed":
        return "failed: the task agent gave up (retried up to the budget)";
      case "blocked":
        return "blocked: needs a human (the wolf)";
      default:
        return (status || "unknown") + ": not started";
    }
  }

  // afterExit says what happens to the sandbox when its session ends
  // (removeSandboxOnExit), and what is true now.
  function afterExit(t, sandbox, session) {
    const keep = !!(t && t.capabilities && t.capabilities.save_sandbox);
    let now = "";
    if (session) now = "live session: the sandbox is in use";
    else if (sandbox) now = "retained now" + (keep ? " (save_sandbox)" : t && (t.status === "failed" || t.status === "blocked") ? " (task " + t.status + ")" : "");
    else now = "no sandbox now";
    return { rule: "removed on success; kept on failed/blocked, or with save_sandbox: true", keep, now };
  }

  // touchesWarning is touches_actual against touches: the paths the landed
  // commits changed outside the task's declared area (touches_exceeded).
  function touchesWarning(t) {
    const actual = (t.output && t.output.touches_actual) || [];
    if (!actual.length) return "";
    return "touches_exceeded: wrote outside its declared area: " + actual.join(", ");
  }

  // ---- actions ---------------------------------------------------------------

  // cardActions are the card header's buttons: watch, open in a terminal,
  // and the in-browser shell (§G.2), each disabled with its TUI command.
  function cardActions(session, sandbox) {
    const live = !!(session && session.acp && session.acp.attachable);
    return [
      { label: "Watch (ACP)", command: "a (the TUI's ACP session tabs)", refusal: live ? "" : "no live ACP session in this sandbox" },
      session && session.persistent
        ? { label: "Prompt", command: "a, then i (the TUI's ACP tab)", refusal: "" }
        : { label: "Prompt (persistent only)", command: "a, then i (the TUI's ACP tab)", refusal: "persistent sessions only: " + ((session && session.kind) || "this") + " sessions are one-shot" },
      { label: "Open in my terminal", command: ":session (from the Tasks pane)", refusal: "" },
      {
        label: "Shell in sandbox (pty scope)",
        command: "sbx exec -it " + (sandbox || "<sandbox>") + " bash",
        refusal: sandbox === "workingman-agent" || sandbox === "step-up" ? "never for workingman-agent or step-up" : "needs --api-pty and the pty scope (§A.5.3, P.6)",
      },
    ];
  }

  // inputActions are the ways to change a task's inputs (§A.4.4).
  function inputActions(t) {
    const running = t.status === "running";
    return [
      { label: "Queue intake", command: "drop a .md file into the project's intake/ directory", refusal: "", href: "/projects/" + encodeURIComponent(t.project) + "/intake" },
      { label: "Edit task…", command: "edit tasks/" + t.name + ".yaml (description, depends_on, touches, areas)", refusal: running ? "the task is running: its inputs cannot change now" : "" },
    ];
  }

  // ---- the card -------------------------------------------------------------

  // card is the sandbox card's model (§G.3). in: {task, tasks (by name),
  // project, session, sandbox (the /sandboxes row), prompt, audit (events),
  // roots}. Without a task (a planning, wolf or helper sandbox) the task
  // parts are null and the agent's static facts stand in.
  function card(inp) {
    const t = inp.task || null;
    const s = inp.session || null;
    const sb = inp.sandbox || null;
    const name = (s && s.sandbox && s.sandbox.name) || (sb && sb.name) || (t && t.sandbox) || "";
    const caps = (t && t.capabilities) || null;
    const gateway = (s && s.gateway) || (sb && sb.gateway) || null;
    const workspaces = (s && s.sandbox && s.sandbox.workspaces && s.sandbox.workspaces.length ? s.sandbox.workspaces : sb && sb.workspaces) || [];
    const mnt = mounts(workspaces, inp.roots);
    const mcp = mcpChips(gateway, caps ? caps.static_mcps : null);
    const kind = (s && s.kind) || (sb && (sb.kind || sb.role)) || (t ? "task" : "");
    const facts = AGENT_FACTS[kind] || null;
    const net = caps ? networkRules(caps.policies, !!gateway) : [];
    const fs = caps ? fsRules(caps.policies) : [];
    const g = grants(mcp, net, fs, mnt.rows);
    const live = s && s.sandbox && s.sandbox.live !== undefined ? s.sandbox.live : sb ? true : null;
    return {
      name,
      kind,
      facts,
      header: {
        live: live === true ? "live (sbx ls)" : live === false ? "gone (sbx ls)" : s ? "sbx ls unavailable" : "no sandbox",
        liveClass: live === true ? "live" : live === false ? "gone" : "unknown",
        session: s ? s.id : "",
        started: (s && s.started_at) || (sb && sb.created_at) || "",
        grants: g,
      },
      inputs: {
        prompt: inp.prompt
          ? { instructions: promptPreview(inp.prompt.instructions_md, 6), context: promptPreview(inp.prompt.context_yaml, 6), dir: inp.prompt.dir || "" }
          : null,
        task: t
          ? {
              rows: [
                ["project", (inp.project && inp.project.work_stream) || t.project],
                ["status", t.status + " · attempts " + (t.attempts || 0) + "/" + ((inp.maxAttempts || 3))],
                ["depends_on", null],
                ["touches", t.touches === null || t.touches === undefined ? "— (not declared)" : t.touches.length ? t.touches.join(", ") : "[] (writes nothing shared)"],
                ["areas", t.areas && t.areas.length ? t.areas.join(", ") : "—"],
                ["exclusive", t.exclusive ? "yes: runs alone" : "no"],
                ["source", sourceText(t)],
                ["base", (t.base || []).map((b) => b.repo + " " + String(b.hash).slice(0, 7)).join(", ") || "— (not cut yet)"],
                ["worktree", t.worktree || "—"],
                ["chosen by", chosenBy(t)],
              ],
              deps: depStates(t, inp.tasks),
              description: t.description || "",
            }
          : null,
        mounts: mnt,
      },
      caps: {
        harness: {
          image: (s && s.sandbox && s.sandbox.image) || (sb && sb.image) || (caps && caps.resolved_harness) || "",
          harness: (caps && caps.harness) || (s && s.harness) || "claude-acp",
          resolved: (caps && caps.resolved_harness) || "",
          kit: base((s && s.sandbox && s.sandbox.kit) || (sb && sb.kit) || ""),
        },
        model: caps ? { model: caps.model || "default", resolved: caps.resolved_model || "" } : { model: (s && s.model) || "default", resolved: "" },
        mcp,
        skills: "none · Plan.Skills exists, nothing sets it today",
        network: caps ? net : null,
        filesystem: caps ? fs : null,
        policyNote: caps ? "" : facts && facts.policies ? facts.policies : "set by the daemon for this agent kind; not in the API",
        runtime: runtimeChanges(inp.audit, name),
      },
      output: t
        ? {
            branch: t.task_branch || "",
            result: resultText(t.status),
            status: t.status,
            stepper: stepper(t, inp.project),
            commitSandbox: t.commit_sandbox || "",
            landsOn: (inp.project && inp.project.branch) || "",
            commits: (t.output && t.output.commits) || [],
            summary: (t.output && t.output.summary) || "",
            created: (t.output && t.output.created_files) || [],
            after: afterExit(t, sb, s),
            touches: touchesWarning(t),
            integration: t.integration || null,
          }
        : null,
      outputNote: !t && facts ? facts.output : "",
      actions: cardActions(s, name),
      inputActions: t ? inputActions(t) : [],
    };
  }

  // ---- Sandboxes list (§G.2) -------------------------------------------------

  // sandboxRows joins GET /sandboxes with GET /sessions: one row per
  // sandbox sbx reports, plus one per session whose sandbox sbx does not
  // (gone, or sbx ls failed). tasks maps "<project>/<task>" to the task
  // resource, when it was fetched, for the grant count and the card link.
  // sandboxes is null when sbx ls failed.
  function sandboxRows(sandboxes, sessions, tasks, now) {
    const rows = [];
    const bySandbox = new Map();
    for (const s of sessions || []) if (s.sandbox && s.sandbox.name) bySandbox.set(s.sandbox.name, s);
    const seen = new Set();
    const row = (sb, s) => {
      const project = (s && s.project) || (sb && sb.project) || "";
      const taskName = (s && s.task) || (sb && sb.task) || "";
      const role = (s && s.kind) || (sb && (sb.role || sb.kind)) || "";
      const t = project && taskName ? tasks && tasks.get(project + "/" + taskName) : null;
      const gateway = (s && s.gateway) || (sb && sb.gateway) || null;
      const workspaces = (s && s.sandbox && s.sandbox.workspaces && s.sandbox.workspaces.length ? s.sandbox.workspaces : sb && sb.workspaces) || [];
      const caps = t && role === "task" ? t.capabilities : null;
      const mcp = mcpChips(gateway, caps ? caps.static_mcps : null);
      const g = grants(mcp, caps ? networkRules(caps.policies, !!gateway) : [], caps ? fsRules(caps.policies) : [], mounts(workspaces, []).rows);
      const since = (s && s.started_at) || (sb && sb.created_at) || "";
      let state, cls;
      if (s && sb) {
        state = "live session · " + (s.status || "running");
        cls = "live";
      } else if (s) {
        state = sandboxes ? "session, sandbox gone" : "session (sbx ls unavailable)";
        cls = sandboxes ? "gone" : "unknown";
      } else if (sb.retained) {
        state = "retained · " + retainedReason(sb, t);
        cls = "retained";
      } else if (sb.managed) {
        state = "idle";
        cls = "idle";
      } else {
        state = "not made by workingman";
        cls = "foreign";
      }
      return {
        name: (sb && sb.name) || (s && s.sandbox && s.sandbox.name) || "",
        role,
        project,
        task: taskName,
        session: s ? s.id : "",
        state,
        cls,
        age: since ? elapsed(since, now) : "",
        since,
        grants: g,
        dynamic: mcp.dynamic,
        mcps: mcp.granted,
        image: (s && s.sandbox && s.sandbox.image) || (sb && sb.image) || "",
        managed: sb ? !!sb.managed : true,
        retained: !!(sb && sb.retained),
        attachable: !!(s && s.acp && s.acp.attachable),
        persistent: !!(s && s.persistent),
      };
    };
    for (const sb of sandboxes || []) {
      seen.add(sb.name);
      rows.push(row(sb, bySandbox.get(sb.name)));
    }
    for (const s of sessions || []) {
      const n = s.sandbox && s.sandbox.name;
      if (n && seen.has(n)) continue;
      rows.push(row(null, s));
    }
    const rank = { live: 0, gone: 1, unknown: 1, retained: 2, idle: 3, foreign: 4 };
    rows.sort((a, b) => rank[a.cls] - rank[b.cls] || (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
    return rows;
  }

  // retainedReason says why a sandbox no session uses is still there.
  function retainedReason(sb, t) {
    if (sb.role === "session") return ":session sandbox (nothing removes these)";
    if (t && t.capabilities && t.capabilities.save_sandbox) return "save_sandbox";
    const status = sb.task_status || (t && t.status) || "";
    if (status === "failed" || status === "blocked") return "task " + status;
    if (status) return "task " + status;
    return "no session";
  }

  function elapsed(since, now) {
    const s = Math.max(0, Math.round(((now || Date.now()) - Date.parse(since)) / 1000));
    if (!isFinite(s)) return "";
    if (s < 60) return s + "s";
    if (s < 3600) return Math.floor(s / 60) + "m";
    if (s < 86400) return Math.floor(s / 3600) + "h" + String(Math.floor((s % 3600) / 60)).padStart(2, "0");
    return Math.floor(s / 86400) + "d";
  }

  // ---- agents map (§G.4) -----------------------------------------------------

  // AGENT_FACTS are each agent kind's capability facts, from the code
  // (survey §7.1–§7.4). They are static and ship with the GUI; the live
  // parts (enabled, sessions, MCPs) come from GET /agents and /sessions.
  // chips: {text, cls} with cls ok (narrow), open (amber, open-ended),
  // ro, or muted.
  const AGENT_FACTS = {
    workingman: {
      title: "workingman agent",
      lane: "helper",
      purpose: "Front door: turns a request (TUI tab, WhatsApp, Signal, Slack) into project.md / intake/*.md, or edits project.yaml / tasks/*.yaml after confirming.",
      sandbox: "workingman-agent",
      mode: "persistent · interactive",
      chips: [{ text: "orch roots rw", cls: "open" }, { text: "state, audit, sessions ro", cls: "ro" }],
      gap: "Does not choose capabilities for others; its own limits on what it edits are by prompt, not by mount.",
      policies: "daemon plan: orch roots writable, state ro, --require-okta-gateway (workingmanPlan)",
      output: "edits to project.yaml / tasks/*.yaml, new project.md / intake/*.md, after confirming",
    },
    "sleepy-head": {
      title: "sleepy-head",
      lane: "helper",
      purpose: "After sleep or downtime: lists cron projects that missed ticks; with an explicit OK triggers exactly one catch-up each (writes catchup-request).",
      sandbox: "sleepy-head",
      mode: "interactive · only when behind and on AC",
      chips: [{ text: "rw: only the behind projects' dirs", cls: "ok" }],
      gap: "A scheduling helper, not a capability one: it shapes when workloads run, not what they may do.",
      policies: "daemon plan: writable mounts limited to the behind projects (sleepyHeadPlan)",
      output: "one catchup-request per approved project",
    },
    "step-up": {
      title: "step-up",
      lane: "helper",
      purpose: "Runtime escalation: a task or commit agent asks for more network or filesystem access; step-up judges, asks the operator, files a typed request; the daemon's executor applies it.",
      sandbox: "step-up",
      mode: "persistent · driven by the daemon",
      chips: [{ text: "rw: requests/ only", cls: "ok" }, { text: "post-office, static", cls: "ok" }, { text: "no orch roots", cls: "ok" }],
      gap: "The only helper that grants capability, and it cannot apply a grant itself (executor + operator approval + 10/h).",
      policies: "daemon plan: requests/ writable, static post-office gateway (stepUpPlan)",
      output: "typed change requests; the executor runs one sbx policy command and persists the rule",
    },
    project: {
      title: "project",
      lane: "workflow",
      purpose: "project.md → project.yaml",
      sandbox: "<ws>-project",
      chips: [{ text: "default MCPs", cls: "muted" }],
      policies: "runner defaults: the control dir, the host's default MCP set",
      output: "a populated project.yaml (status ready), or blocked",
    },
    planning: {
      title: "planning",
      lane: "workflow",
      purpose: "writes tasks/*.yaml",
      sandbox: "<ws>",
      highlight: "chooses per-task capabilities: static_mcps · policies · model · harness",
      chips: [{ text: "default MCPs", cls: "muted" }],
      policies: "runner defaults: the control dir and the workspace",
      output: "tasks/*.yaml, with each task's static_mcps, policies, model and harness",
    },
    task: {
      title: "task",
      lane: "workflow",
      purpose: "≤ max_parallel_tasks · own worktree + sandbox",
      sandbox: "<ws>-<task>",
      chips: [{ text: "task's static_mcps · policies · model", cls: "ok" }, { text: "dynamic gateway", cls: "open" }],
      policies: "the task's policies:",
      output: "commits on its task branch",
    },
    commit: {
      title: "commit",
      lane: "workflow",
      purpose: "queue head → rebase, verify, ff-only merge",
      sandbox: "<ws>-<task>-commit",
      chips: [{ text: "inherits task caps", cls: "muted" }],
      policies: "inherits the task's MCPs, policies and model",
      output: "the task's commits on the project branch (committed), or an integration failure",
    },
    review: {
      title: "review",
      lane: "workflow",
      purpose: "PR poll → fix / push tasks",
      sandbox: "<ws>-review",
      chips: [{ text: "hard-coded: github + 2 hosts", cls: "ok" }],
      policies: "hard-coded: static github gateway, allow api.github.com and api.githubcopilot.com",
      output: "pr-comment / pr-check / pr-push tasks, pull_requests updates",
    },
    archive: {
      title: "archive",
      lane: "workflow",
      purpose: "cleanup: true → ≤ 1 commit + push",
      sandbox: "<ws>-archive",
      chips: [{ text: "hard-coded: push host only", cls: "ok" }],
      policies: "hard-coded: a network allow for the push host",
      output: "at most one commit and push per repo, then archive: true",
    },
    wolf: {
      title: "wolf (interactive)",
      lane: "wolf",
      purpose: "any block → diagnose, edit project/task files, re-run tasks",
      sandbox: "<ws>-wolf",
      chips: [{ text: "dynamic MCPs", cls: "open" }, { text: "persistent", cls: "muted" }],
      policies: "runner defaults, dynamic gateway",
      output: "edits to this project's files, blocked-session.yaml",
    },
    session: {
      title: ":session",
      lane: "none",
      purpose: "the sandbox's own interactive claude in a terminal tab (not ACP)",
      sandbox: "session-<ws>[-<task>]",
      chips: [],
      policies: "the :session launch's defaults",
      output: "whatever the human does; nothing removes the sandbox",
    },
  };

  const WORKFLOW = ["project", "planning", "task", "commit", "review", "archive", "wolf"];

  // EDGES are the helpers' file-write interface to the workflow (§G.4):
  // the same file contract the GUI uses.
  const EDGES = [
    { from: "workingman", via: "project.md", to: "project" },
    { from: "workingman", via: "intake/*.md", to: "planning" },
    { from: "sleepy-head", via: "catchup-request → cron replan", to: "planning" },
    { from: "step-up", via: "sbx policy + persisted policies:", to: "task" },
    { from: "step-up", via: "sbx policy + persisted policies:", to: "commit" },
  ];

  // FLOWS are the workflow's own arrows.
  const FLOWS = [
    { from: "project", to: "planning", via: "project.yaml" },
    { from: "planning", to: "task", via: "tasks/*.yaml" },
    { from: "task", to: "commit", via: "success → commit queue" },
    { from: "commit", to: "branch", via: "ff-only" },
    { from: "branch", to: "review", via: "PR" },
    { from: "review", to: "task", via: "pr-comment / pr-check / pr-push tasks" },
    { from: "branch", to: "archive", via: "cleanup: true" },
    { from: "any", to: "wolf", via: "blocked (retries exhausted, partial integration, :wolf)" },
  ];

  // concreteSandbox fills a sandbox pattern in for one work stream.
  function concreteSandbox(pattern, ws) {
    return ws ? pattern.replace("<ws>", ws) : pattern;
  }

  // agentsMap is the agents screen's model. agents is GET /agents, sessions
  // GET /sessions, project the selected project summary (or null for every
  // project), audit the step_up_op events.
  function agentsMap(agents, sessions, project, audit) {
    const byKind = new Map();
    for (const s of sessions || []) {
      if (project && s.project !== project.id) continue;
      if (!byKind.has(s.kind)) byKind.set(s.kind, []);
      byKind.get(s.kind).push(s);
    }
    const ws = project ? project.work_stream : "";
    const helpers = ["workingman", "sleepy-head", "step-up"].map((kind) => {
      const a = (agents || []).find((x) => x.kind === kind) || { kind, enabled: false, mcps: [] };
      const f = AGENT_FACTS[kind];
      const live = (sessions || []).filter((s) => s.kind === kind);
      const chips = f.chips.slice();
      if (a.mcps && a.mcps.length) chips.push({ text: "MCPs: " + a.mcps.join(", ") + (a.mcp_dynamic ? "" : ", static"), cls: a.mcp_dynamic ? "open" : "ok" });
      if (a.mcp_dynamic) chips.push({ text: "dynamic MCPs", cls: "open" });
      let state = a.enabled ? (a.session || live.length ? "running" : "enabled, not running") : "off";
      const extra = [];
      if (kind === "sleepy-head" && a.pending_catchups && a.pending_catchups.length) extra.push("behind: " + a.pending_catchups.join(", "));
      if (kind === "step-up") {
        const ops = (audit || []).filter((e) => e.event === "step_up_op");
        if (ops.length) extra.push(`${ops.length} step-up op${ops.length === 1 ? "" : "s"} in the audit ring (${ops.filter((e) => e.fields && e.fields.status === "applied").length} applied)`);
      }
      return {
        kind,
        title: f.title,
        purpose: f.purpose,
        sandbox: a.sandbox || f.sandbox,
        mode: f.mode,
        enabled: !!a.enabled,
        state,
        session: a.session || (live[0] && live[0].id) || "",
        chips,
        gap: f.gap,
        extra,
      };
    });
    const workflow = WORKFLOW.map((kind) => {
      const f = AGENT_FACTS[kind];
      const live = byKind.get(kind) || [];
      return {
        kind,
        title: f.title,
        purpose: f.purpose,
        sandbox: concreteSandbox(f.sandbox, ws),
        chips: f.chips,
        highlight: f.highlight || "",
        live: live.map((s) => ({ id: s.id, task: s.task || "", sandbox: (s.sandbox && s.sandbox.name) || "", project: s.project || "" })),
      };
    });
    return { helpers, workflow, edges: EDGES, flows: FLOWS, branch: project ? project.branch || "" : "" };
  }

  return {
    KNOWN_MCPS,
    AGENT_FACTS,
    mounts,
    depStates,
    chosenBy,
    sourceText,
    promptPreview,
    mcpChips,
    networkRules,
    fsRules,
    grants,
    runtimeChanges,
    stepper,
    resultText,
    afterExit,
    touchesWarning,
    cardActions,
    inputActions,
    card,
    sandboxRows,
    retainedReason,
    agentsMap,
    elapsed,
  };
});
