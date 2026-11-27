// workingman GUI: the Projects screens (docs/proposals/workingman-gui.md
// §G.1): the work-stream rail, and the project view with its task graph,
// commit-queue lane and selected-task strip. The model is graph.js (wm.pv);
// this file fetches, subscribes and draws.
//
// Writes (P.4), for a session with the write scope: "+ New project" and
// "Queue intake" open their forms (intake.js); the header's Stop/Start,
// Wolf…, Review, Cleanup, Hide/Show and Catch up are project actions
// (POST /projects/{id}/actions/{action}) sent with the project's etag as
// If-Match; Open session and the two shells open host tabs (§A.4.6). The
// selected-task strip's Retry…, Keep sandbox, Edit task… and its host tabs
// are task actions (taskactions.js), sent with the task's etag. archive, and
// Watch and Prompt (the session relay, P.5), stay disabled and name the TUI
// command that does them today.
"use strict";

(function () {
  const { h, pv } = window.wm;
  const wm = window.wm;
  const SVG = "http://www.w3.org/2000/svg";

  // s builds an SVG element; like wm.h, strings become text nodes.
  function s(tag, attrs, ...children) {
    const el = document.createElementNS(SVG, tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v === null || v === undefined || v === false) continue;
      if (k.startsWith("on") && typeof v === "function") el.addEventListener(k.slice(2), v);
      else el.setAttribute(k, String(v));
    }
    for (const c of children.flat(Infinity)) {
      if (c === null || c === undefined || c === false) continue;
      el.append(c instanceof Node ? c : document.createTextNode(String(c)));
    }
    return el;
  }

  // debounce runs fn once, ms after the last call.
  function debounce(fn, ms) {
    let timer = 0;
    const d = () => {
      clearTimeout(timer);
      timer = setTimeout(fn, ms);
    };
    d.cancel = () => clearTimeout(timer);
    return d;
  }

  const enc = encodeURIComponent;
  const PROJECT_EVENTS = ["project.created", "project.updated", "project.status", "project.removed", "project.live", "queue.updated", "reset"];

  // ---- work-stream rail ----------------------------------------------------

  // rail draws the left column: one card per project, with a filter, fed by
  // GET /projects and project.* events. selected is the open project's id.
  function rail(selected) {
    const list = h("div", { class: "rail-list" });
    const q = h("input", { type: "search", placeholder: "filter…", "aria-label": "filter work streams" });
    const status = h(
      "select",
      { "aria-label": "status" },
      ["any", "working", "ready", "blocked", "idle", "stopped", "merged"].map((v) => h("option", { value: v }, "status: " + v))
    );
    const hidden = h("input", { type: "checkbox" });
    const el = h(
      "aside",
      { class: "rail" },
      h("h2", {}, "Work streams"),
      h("div", { class: "rail-filter" }, q, status, h("label", { class: "muted" }, hidden, " hidden")),
      list,
      h("div", { class: "rail-foot" }, wm.writeAction("+ New project", "drop project.md under the orch root", { href: "/new-project" }))
    );
    let items = [];
    const draw = () => {
      const needle = q.value.trim().toLowerCase();
      const shown = items.filter(
        (p) =>
          (!status.value || status.value === "any" || p.status === status.value) &&
          (!needle || p.work_stream.toLowerCase().includes(needle) || (p.description || "").toLowerCase().includes(needle))
      );
      list.replaceChildren(...(shown.length ? shown.map(card) : [h("p", { class: "muted" }, "no work streams")]));
    };
    const card = (p) => {
      const f = pv.railFacts(p);
      const cls = ["rail-card", "st-" + p.status];
      if (p.id === selected) cls.push("selected");
      for (const b of f.badges) cls.push("has-" + b.kind);
      const bar = h("div", { class: "bar" });
      for (const seg of f.bar) {
        const part = h("span", { class: "seg st-" + seg.status, title: seg.status });
        part.style.flexGrow = String(seg.fraction);
        bar.append(part);
      }
      return wm.link(
        "/projects/" + enc(p.id),
        h("div", { class: cls.join(" ") },
          h("div", { class: "rail-head" }, h("strong", {}, p.work_stream), wm.status(p.status)),
          h("div", { class: "muted small" }, f.summary),
          f.badges.length ? h("div", { class: "badges" }, f.badges.map((b) => h("span", { class: "tag tag-" + b.kind, title: b.title }, b.text))) : null,
          f.note ? h("div", { class: "small note-line" }, f.note) : null,
          bar
        )
      );
    };
    const load = async () => {
      try {
        const l = await wm.api.list("/projects" + (hidden.checked ? "?hidden=any" : ""));
        items = l.items;
        draw();
      } catch (e) {
        list.replaceChildren(wm.errorNotice(e));
      }
    };
    q.addEventListener("input", draw);
    status.addEventListener("change", draw);
    hidden.addEventListener("change", load);
    const reload = debounce(load, 150);
    const offs = PROJECT_EVENTS.map((t) => wm.stream.on(t, reload));
    load();
    return {
      el,
      stop() {
        reload.cancel();
        offs.forEach((off) => off());
      },
    };
  }

  wm.screen("/", () => wm.navigate("/projects", true));

  wm.screen("/projects", (_, main) => {
    const r = rail("");
    main.classList.add("wide");
    main.append(
      h("div", { class: "pv" }, r.el, h("section", { class: "pv-body" }, h("p", { class: "notice" }, "Select a work stream to see its task graph and commit queue.")))
    );
    return () => {
      main.classList.remove("wide");
      r.stop();
    };
  });

  // ---- project view --------------------------------------------------------

  wm.screen("/projects/:id", async (params, main) => {
    const id = params.id;
    const r = rail(id);
    const body = h("section", { class: "pv-body" }, h("p", { class: "muted" }, "Loading…"));
    main.classList.add("wide");
    main.append(h("div", { class: "pv" }, r.el, body));

    let state = null;
    let selected = new URLSearchParams(location.search).get("task") || "";
    const pending = []; // events that arrive while loading
    const header = h("div", { class: "pv-header" });
    // actionNote is the last action's outcome; wolfOpen shows the wolf's
    // confirm row. Both outlive a redraw of the header.
    const actionNote = h("p", { class: "small action-note" });
    let wolfOpen = false;
    const wolfReason = h("input", { type: "text", "aria-label": "wolf reason", placeholder: "optional: what do you need help with?", maxlength: 500 });
    const banner = h("div");
    const graphBox = h("div", { class: "graph-scroll" });
    const laneBox = h("div", { class: "lane" });
    const strip = h("div", { class: "strip" });
    let transcript = null;
    // The selected task's controls (taskactions.js): a write's response, or
    // a 412's current, replaces the task in state.
    const tc = wm.taskControls((t) => {
      if (state) state.tasks.set(t.name, Object.assign(state.tasks.get(t.name) || {}, t));
    }, () => drawStrip());

    const load = async () => {
      const [p, tasks, graph, sessions] = await Promise.all([
        wm.api.get("/projects/" + enc(id)),
        wm.api.list("/projects/" + enc(id) + "/tasks"),
        wm.api.get("/projects/" + enc(id) + "/graph"),
        wm.api.list("/sessions?project=" + enc(id)),
      ]);
      state = pv.newState(p, tasks.items, graph, sessions.items, Math.min(tasks.cursor, sessions.cursor));
      for (const ev of pending.splice(0)) pv.apply(state, ev);
    };
    const refetchGraph = debounce(async () => {
      try {
        const [g, p] = await Promise.all([wm.api.get("/projects/" + enc(id) + "/graph"), wm.api.get("/projects/" + enc(id))]);
        state.graph = g;
        // The detail form has fields (description, repos) the summary in
        // project.updated lacks; keep live from the stream if it is newer.
        state.project = Object.assign(state.project, p);
        drawAll();
      } catch (e) {
        // a transient failure: the next event refetches
      }
    }, 200);
    const reloadAll = debounce(async () => {
      try {
        await load();
        drawAll();
      } catch (e) {
        body.replaceChildren(wm.errorNotice(e));
      }
    }, 200);

    function drawAll() {
      if (!state) return;
      if (state.removed) {
        body.replaceChildren(h("p", { class: "notice" }, "This work stream was removed (archived or deleted)."));
        return;
      }
      drawHeader();
      drawGraph();
      drawLane();
      drawStrip();
    }

    function drawHeader() {
      const p = state.project;
      const repos = (p.repos || []).map((r) => r.org + "/" + r.name).join(", ");
      const meta = [
        ["branch", h("code", {}, p.branch || "—")],
        repos ? ["repos", repos] : null,
        ["max_parallel_tasks", String(p.max_parallel_tasks || 1)],
        p.verify ? ["verify", h("code", {}, p.verify)] : null,
        (p.pull_requests || []).length || (p.flags && p.flags.review) ? ["", wm.link("/projects/" + enc(p.id) + "/pulls", pullsLabel(p))] : null,
        ["", wm.link("/timeline?project=" + enc(p.id), "timeline ▸")],
      ].filter(Boolean);
      header.replaceChildren(
        h("div", { class: "pv-title" },
          h("h1", {}, p.work_stream),
          wm.status(p.status),
          h("span", { class: "muted small meta" }, meta.map(([k, v], i) => [i ? " · " : "", k ? [k, " "] : null, v]))
        ),
        h("div", { class: "actions" }, pv.projectActions(p).map(actionButton)),
        ...(wolfOpen ? [wolfConfirm(p)] : []),
        actionNote
      );
      const notes = [];
      if (p.blocked_reason) notes.push(h("p", { class: "notice error" }, "blocked: ", p.blocked_reason));
      if (p.load_error) notes.push(h("p", { class: "notice error" }, "project.yaml: ", p.load_error));
      for (const e of (state.graph && state.graph.errors) || []) notes.push(h("p", { class: "notice warn" }, "task graph: ", e));
      banner.replaceChildren(...notes);
    }

    // actionButton draws one header action: a link to its form, a project
    // action the API serves, or the disabled button with its TUI command.
    function actionButton(a) {
      if (a.href) return wm.writeAction(a.label, a.command, a);
      if (!a.action) return wm.disabledAction(a.label, a.command, a.refusal, a.danger);
      const o = Object.assign({}, a, { title: a.tab ? pv.TAB_NOTE : a.command + " in the TUI" });
      o.onclick = a.action === "wolf" ? () => ((wolfOpen = !wolfOpen), drawHeader()) : (ev) => run(a.action, a.body || {}, ev && ev.target);
      return wm.writeAction(a.label, a.command, o);
    }

    // wolfConfirm is Wolf…'s confirm row: the wolf interrupts work, so it is
    // two clicks, with an optional reason for a project that has none.
    function wolfConfirm(p) {
      const keeps = p.status === "blocked" && p.blocked_reason;
      return h("div", { class: "wolf-confirm notice warn small" },
        "Summon the wolf: blocks ", p.work_stream, " (status: blocked) and the daemon launches the wolf agent. ",
        keeps ? "Its blocked_reason is kept: " : "Reason, written as blocked_reason: ",
        keeps ? h("code", {}, p.blocked_reason) : wolfReason, " ",
        h("button", { class: "action danger", type: "button", onclick: (ev) => run("wolf", keeps ? {} : { reason: wolfReason.value.trim() }, ev && ev.target) }, "Summon wolf"), " ",
        h("button", { class: "action", type: "button", onclick: () => ((wolfOpen = false), drawHeader()) }, "Cancel")
      );
    }

    // run sends a project action; the response is the project as its file
    // now is, the daemon's reaction follows on the stream. A 412 or 409
    // carries the project as it is now, which replaces the stale one.
    async function run(action, body, button) {
      if (button) button.setAttribute("disabled", "");
      actionNote.className = "small muted action-note";
      actionNote.textContent = action + ": sending…";
      try {
        const res = await wm.projectAction(state.project, action, body);
        if (res && res.project && res.project.id) Object.assign(state.project, res.project);
        if (action === "wolf") {
          wolfOpen = false;
          wolfReason.value = "";
        }
        actionNote.className = "small ok action-note";
        actionNote.textContent = (action === "open-shell" ? "shell " + body.where : action) + ": " + pv.actionDone(action, res);
      } catch (e) {
        if (e.current && e.current.id) Object.assign(state.project, e.current);
        actionNote.className = "small error action-note";
        actionNote.textContent = action + ": " + wm.actionError(e);
        if (e.status === 412) refetchGraph();
      }
      drawHeader();
    }

    function drawGraph() {
      const g = state.graph || { nodes: [], edges: [] };
      if (!g.nodes.length) {
        graphBox.replaceChildren(h("p", { class: "muted pad" }, "No tasks yet. The planning agent writes tasks/*.yaml."));
        return;
      }
      const L = pv.layout(g);
      const ctx = { graph: g, project: state.project, tasks: state.tasks, sessions: state.sessions, now: Date.now() };
      const statusOf = new Map(g.nodes.map((n) => [n.name, (state.tasks.get(n.name) || n).status]));
      const svg = s("svg", { class: "graph", width: L.width, height: L.height, viewBox: `0 0 ${L.width} ${L.height}`, role: "img", "aria-label": "task dependency graph" });
      svg.append(
        s("defs", {},
          s("marker", { id: "arrow", viewBox: "0 0 10 10", refX: 9, refY: 5, markerWidth: 7, markerHeight: 7, orient: "auto" }, s("path", { d: "M0,0 L10,5 L0,10 z", class: "arrowhead" })),
          s("marker", { id: "arrow-bad", viewBox: "0 0 10 10", refX: 9, refY: 5, markerWidth: 7, markerHeight: 7, orient: "auto" }, s("path", { d: "M0,0 L10,5 L0,10 z", class: "arrowhead bad" }))
        )
      );
      for (let d = 0; d < L.columns; d++) {
        svg.append(s("text", { x: 16 + d * (L.nodeW + 50) + 4, y: 20, class: "col-label" }, "depth " + d));
      }
      for (const e of L.edges) {
        const bad = pv.failedDep(statusOf.get(e.from));
        svg.append(s("path", { d: e.path, class: "edge" + (bad ? " bad" : ""), "marker-end": bad ? "url(#arrow-bad)" : "url(#arrow)" }, s("title", {}, e.from + " → " + e.to)));
      }
      for (const n of L.nodes) {
        const f = pv.nodeFacts(n.name, ctx);
        if (n.name === selected) f.classes.push("selected");
        const node = s(
          "g",
          { class: f.classes.join(" "), transform: `translate(${n.x},${n.y})`, tabindex: 0, role: "button", "aria-label": n.name + ": " + f.line },
          s("title", {}, [n.name, f.title].filter(Boolean).join("\n")),
          s("rect", { width: L.nodeW, height: L.nodeH, rx: 6 }),
          s("text", { x: 10, y: 19, class: "n-name" }, clip(n.name, 24)),
          s("text", { x: 10, y: 35, class: "n-line" }, clip(f.line, 30)),
          s("text", { x: 10, y: 49, class: "n-detail" }, clip(f.detail, 34))
        );
        const pick = () => select(n.name);
        node.addEventListener("click", pick);
        node.addEventListener("keydown", (e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            pick();
          }
        });
        svg.append(node);
      }
      graphBox.replaceChildren(svg);
    }

    function drawLane() {
      const ln = pv.lane(state.project, state.tasks, state.graph);
      const items = [];
      if (!ln.queued.length) items.push(h("li", { class: "muted lane-empty" }, "queue empty"));
      // The head of the queue lands first, so it sits nearest the branch.
      for (const q of ln.queued.slice().reverse()) {
        items.push(
          h("li", { class: "q" + (q.integrating ? " integrating" : "") },
            h("strong", {}, "#" + q.position + " ", taskLink(q.name)),
            h("div", { class: "small" + (q.integrating ? " ok" : " muted") },
              q.integrating ? "integrating: rebase → verify → ff-only" : q.waiting || (q.succeeded_at ? "succeeded_at " + new Date(q.succeeded_at).toLocaleTimeString() : "waiting")
            )
          )
        );
      }
      const landed = ln.landed.map((c) =>
        h("li", { class: "landed" },
          h("span", { class: "dot" }),
          h("code", {}, c.hash ? c.hash.slice(0, 7) : "(no commit)"),
          " ",
          taskLink(c.task),
          c.repo ? h("span", { class: "muted small" }, " · " + c.repo) : null,
          c.completed_at ? h("span", { class: "muted small" }, " · " + new Date(c.completed_at).toLocaleTimeString()) : null
        )
      );
      if (ln.base.length) {
        landed.push(h("li", { class: "landed base" }, h("span", { class: "dot" }), h("span", { class: "muted small" }, "base ", ln.base.map((b) => b.hash.slice(0, 7) + (ln.base.length > 1 ? " (" + b.repo + ")" : "")).join(", "))));
      }
      const failures = ln.failures.map((f) =>
        h("div", { class: "notice warn small" },
          h("strong", {}, taskLink(f.task), ": ", f.text), " → ", f.outcome,
          f.output ? h("pre", { class: "tailout" }, f.output) : null
        )
      );
      laneBox.replaceChildren(
        h("h2", {}, "Commit queue → project branch"),
        h("p", { class: "muted small" }, "Queued(): by succeeded_at · 1 commit agent", ln.reserved && !ln.integrating ? " · commit agent reserved" : ""),
        h("div", { class: "branch" },
          h("div", { class: "branch-name" }, h("code", {}, ln.branch || "(no branch)"), " (project branch)"),
          h("ol", { class: "queue" }, items),
          h("div", { class: "muted small sep" }, "landed (newest first)"),
          h("ol", { class: "landed-list" }, landed.length ? landed : [h("li", { class: "muted small" }, "nothing landed yet")])
        ),
        ...(failures.length ? [h("div", {}, h("h2", {}, "Integration failures"), failures)] : [])
      );
    }

    function taskLink(name) {
      return h("a", { href: "?task=" + enc(name), class: "tasklink", onclick: (e) => (e.preventDefault(), e.stopPropagation(), select(name)) }, name);
    }

    function select(name) {
      selected = selected === name ? "" : name;
      const url = new URL(location.href);
      if (selected) url.searchParams.set("task", selected);
      else url.searchParams.delete("task");
      history.replaceState(null, "", url.pathname + url.search);
      drawGraph();
      drawStrip();
    }

    function drawStrip() {
      const t = selected && state.tasks.get(selected);
      if (!t) {
        tc.set(null, null, null);
        stopTranscript();
        strip.replaceChildren(h("p", { class: "muted pad" }, "Select a task in the graph to see its sandbox."));
        return;
      }
      const sess = t.session ? state.sessions.get(t.session) : null;
      tc.set(t, state.project, sess);
      const c = t.capabilities || {};
      const sb = sess && sess.sandbox;
      const gw = sess && sess.gateway;
      const mcps = gw ? gw.servers || [] : c.static_mcps || [];
      const dynamic = gw ? gw.dynamic : false;
      const facts = [
        pv.attemptText(t, state.graph && state.graph.max_attempts),
        t.depends_on.length ? "depends_on " + t.depends_on.join(", ") : "no dependencies",
        t.touches && t.touches.length ? "touches " + t.touches.join(", ") : t.areas.length ? "areas " + t.areas.join(", ") : null,
        "exclusive: " + (t.exclusive ? "yes" : "no"),
      ].filter(Boolean);
      let output = t.task_branch || "";
      if (t.status === "committed") output += " → landed " + ((t.output.commits || []).map((x) => x.hash.slice(0, 7)).join(", ") || "(no commit)");
      else if (t.queue_position) output += " → queue #" + t.queue_position;
      else if (t.status === "success") output += " → queued";
      else output += " → (not yet queued)";
      const liveDot = sb && sb.live === true ? "live" : sb && sb.live === false ? "gone" : sess ? "unknown" : "none";
      const row = (k, ...v) => h("div", { class: "kv" }, h("span", { class: "k" }, k), h("span", { class: "v" }, ...v));
      const card = h("div", { class: "mini-card" },
        h("h3", {}, "Sandbox ", h("code", {}, t.sandbox || "—"), " ",
          h("span", { class: "live live-" + liveDot }, sess ? (liveDot === "live" ? "live" : "sbx: " + liveDot) + " · session " + sess.id : t.sandbox ? "no live session" : "")),
        row("image", h("code", {}, (sb && sb.image) || c.resolved_harness || c.harness || "—")),
        row("model", h("code", {}, (c.model || "default") + (c.resolved_model ? " → " + c.resolved_model : ""))),
        row("MCPs", mcps.length ? mcps.map((m) => h("span", { class: "tag tag-mcp" }, m)) : h("span", { class: "muted" }, "none"),
          dynamic ? h("span", { class: "warn small" }, " ⚠ dynamic gateway: mcp-find / mcp-add open") : null),
        row("network", h("code", {}, pv.policySummary(c.policies))),
        row("output", h("code", {}, output)),
        h("div", { class: "actions" },
          pv.taskActions(t, sess, state.project).map(tc.button),
          t.seed ? null : tc.keep(),
          wm.wl.inputActions(t).filter((a) => a.action === "edit").map(tc.button),
          wm.link("/projects/" + enc(state.project.id) + "/tasks/" + enc(t.name), "Full sandbox card ▸")),
        tc.panel()
      );
      const reason = t.failure_reason || t.blocked_reason;
      strip.replaceChildren(
        h("div", { class: "strip-head" },
          h("h2", {}, "Selected: ", t.name), wm.status(t.status), h("span", { class: "muted small" }, " ", facts.join(" · "))
        ),
        ...(reason ? [h("p", { class: "notice error small" }, reason)] : []),
        h("div", { class: "strip-body" }, card, transcriptPanel(t, sess))
      );
    }

    // transcriptPanel is the live transcript preview (wm.transcriptView):
    // the tail of the session's stream.log, polled while the task stays
    // selected. When the session ends the panel stays, labelled lost, until
    // another task is selected.
    function transcriptPanel(t, sess) {
      if (sess && (!transcript || transcript.sid !== sess.id)) {
        stopTranscript();
        transcript = wm.transcriptView({ sid: sess.id, session: sess, mode: "preview" });
        transcript.task = t.name;
      } else if (sess) {
        transcript.setSession(sess);
      } else if (transcript && transcript.task === t.name) {
        transcript.end();
      } else {
        stopTranscript();
      }
      if (transcript) return transcript.el;
      return h("div", { class: "transcript" },
        h("div", { class: "t-head" }, "transcript ", h("span", { class: "t-state t-lost" }, "no live session")),
        h("div", { class: "t-line muted" }, "a transcript is lost when its session ends: the wrapper deletes the session directory with its stream.log"));
    }

    function stopTranscript() {
      if (transcript) transcript.stop();
      transcript = null;
    }

    // Subscribe before loading, so nothing between the lists' cursor and
    // the first draw is lost; the backlog is applied once the lists arrive.
    const onEvent = (ev) => {
      if (ev.type === "reset") return reloadAll();
      if (!state) {
        if (ev.project === id) pending.push(ev);
        return;
      }
      const r = pv.apply(state, ev);
      if (!r.changed) return;
      drawAll();
      if (r.graph) refetchGraph();
    };
    const off = wm.stream.on("*", onEvent);
    // Running times on the nodes move on without events.
    const tick = setInterval(() => state && drawGraph(), 15000);

    try {
      await load();
    } catch (e) {
      off();
      clearInterval(tick);
      r.stop();
      body.replaceChildren(wm.errorNotice(e));
      return () => main.classList.remove("wide");
    }
    body.replaceChildren(
      header,
      banner,
      h("div", { class: "pv-main" },
        h("div", { class: "graph-panel" },
          h("div", { class: "panel-head" }, h("h2", {}, "Task graph"), h("span", { class: "muted small" }, "edges = depends_on · left → right by depth")),
          graphBox,
          legend()
        ),
        laneBox
      ),
      strip
    );
    drawAll();

    return () => {
      main.classList.remove("wide");
      off();
      clearInterval(tick);
      refetchGraph.cancel();
      reloadAll.cancel();
      stopTranscript();
      r.stop();
    };
  });

  function legend() {
    const item = (cls, label) => h("span", { class: "lg" }, h("span", { class: "sw " + cls }), label);
    return h("div", { class: "legend" },
      item("st-committed", "committed"),
      item("st-success", "success = queued for integration"),
      item("st-running", "running"),
      item("st-ready", "ready"),
      item("st-failed", "failed"),
      item("st-blocked", "blocked"),
      h("span", { class: "lg muted" }, "red dashed edge = dependency failed · dashed outline = deferred")
    );
  }

  // pullsLabel is the header's link to the project's PRs.
  function pullsLabel(p) {
    const prs = p.pull_requests || [];
    const open = prs.filter((x) => x.state !== "merged" && x.state !== "closed");
    if (!prs.length) return "pull requests ▸";
    return (open.length ? open.map((x) => "PR #" + x.number).join(", ") : prs.length + " PR" + (prs.length === 1 ? "" : "s")) + " ▸";
  }

  function clip(text, n) {
    text = String(text || "");
    return text.length > n ? text.slice(0, n - 1) + "…" : text;
  }
})();
