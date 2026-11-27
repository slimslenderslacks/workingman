// workingman GUI: the model of the timeline, pull-request and intake views
// (docs/proposals/workingman-gui.md §G.5). Pure functions over the API's
// resources, with no DOM: the timeline's rows (event family, highlight,
// summary, the project / task / sandbox / session each row names) and
// filters, a project's PR cards (state, poll ladder and next poll, fix
// tasks, review budgets), and the intake and new-project forms' hints and
// name checks. timeline.js, pulls.js and intake.js draw what these return.
//
// Like graph.js, the file runs in the browser, where it becomes wm.sv, and
// under node for its tests (internal/api/testdata/secondary_test.js).
"use strict";

(function (factory) {
  const sv = factory();
  if (typeof module === "object" && module.exports) module.exports = sv;
  else window.wm.sv = sv;
})(function () {
  const enc = encodeURIComponent;

  // ---- timeline (audit) ----------------------------------------------------

  // HIGHLIGHT are the audit events §A.3.3 asks the GUI to draw orange.
  const HIGHLIGHT = new Set([
    "project_blocked", "task_retry_exhausted", "integration_conflicted", "integration_verify_failed",
    "touches_exceeded", "archive_incomplete", "review_error", "step_up_op", "session_reaped",
  ]);

  // FAMILIES are the event families of survey §13, in filter order. Each is
  // matched by name prefixes (and a few exact names), first match wins.
  const FAMILIES = [
    { key: "api", label: "API", prefixes: ["api_"] },
    { key: "integration", label: "worktrees / integration", prefixes: ["integration_", "worktree_", "touches_"] },
    { key: "task", label: "tasks", prefixes: ["task_", "taskgraph_", "no_ready_tasks", "sandbox_retained", "outcome_"] },
    { key: "session", label: "sessions", prefixes: ["session_", "acp_session_", "wolf_", "archive_", "cleanup_", "interactive_", "workingman_agent_", "step_up_", "sleepy_head_"] },
    { key: "review", label: "review / PRs", prefixes: ["review_"] },
    { key: "intake", label: "intake / planning", prefixes: ["intake_", "planning_", "repo_sync_", "dispatch_deferred_planning", "replan_"] },
    { key: "cron", label: "cron", prefixes: ["cron_", "catchup_", "clock_jump", "power_", "cap_resumed"] },
    { key: "project", label: "projects", prefixes: ["project_", "seed_", "blocked_session_", "planned_snapshot_"] },
    { key: "messaging", label: "channels / post-office", prefixes: ["channel_", "channels_", "router_", "postoffice_", "notify_"] },
    { key: "process", label: "daemon", prefixes: ["daemon_", "startup_", "watch", "git_preflight", "state_snapshot_", "ssh_auth_sock_", "scheduler_"] },
  ];

  function family(name) {
    name = String(name || "");
    for (const f of FAMILIES) if (f.prefixes.some((p) => name.startsWith(p))) return f.key;
    // <name>_gateway_* and <name>_mcp_* are the gateway families (survey §13).
    if (/_gateway_|_mcp_/.test(name)) return "session";
    return "other";
  }

  // PATH_FIELDS and LONG_FIELDS are left out of a row's one-line summary:
  // the paths become links, the long fields the detail line.
  const PATH_FIELDS = new Set(["path", "key", "project", "project_path", "task_path", "workspace", "workspaces"]);
  const LONG_FIELDS = ["failure_reason", "blocked_reason", "reason", "err", "detail", "output", "warning"];

  // taskOf is the task an event names: its task field, else a
  // tasks/<name>.yaml path.
  function taskOf(f) {
    if (f.task) return f.task;
    for (const k of ["path", "task_path"]) {
      const m = /\/tasks\/([^/]+)\.yaml$/.exec(f[k] || "");
      if (m) return m[1];
    }
    return "";
  }

  // row turns an audit event ({seq, time, event, project, fields}) into a
  // timeline row: {seq, time, event, family, highlight, summary, detail,
  // links: [{kind, label, href}]}. Each link is a project, task, sandbox or
  // session the event names (§G.5: "click a row → jumps to the project /
  // task / sandbox it names").
  function row(ev) {
    const f = ev.fields || {};
    const project = ev.project || "";
    const task = taskOf(f);
    const links = [];
    if (project) links.push({ kind: "project", label: project.replace(/^p_\d+\./, ""), href: "/projects/" + enc(project) });
    if (project && task) links.push({ kind: "task", label: task, href: "/projects/" + enc(project) + "?task=" + enc(task) });
    const sandbox = f.sandbox || (ev.event === "step_up_op" ? f.target : "");
    if (sandbox) links.push({ kind: "sandbox", label: sandbox, href: "/sandboxes/" + enc(sandbox) });
    if (f.session_id) links.push({ kind: "session", label: f.session_id, href: "/sessions/" + enc(f.session_id) });
    const parts = [];
    for (const [k, v] of Object.entries(f)) {
      if (PATH_FIELDS.has(k) || LONG_FIELDS.includes(k) || v === "") continue;
      if (k === "task" && task) continue;
      parts.push(k + "=" + clip(v, 60));
    }
    if (task) parts.unshift(task);
    const detail = LONG_FIELDS.filter((k) => f[k]).map((k) => k + ": " + clip(f[k], 300)).join(" · ");
    return {
      seq: ev.seq,
      time: ev.time,
      event: ev.event,
      family: family(ev.event),
      highlight: HIGHLIGHT.has(ev.event),
      summary: parts.join(" "),
      detail,
      links,
    };
  }

  // matches applies the timeline's filters: {project, family, highlighted,
  // q (a substring of the event, summary or detail)}.
  function matches(r, ev, flt) {
    if (!flt) return true;
    if (flt.project && ev.project !== flt.project) return false;
    if (flt.family && r.family !== flt.family) return false;
    if (flt.highlighted && !r.highlight) return false;
    const q = (flt.q || "").trim().toLowerCase();
    if (q && ![r.event, r.summary, r.detail].some((s) => String(s).toLowerCase().includes(q))) return false;
    return true;
  }

  // Timeline is the timeline's list: audit events by their (audit) seq,
  // newest first when drawn, bounded to max. add ignores an event it
  // already holds, so the list from GET /audit and the stream's audit
  // events can overlap.
  class Timeline {
    constructor(max) {
      this.max = max || 2000;
      this.events = []; // oldest first
      this.last = 0;
    }
    load(items) {
      this.events = (items || []).slice().sort((a, b) => a.seq - b.seq);
      this.last = this.events.length ? this.events[this.events.length - 1].seq : 0;
      this.trim();
    }
    add(ev) {
      if (!ev || !ev.seq || ev.seq <= this.last) return false;
      this.events.push(ev);
      this.last = ev.seq;
      this.trim();
      return true;
    }
    trim() {
      if (this.events.length > this.max) this.events.splice(0, this.events.length - this.max);
    }
    rows(flt) {
      const out = [];
      for (let i = this.events.length - 1; i >= 0; i--) {
        const ev = this.events[i];
        const r = row(ev);
        if (matches(r, ev, flt)) out.push(r);
      }
      return out;
    }
    // range is the seqs held, for "seq a–b"; first > 1 means older lines
    // have left the daemon's ring.
    range() {
      if (!this.events.length) return null;
      return { first: this.events[0].seq, last: this.last };
    }
  }

  // ---- pull requests -------------------------------------------------------

  // rung is a ladder spec's short label: "@every 8m" → "8m".
  const rung = (spec) => String(spec || "").replace(/^@every\s+/, "");

  // until says how far t is from now: "in 3m", "due".
  function until(t, now) {
    const ms = Date.parse(t) - now;
    if (!isFinite(ms)) return "";
    if (ms <= 0) return "due";
    const s = Math.round(ms / 1000);
    if (s < 60) return "in " + s + "s";
    if (s < 3600) return "in " + Math.round(s / 60) + "m";
    return "in " + Math.floor(s / 3600) + "h" + String(Math.round((s % 3600) / 60)).padStart(2, "0");
  }

  // FIX_KINDS are the task sources a review creates (task.Source kinds).
  const FIX_KINDS = { "pr-comment": "review comment", "pr-check": "failed check", "pr-push": "push" };

  // fixTask is one fix task's line: its source kind and where it is.
  function fixTask(name, t) {
    if (!t) return { name, kind: "", status: "missing", text: "task file gone" };
    const kind = (t.source && t.source.kind) || "";
    let text = t.status;
    if (t.status === "committed") text = "committed ✓";
    else if (t.queue_position) text = "in commit queue #" + t.queue_position;
    else if (t.status === "success") text = "queued for integration";
    else if (t.status === "failed" || t.status === "blocked") text = t.status + (t.failure_reason || t.blocked_reason ? ": " + clip(t.failure_reason || t.blocked_reason, 80) : "");
    return { name, kind, kindText: FIX_KINDS[kind] || kind, status: t.status, text };
  }

  // safeURL keeps an https link only: pull_requests[].url is written by an
  // agent, and a javascript: URL must never become a link.
  const safeURL = (u) => (/^https:\/\/[^\s"'<>]+$/i.test(String(u || "")) ? String(u) : "");

  // pullCard is one PR's card (§G.5). pr is a GET /projects/{id}/pulls item;
  // tasks maps task name → task; now is the clock.
  function pullCard(pr, tasks, now) {
    const rp = pr.review_poll;
    const ladder = (pr.poll_ladder || []).map((spec, i) => ({ label: rung(spec), current: !!rp && rp.backoff_index === i }));
    const facts = [];
    if (pr.head_sha) facts.push("head " + pr.head_sha.slice(0, 8));
    facts.push(pr.watching ? "watching" : pr.state === "merged" || pr.state === "closed" ? "not watched (" + pr.state + ")" : "not watching");
    let poll = "no poll armed";
    if (rp) {
      poll = pr.next_poll_at ? "next poll " + until(pr.next_poll_at, now) : "poll armed (" + rung(rp.schedule) + ")";
      poll += rp.has_baseline ? " · baseline " + (rp.baseline_source || "?") : " · no baseline yet: the next tick runs the review agent";
    }
    const b = pr.budgets || {};
    const budget = (n, max, name) => ({ name, n: n || 0, max: max || 0, warn: max > 0 && (n || 0) >= max - 1, text: name + " " + (n || 0) + "/" + (max || "?") });
    return {
      title: pr.repo + " #" + pr.number,
      state: pr.state || "unknown",
      url: safeURL(pr.url),
      facts,
      ladder,
      poll,
      fixTasks: (pr.fix_tasks || []).map((n) => fixTask(n, tasks && tasks.get(n))),
      budgets: [budget(b.review_fix_cycles, b.max_review_fix_cycles, "review_fix_cycles"), budget(b.review_errors, b.max_review_errors, "review_errors")],
    };
  }

  // pullsNote explains a project with no PR cards.
  function pullsNote(project) {
    if (!project) return "";
    if (project.flags && project.flags.review) return "review is on: the review agent opens the PR once the project's commits are pushed.";
    return "no pull requests: this project does not run the review loop (review: false).";
  }

  // ---- intake and new project (§A.4.3, §G.5) ------------------------------

  const KEBAB = /^[a-z0-9]+(-[a-z0-9]+)*$/;

  // intakeHint says what the daemon will do with a request queued now,
  // given the project (handleIntakeFile, internal/daemon/intake.go). refuse
  // is set when the GUI should not offer the queue at all.
  function intakeHint(p) {
    const status = (p && p.status) || "";
    const live = (p && p.live) || {};
    if (!status) return { text: "", refuse: "the project agent has not filled in this project yet; queue intake once it is ready" };
    if ((live.sessions || []).includes("planning")) {
      return { text: "a planner is running: it will not see this request, so a follow-up planning run picks it up when this one ends" };
    }
    switch (status) {
      case "working":
        return { text: "the project is working: a planner runs alongside its tasks and turns this into tasks" };
      case "stopped":
        return { text: "", refuse: "the project is stopped, and queuing intake would flip it to ready; start it first" };
      case "blocked":
        return { text: "the project is blocked: the daemon flips it to ready (clearing blocked_reason) and plans this", warn: true };
      default:
        return { text: "the project is " + status + ": the daemon flips it to ready and plans this" };
    }
  }

  // intakeNameError checks an intake name as the server will (one kebab-case
  // segment, no .md); "" is fine (the server picks one).
  function intakeNameError(name) {
    name = String(name || "").trim();
    if (!name) return "";
    if (name.endsWith(".md")) return "leave off .md: the server adds it";
    if (!KEBAB.test(name)) return "lowercase letters, digits and single hyphens only";
    if (name.length > 100) return "at most 100 characters";
    return "";
  }

  // projectNameError checks a new work stream's name as the server will.
  function projectNameError(name, existing) {
    name = String(name || "").trim();
    if (!name) return "a name is required";
    if (!KEBAB.test(name)) return "lowercase letters, digits and single hyphens only";
    if (name.length > 63) return "at most 63 characters: it prefixes every sandbox name";
    if ((existing || []).includes(name)) return name + " already exists";
    return "";
  }

  // intakeGroups splits a project's intake list into pending (and blank)
  // requests, which can be withdrawn, and processed ones, newest first.
  function intakeGroups(items) {
    const newest = (a, b) => String(b.created_at || "").localeCompare(String(a.created_at || ""));
    const all = (items || []).slice();
    return {
      pending: all.filter((r) => r.state === "pending" || r.state === "empty").sort(newest),
      processed: all.filter((r) => r.state === "processed").sort(newest),
    };
  }

  // ---- inbox (§A.4.5, §G.5) -------------------------------------------------

  // sender renders a message's sender as the TUI does (postoffice
  // Sender.Display): "task fix-ci (workingman-messaging)", "step-up".
  function sender(from) {
    const f = from || {};
    const project = f.project ? String(f.project).split("/").slice(-2, -1)[0] || "" : "";
    const agent = f.agent || f.sandbox || "";
    if (f.task && project) return agent + " " + f.task + " (" + project + ")";
    if (f.task) return agent + " " + f.task;
    if (agent === "workingman") return "workingman agent";
    if (project) return agent + " (" + project + ")";
    return agent;
  }

  // closedText says how a closed message was closed: "answered via slack:
  // deny". It is what the TUI's reply line shows.
  function closedText(res) {
    if (!res) return "";
    const via = res.responder && res.responder.via;
    const head = res.outcome + (via ? " via " + via : "");
    if (res.outcome === "answered") return head + ": " + (res.option || res.response || "");
    if (res.outcome === "cancelled" && res.reason) return head + " — " + res.reason;
    return head;
  }

  // inboxItem is one inbox row's model: who asked, whether it can be
  // answered from here, and how. m is a GET /messages item; now is ms.
  // A step-up request is flagged: its body is the exact CHANGE line the
  // executor will apply, so it is drawn verbatim and there is no "approve
  // all" (§A.4.5).
  function inboxItem(m, now) {
    const open = m.state === "open";
    const expired = !!m.expired || (m.expires_at && Date.parse(m.expires_at) <= now);
    const request = m.kind === "request";
    let refusal = "";
    if (!open) refusal = "closed: " + closedText(m.resolution);
    else if (expired) refusal = "this message has expired";
    return {
      id: m.id,
      from: sender(m.from),
      subject: m.subject || "",
      body: m.body || "",
      kind: m.kind,
      stepUp: !!(m.from && (m.from.agent === "step-up" || m.from.sandbox === "step-up")),
      sandbox: (m.from && m.from.sandbox) || "",
      options: request ? (m.options || []).slice() : [],
      canReply: request,
      canDismiss: !request,
      open: open && !expired,
      refusal,
      closed: closedText(m.resolution),
      expires: m.expires_at ? until(m.expires_at, now) : "",
    };
  }

  // inboxOrder sorts like the TUI inbox: open first, then newest first.
  function inboxOrder(items) {
    return (items || []).slice().sort((a, b) => {
      const ao = a.state === "open" ? 0 : 1;
      const bo = b.state === "open" ? 0 : 1;
      if (ao !== bo) return ao - bo;
      return String(b.created_at || "").localeCompare(String(a.created_at || ""));
    });
  }

  function clip(text, n) {
    text = String(text === undefined || text === null ? "" : text);
    return text.length > n ? text.slice(0, n - 1) + "…" : text;
  }

  return {
    HIGHLIGHT, FAMILIES, family, taskOf, row, matches, Timeline, rung, until, fixTask, safeURL, pullCard, pullsNote,
    intakeHint, intakeNameError, projectNameError, intakeGroups,
    sender, closedText, inboxItem, inboxOrder,
  };
});
