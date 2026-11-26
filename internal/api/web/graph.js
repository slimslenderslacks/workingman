// workingman GUI: the project view's model (docs/proposals/workingman-gui.md
// §G.1). Pure functions over the API's resources, with no DOM: the task
// graph's layout and node labels, the commit-queue lane, the project's
// action buttons, the transcript preview lines and the patching of state from
// stream events. projects.js draws what these return.
//
// The file runs in the browser, where it becomes wm.pv, and under node for
// its tests (internal/api/testdata/graph_test.js), where it is a CommonJS
// module. It must stay free of anything either side lacks.
"use strict";

(function (factory) {
  const pv = factory();
  if (typeof module === "object" && module.exports) module.exports = pv;
  else window.wm.pv = pv;
})(function () {
  // Node geometry for the graph, in SVG user units.
  const NODE_W = 180;
  const NODE_H = 58;
  const COL_GAP = 50;
  const ROW_GAP = 18;
  const PAD = 16;
  const HEAD = 24; // room for the "depth n" labels

  // ---- task graph ----------------------------------------------------------

  // depths gives every node its dependency depth: 0 without dependencies,
  // else one more than its deepest dependency. Edges from a name that is not
  // a node (a dangling depends_on, which the graph reports in errors) are
  // ignored; a cycle (also an error) is cut where it is found.
  function depths(nodes, edges) {
    const names = new Set(nodes.map((n) => n.name));
    const deps = new Map(nodes.map((n) => [n.name, []]));
    for (const e of edges) {
      if (names.has(e.from) && names.has(e.to)) deps.get(e.to).push(e.from);
    }
    const depth = new Map();
    const visiting = new Set();
    const visit = (name) => {
      if (depth.has(name)) return depth.get(name);
      if (visiting.has(name)) return 0;
      visiting.add(name);
      let d = 0;
      for (const dep of deps.get(name)) d = Math.max(d, visit(dep) + 1);
      visiting.delete(name);
      depth.set(name, d);
      return d;
    };
    for (const n of nodes) visit(n.name);
    return depth;
  }

  // layout places the graph's nodes left to right by depth. Within a column
  // nodes are ordered by the mean row of their dependencies (so edges run
  // roughly straight), then by name. It returns {nodes: [{name, depth, row,
  // x, y}], edges: [{from, to, path}], columns, width, height}.
  function layout(graph) {
    const nodes = (graph && graph.nodes) || [];
    const edges = (graph && graph.edges) || [];
    const depth = depths(nodes, edges);
    const cols = [];
    for (const n of nodes) {
      const d = depth.get(n.name);
      (cols[d] = cols[d] || []).push(n.name);
    }
    const row = new Map();
    const depsOf = new Map(nodes.map((n) => [n.name, []]));
    for (const e of edges) if (depsOf.has(e.to) && depth.has(e.from)) depsOf.get(e.to).push(e.from);
    cols.forEach((col, d) => {
      const key = (name) => {
        const rows = depsOf.get(name).map((p) => row.get(p)).filter((r) => r !== undefined);
        return rows.length ? rows.reduce((a, b) => a + b, 0) / rows.length : -1;
      };
      if (d > 0) col.sort((a, b) => key(a) - key(b) || (a < b ? -1 : a > b ? 1 : 0));
      else col.sort();
      col.forEach((name, i) => row.set(name, i));
    });
    const placed = new Map();
    const out = [];
    for (const n of nodes) {
      const d = depth.get(n.name);
      const r = row.get(n.name);
      const p = { name: n.name, depth: d, row: r, x: PAD + d * (NODE_W + COL_GAP), y: HEAD + PAD + r * (NODE_H + ROW_GAP) };
      placed.set(n.name, p);
      out.push(p);
    }
    const lines = [];
    for (const e of edges) {
      const a = placed.get(e.from);
      const b = placed.get(e.to);
      if (!a || !b) continue;
      const x1 = a.x + NODE_W, y1 = a.y + NODE_H / 2;
      const x2 = b.x - 2, y2 = b.y + NODE_H / 2;
      const mx = (x1 + x2) / 2;
      lines.push({ from: e.from, to: e.to, path: `M${x1},${y1} C${mx},${y1} ${mx},${y2} ${x2},${y2}` });
    }
    const rows = Math.max(1, ...cols.map((c) => c.length));
    return {
      nodes: out,
      edges: lines,
      columns: cols.length,
      width: PAD * 2 + Math.max(1, cols.length) * NODE_W + Math.max(0, cols.length - 1) * COL_GAP,
      height: HEAD + PAD * 2 + rows * NODE_H + (rows - 1) * ROW_GAP,
      nodeW: NODE_W,
      nodeH: NODE_H,
    };
  }

  // failedDep reports whether a dependency's status leaves its dependents
  // stuck: their edge is drawn dashed red.
  const failedDep = (status) => status === "failed" || status === "blocked";

  const short = (hash) => String(hash || "").slice(0, 7);

  // attemptText is the TUI's "attempt n/m" (tui.go integrationHint): the
  // run the task is on, out of the runs its budget allows.
  function attemptText(t, maxAttempts) {
    const max = maxAttempts || 3;
    return `attempt ${(t.attempts || 0) + 1}/${max + 1}`;
  }

  // integrationText describes a task's last failed integration, like the
  // TUI's integrationHint.
  function integrationText(in_) {
    switch (in_.result) {
      case "conflict":
        return "conflict" + (in_.files && in_.files.length ? " in " + in_.files.join(", ") : in_.repo ? " in " + in_.repo : "");
      case "verify_failed":
        return "build check failed" + (in_.repo ? " in " + in_.repo : "");
      case "partial":
        return "partially integrated";
      default:
        return "integration error" + (in_.repo ? " in " + in_.repo : "");
    }
  }

  function elapsed(since, now) {
    const s = Math.max(0, Math.round((now - Date.parse(since)) / 1000));
    if (!isFinite(s)) return "";
    if (s < 60) return s + "s";
    if (s < 3600) return Math.floor(s / 60) + "m";
    return Math.floor(s / 3600) + "h" + String(Math.floor((s % 3600) / 60)).padStart(2, "0");
  }

  // nodeFacts is what a graph node says under its name: a glyph and the
  // status with its most useful derived fact (line), a dim detail line, and
  // the classes that style it. ctx is {graph, project, sessions (by id),
  // tasks (by name), now}. success is drawn as "queued for integration",
  // never as done (§G.1).
  function nodeFacts(name, ctx) {
    const t = ctx.tasks.get(name) || { name, status: (ctx.graph.nodes.find((n) => n.name === name) || {}).status };
    const live = (ctx.project && ctx.project.live) || {};
    const integrating = live.integrating || "";
    const deferred = (ctx.graph && ctx.graph.deferred) || {};
    const max = ctx.graph && ctx.graph.max_attempts;
    const cls = ["node", "st-" + (t.status || "unknown")];
    let line = "", detail = "", title = "";
    const reran = t.integration && t.status !== "committed";
    switch (t.status) {
      case "committed": {
        const c = (t.output && t.output.commits) || [];
        line = "✓ committed" + (c.length ? " · " + short(c[0].hash) : "");
        detail = c.length > 1 ? `${c.length} commits` : hints(t);
        break;
      }
      case "success":
        if (name === integrating) {
          line = "⧗ success · integrating";
          detail = "commit agent: rebase → verify → ff-only";
          cls.push("integrating");
        } else {
          line = "⧗ success · " + (t.queue_position ? "queue #" + t.queue_position : "queued");
          detail = t.succeeded_at ? "succeeded_at " + clock(t.succeeded_at) : "queued for integration";
        }
        break;
      case "running": {
        const s = t.session && ctx.sessions.get(t.session);
        line = "● running" + (s && s.started_at ? " · " + elapsed(s.started_at, ctx.now) : "") + (s && s.prompt_count ? " · turn " + s.prompt_count : "");
        detail = t.sandbox ? "sandbox " + t.sandbox : "";
        break;
      }
      case "failed":
        line = "✗ failed · " + attemptText(t, max);
        detail = t.capabilities && t.capabilities.save_sandbox ? "sandbox retained" : t.failure_reason || "";
        title = t.failure_reason || "";
        break;
      case "blocked":
        line = "■ blocked";
        detail = t.blocked_reason || (t.capabilities && t.capabilities.save_sandbox ? "sandbox retained" : "");
        title = t.blocked_reason || "";
        break;
      case "ready": {
        const waiting = t.waiting_on || [];
        const pending = depsNotCommitted(t, ctx.tasks);
        if (waiting.length) {
          line = "○ ready · waiting on landing";
          detail = waiting.map((w) => (w === integrating ? w + " (integrating)" : w)).join(", ");
        } else if (pending.length) {
          line = "○ ready · waiting_on";
          detail = pending.map((p) => p.name + (failedDep(p.status) ? " (" + p.status + ")" : "")).join(", ");
        } else if ((ctx.graph.runnable || []).includes(name)) {
          line = "○ ready · runnable";
          detail = hints(t);
        } else {
          line = "○ ready";
          detail = hints(t);
        }
        break;
      }
      default:
        line = t.status || "unknown";
    }
    if (reran) {
      cls.push("rerun");
      line += " · re-run";
      detail = integrationText(t.integration) + " (" + attemptText(t, max) + ")";
    }
    if ((deferred.overlap || []).includes(name)) {
      cls.push("deferred");
      title = "deferred: overlaps a task in flight" + (t.areas && t.areas.length ? " on " + t.areas.join(", ") : "");
    } else if (t.status === "ready" && (ctx.graph.runnable || []).includes(name) && (deferred.cap || deferred.power)) {
      cls.push("deferred");
      title = deferred.cap ? "deferred: waiting for a global task slot" : "deferred: waiting for AC power";
    }
    if (t.exclusive) title = (title ? title + " · " : "") + "exclusive";
    return { line, detail, title, classes: cls };
  }

  function hints(t) {
    if (t.areas && t.areas.length) return "areas: " + t.areas.join(", ");
    if (t.touches && t.touches.length) return "touches: " + t.touches.join(", ");
    return "";
  }

  function depsNotCommitted(t, tasks) {
    return (t.depends_on || []).map((d) => tasks.get(d) || { name: d, status: "missing" }).filter((d) => d.status !== "committed");
  }

  function clock(ts) {
    const d = new Date(ts);
    if (isNaN(d)) return "";
    return String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0");
  }

  // ---- commit-queue lane ---------------------------------------------------

  // lane is the commit queue and the landed commits (§G.1, right): queued
  // tasks head first (taskgraph.Queued order), which one the commit agent is
  // integrating, the commits that landed newest first, the base they landed
  // on, and the tasks whose last integration failed.
  function lane(project, tasks, graph) {
    const queueNames = (graph && graph.commit_queue) || (project && project.commit_queue) || [];
    const live = (project && project.live) || {};
    const queued = queueNames.map((name, i) => {
      const t = tasks.get(name) || { name };
      return {
        name,
        position: i + 1,
        integrating: live.integrating === name,
        waiting: (t.waiting_on || []).length ? "waiting on " + t.waiting_on.join(", ") : "",
        succeeded_at: t.succeeded_at || null,
      };
    });
    const committed = [...tasks.values()].filter((t) => t.status === "committed");
    const at = (t) => (t.completed_at ? Date.parse(t.completed_at) : 0);
    committed.sort((a, b) => at(b) - at(a) || (a.name < b.name ? -1 : 1));
    const landed = [];
    for (const t of committed) {
      const commits = (t.output && t.output.commits) || [];
      if (!commits.length) landed.push({ task: t.name, hash: "", repo: "", completed_at: t.completed_at || null });
      for (const c of commits) landed.push({ task: t.name, hash: c.hash, repo: c.repo, completed_at: t.completed_at || null });
    }
    // The base: what the first task to land was cut from.
    let base = [];
    const first = committed.length ? committed[committed.length - 1] : null;
    if (first && first.base && first.base.length) base = first.base;
    else {
      for (const t of tasks.values()) if (t.base && t.base.length) { base = t.base; break; }
    }
    const failures = [...tasks.values()]
      .filter((t) => t.integration && t.status !== "committed")
      .map((t) => ({
        task: t.name,
        status: t.status,
        result: t.integration.result,
        text: integrationText(t.integration),
        files: t.integration.files || [],
        output: tail(t.integration.output || "", 8),
        // Where the failure sent the task: a conflict or failed check sends
        // it back to ready; a partial publish blocks the project.
        outcome: t.integration.result === "partial" ? "project blocked" : t.status === "ready" ? "task back to ready" : "task " + t.status,
      }))
      .sort((a, b) => (a.task < b.task ? -1 : 1));
    return {
      branch: (project && project.branch) || "",
      queued,
      integrating: live.integrating || "",
      reserved: !!live.commit_reserved,
      landed,
      base,
      failures,
    };
  }

  function tail(text, n) {
    const lines = String(text).replace(/\s+$/, "").split("\n");
    return lines.slice(-n).join("\n");
  }

  // ---- actions ---------------------------------------------------------------

  // projectActions lists the header's buttons (§G.1, §A.4.7). The GUI is
  // read-only, so every one is disabled; each carries the TUI command that
  // does it today and, when that command would refuse right now, the TUI's
  // own refusal text (internal/control).
  function projectActions(p) {
    const name = p.work_stream;
    const status = p.status || "";
    const out = [];
    if (status === "stopped") {
      out.push({ label: "Start", command: ":start", refusal: "" });
    } else {
      out.push({ label: "Stop", command: ":stop", refusal: status === "" ? `${name} has no status yet to stop` : "" });
    }
    out.push({ label: (p.live && p.live.wolf_in_flight) ? "Wolf (in flight)" : "Wolf…", command: ":wolf", refusal: "", danger: true });
    const watching = p.flags && p.flags.watching_pr;
    out.push({
      label: watching ? "Poll PR now" : "Review",
      command: ":review",
      refusal: status === "idle" ? "" : `review starts on an idle work stream or kicks one already watching a PR (this one is "${status}")`,
    });
    out.push({ label: "Queue intake", command: "drop a .md file into the project's intake/ directory", refusal: "" });
    out.push({ label: "Open shell ▾", command: ":orch or :workspace", refusal: "" });
    return out;
  }

  // taskActions lists the selected-task strip's buttons (§G.1, §G.2).
  // session is the task's live session resource, if any.
  function taskActions(t, session) {
    const live = !!(session && session.acp && session.acp.attachable);
    const out = [
      { label: "Watch (ACP)", command: "a (the TUI's ACP session tabs)", refusal: live ? "" : "no live ACP session for this task" },
      { label: "Open in my terminal", command: ":session (from the Tasks pane)", refusal: "" },
    ];
    if (session && session.persistent) {
      out.push({ label: "Prompt", command: "a, then i (the TUI's ACP tab)", refusal: "" });
    } else {
      out.push({ label: "Prompt (persistent only)", command: "a, then i (the TUI's ACP tab)", refusal: "persistent sessions only: task sessions are one-shot" });
    }
    if (t && (t.status === "failed" || t.status === "blocked")) {
      out.push({ label: "Retry", command: ":wolf, then ask it to re-run the task", refusal: "" });
    }
    return out;
  }

  // policySummary is a task's network policy as the sandbox sees it: the
  // deny-all baseline and the task's grants.
  function policySummary(policies) {
    const net = (policies || []).filter((r) => r.type === "network");
    const allow = net.filter((r) => r.action === "allow").map((r) => r.resource);
    const deny = net.filter((r) => r.action === "deny").map((r) => r.resource);
    let s = "deny **";
    if (allow.length) s += " · allow " + allow.join(", ");
    if (deny.length) s += " · deny " + deny.join(", ");
    return s;
  }

  // ---- transcript preview --------------------------------------------------

  // transcriptLines turns stream.log frames (ACP JSON-RPC, one per entry)
  // into the preview's lines: a "▸ title" per tool call, the agent's text,
  // and the plan. Consecutive message chunks are joined before splitting.
  // Returns the last max lines, each at most width characters.
  function transcriptLines(frames, max, width) {
    max = max || 12;
    width = width || 110;
    const out = [];
    let text = "";
    const flush = () => {
      if (!text) return;
      for (const l of text.split("\n")) if (l.trim()) out.push({ kind: "text", text: "  " + l.trim() });
      text = "";
    };
    for (const f of frames) {
      const u = f && f.method === "session/update" && f.params && f.params.update;
      if (!u) continue;
      switch (u.sessionUpdate) {
        case "agent_message_chunk":
          if (u.content && u.content.text) text += u.content.text;
          break;
        case "tool_call":
          flush();
          out.push({ kind: "tool", text: "▸ " + (u.title || u.kind || "tool call") });
          break;
        case "tool_call_update":
          if (u.status === "failed") {
            flush();
            out.push({ kind: "error", text: "  ✗ " + (u.title || "tool call failed") });
          }
          break;
        case "plan":
          flush();
          out.push({ kind: "plan", text: "  plan: " + (u.entries || []).map((e, i) => `${i + 1} ${e.content}`).join("  ") });
          break;
      }
    }
    flush();
    return out.slice(-max).map((l) => (l.text.length > width ? { kind: l.kind, text: l.text.slice(0, width - 1) + "…" } : l));
  }

  // ---- live state ----------------------------------------------------------

  // newState is the project view's state: the project (detail), its tasks
  // and sessions by key, the graph, and the cursor the lists were read at.
  function newState(project, tasks, graph, sessions, cursor) {
    return {
      project,
      tasks: new Map((tasks || []).map((t) => [t.name, t])),
      graph,
      sessions: new Map((sessions || []).map((s) => [s.id, s])),
      cursor: cursor || 0,
      removed: false,
    };
  }

  // apply patches state with one stream event (§A.3.3) and reports what it
  // touched: {graph: refetch the graph, sessions: the session map changed,
  // project: the header or rail changed}. Events for other projects, and
  // events at or before the cursor the lists were read at (the stream's
  // replay of what the lists already hold), change nothing.
  function apply(state, ev) {
    const none = { graph: false, sessions: false, project: false, changed: false };
    if (!ev || ev.type === "hello" || ev.type === "heartbeat") return none;
    if (ev.type === "reset") return { graph: true, sessions: true, project: true, changed: true, reset: true };
    const id = state.project && state.project.id;
    if (ev.project !== id) return none;
    if (ev.seq && ev.seq <= state.cursor) return none;
    const d = ev.data || {};
    const r = { graph: false, sessions: false, project: false, changed: true };
    switch (ev.type) {
      case "project.updated":
        Object.assign(state.project, d.project || {});
        r.project = true;
        r.graph = true;
        break;
      case "project.status":
        state.project.status = d.to;
        state.project.blocked_reason = d.blocked_reason;
        state.project.stopped_from = d.stopped_from;
        r.project = true;
        break;
      case "project.live":
        state.project.live = d.live;
        r.project = true;
        r.graph = true;
        break;
      case "project.removed":
        state.removed = true;
        r.project = true;
        break;
      case "task.created":
        state.tasks.set(d.name, d);
        r.graph = true;
        break;
      case "task.updated":
        state.tasks.set(d.name, d.task);
        r.graph = true;
        break;
      case "task.status": {
        const t = state.tasks.get(d.name);
        if (t) Object.assign(t, { status: d.to, attempts: d.attempts, failure_reason: d.failure_reason, blocked_reason: d.blocked_reason });
        r.graph = true;
        break;
      }
      case "task.removed":
        state.tasks.delete(d.name);
        r.graph = true;
        break;
      case "queue.updated":
        state.project.commit_queue = d.commit_queue || [];
        state.project.live = Object.assign({}, state.project.live, { integrating: d.integrating || "", commit_reserved: !!d.commit_reserved });
        if (state.graph) state.graph.commit_queue = d.commit_queue || [];
        r.project = true;
        r.graph = true;
        break;
      case "session.started":
        state.sessions.set(d.id, d);
        r.sessions = true;
        break;
      case "session.updated":
        state.sessions.set(d.id, d.session);
        r.sessions = true;
        break;
      case "session.ended":
        state.sessions.delete(d.id);
        r.sessions = true;
        break;
      default:
        return none;
    }
    if (ev.seq) state.cursor = ev.seq;
    return r;
  }

  // ---- work-stream rail ----------------------------------------------------

  // The order of the rail's progress bar segments.
  const BAR = ["committed", "success", "running", "ready", "failed", "blocked"];

  // railFacts is a rail card's summary line, bar segments and badges: the
  // TUI's card border colours as words (§G.1, survey §12.1).
  function railFacts(p) {
    const c = p.task_counts || {};
    const total = p.task_total || 0;
    const parts = [total + (total === 1 ? " task" : " tasks")];
    if (c.success) parts.push(c.success + " queued");
    if (c.running) parts.push(c.running + " running");
    if (c.failed) parts.push(c.failed + " failed");
    const badges = [];
    if (p.load_error) badges.push({ kind: "bad", text: "load error", title: p.load_error });
    if (p.cron_active) badges.push({ kind: "cron", text: "cron", title: p.cron });
    if (p.flags && p.flags.watching_pr) {
      const pr = (p.pull_requests || []).find((x) => x.state !== "merged" && x.state !== "closed") || (p.pull_requests || [])[0];
      badges.push({ kind: "pr", text: pr ? "PR #" + pr.number : "watching PR", title: pr ? pr.repo + "#" + pr.number : "" });
    }
    if (p.flags && p.flags.archive) badges.push({ kind: "archive", text: "archive-ready", title: "cleaned up: :archive is next" });
    if (p.flags && p.flags.hidden) badges.push({ kind: "muted", text: "hidden", title: "" });
    const live = p.live || {};
    let note = "";
    if (p.status === "blocked" && live.wolf_in_flight) note = "wolf in flight · join ▸";
    else if (live.integrating) note = "integrating " + live.integrating;
    return {
      summary: parts.join(" · "),
      bar: total ? BAR.filter((s) => c[s]).map((s) => ({ status: s, fraction: c[s] / total })) : [],
      badges,
      note,
    };
  }

  return {
    depths,
    layout,
    failedDep,
    nodeFacts,
    attemptText,
    integrationText,
    lane,
    projectActions,
    taskActions,
    policySummary,
    transcriptLines,
    newState,
    apply,
    railFacts,
    elapsed,
  };
});
