// workingman GUI: the Sandboxes screens (docs/proposals/workingman-gui.md
// §G.2, §G.3): the Sandboxes list, the task → sandbox mapping seen from the
// sandbox side, and the sandbox card, inputs → capabilities → output. The
// model is workload.js (wm.wl); this file fetches, subscribes and draws.
//
// Writes: "Queue intake" links to the project's intake form (intake.js)
// for a session with the write scope (P.4). Every other action button is
// drawn disabled and names the TUI command that does it today.
"use strict";

(function () {
  const wm = window.wm;
  const { h, wl } = wm;
  const enc = encodeURIComponent;

  function debounce(fn, ms) {
    let timer = 0;
    const d = () => {
      clearTimeout(timer);
      timer = setTimeout(fn, ms);
    };
    d.cancel = () => clearTimeout(timer);
    return d;
  }

  // follow refetches (debounced) on the given event types, and on reset.
  function follow(types, fn) {
    const d = debounce(fn, 200);
    const offs = types.concat(["reset"]).map((t) => wm.stream.on(t, d));
    return () => {
      d.cancel();
      offs.forEach((off) => off());
    };
  }

  // maybe resolves to null instead of failing: for the parts of a screen
  // that may be missing (sbx ls down, a session's prompt files gone).
  const maybe = (p) => p.then((v) => v, () => null);

  const action = (a) => (a.href ? wm.writeAction(a.label, a.command, a) : wm.disabledAction(a.label, a.command, a.refusal, a.danger));

  function taskHref(project, task) {
    return "/projects/" + enc(project) + "/tasks/" + enc(task);
  }

  // cardHref is where a sandbox's card lives: under its task when it has
  // one, else under /sandboxes.
  function cardHref(row) {
    if (row.project && row.task && (row.role === "task" || row.role === "commit")) return taskHref(row.project, row.task) + (row.role === "commit" ? "?sandbox=" + enc(row.name) : "");
    return "/sandboxes/" + enc(row.name);
  }

  // ---- Sandboxes list (§G.2) -------------------------------------------------

  wm.screen("/sandboxes", (_, main) => {
    const retainedOnly = h("input", { type: "checkbox" });
    const role = h("select", { "aria-label": "role" }, ["any", "task", "commit", "planning", "wolf", "review", "workingman", "step-up", "sleepy-head", "session"].map((v) => h("option", { value: v }, "role: " + v)));
    const body = h("tbody");
    const note = h("div");
    const meta = h("p", { class: "muted small" });
    main.append(
      h("div", { class: "page-head" },
        h("h1", {}, "Sandboxes"),
        h("span", { class: "muted small" }, "every sandbox sbx reports and every tracked session · GET /api/v1/sandboxes + /sessions")),
      h("p", { class: "muted small" },
        "One row per workload. A task's row links to its sandbox card; a retained sandbox is one no session uses: a task kept after failing, save_sandbox, or a :session sandbox nothing removes."),
      h("div", { class: "filters" }, role, h("label", { class: "muted small" }, retainedOnly, " retained only")),
      note,
      h("table", { class: "sandboxes" },
        h("thead", {}, h("tr", {}, ["sandbox", "role", "project / task", "state", "age", "grants", "image", ""].map((c) => h("th", {}, c)))),
        body),
      meta
    );
    let rows = [];
    const draw = () => {
      const shown = rows.filter((r) => (!retainedOnly.checked || r.retained) && (!role.value || role.value === "any" || r.role === role.value));
      body.replaceChildren(...(shown.length ? shown.map(drawRow) : [h("tr", {}, h("td", { colspan: 8, class: "muted" }, "no sandboxes"))]));
    };
    const drawRow = (r) =>
      h("tr", { class: "sb-row sb-" + r.cls },
        h("td", {}, wm.link(cardHref(r), h("code", {}, r.name || "—"))),
        h("td", {}, r.role || h("span", { class: "muted" }, "—")),
        h("td", {},
          r.project ? wm.link("/projects/" + enc(r.project) + (r.task ? "?task=" + enc(r.task) : ""), r.project.replace(/^p_\d+\./, "")) : h("span", { class: "muted" }, "—"),
          r.task ? [" / ", wm.link(taskHref(r.project, r.task), r.task)] : null),
        h("td", {}, h("span", { class: "sb-state sb-state-" + r.cls }, r.state)),
        h("td", { class: "mono" }, r.age),
        h("td", {}, grantBadge(r.grants)),
        h("td", { class: "mono small" }, r.image ? r.image.replace(/^.*\//, "") : ""),
        h("td", { class: "row-actions" },
          wl.cardActions(r.session ? { id: r.session, kind: r.role, persistent: r.persistent, acp: { attachable: r.attachable } } : null, r.name)
            .filter((a) => a.label !== "Prompt" && !a.label.startsWith("Prompt ("))
            .map(action))
      );
    const load = async () => {
      const [sbs, sess] = await Promise.all([
        wm.api.list("/sandboxes").catch((e) => ({ error: e })),
        wm.api.list("/sessions"),
      ]);
      const sandboxes = sbs.error ? null : sbs.items;
      note.replaceChildren(...(sbs.error ? [h("p", { class: "notice warn small" }, "sbx ls is unavailable (", sbs.error.message, "): only tracked sessions are listed.")] : []));
      // The tasks of every project that appears, for grant counts.
      const projects = new Set();
      for (const s of sess.items) if (s.project && s.task) projects.add(s.project);
      for (const b of sandboxes || []) if (b.project && b.task) projects.add(b.project);
      const tasks = new Map();
      await Promise.all(
        [...projects].map(async (p) => {
          const l = await maybe(wm.api.list("/projects/" + enc(p) + "/tasks"));
          for (const t of (l && l.items) || []) tasks.set(p + "/" + t.name, t);
        })
      );
      rows = wl.sandboxRows(sandboxes, sess.items, tasks, Date.now());
      const n = (cls) => rows.filter((r) => r.cls === cls).length;
      meta.textContent = `${rows.length} sandboxes · ${n("live")} live · ${n("retained")} retained · ${n("foreign")} not workingman's · cursor ${sess.cursor}`;
      draw();
    };
    const reload = async () => {
      try {
        await load();
      } catch (e) {
        body.replaceChildren(h("tr", {}, h("td", { colspan: 8 }, wm.errorNotice(e))));
      }
    };
    retainedOnly.addEventListener("change", draw);
    role.addEventListener("change", draw);
    const stop = follow(["session.started", "session.updated", "session.ended", "task.status", "task.updated"], reload);
    reload();
    return stop;
  });

  function grantBadge(g) {
    return h("span", { class: "grants" + (g.open ? " open" : ""), title: g.items.map((i) => (i.open ? "⚠ " : "") + i.text).join("\n") || "baseline only" },
      g.count + " grant" + (g.count === 1 ? "" : "s"), g.open ? " · " + g.open + " open-ended" : "");
  }

  // ---- sandbox card (§G.3) -----------------------------------------------------

  // A task's card: the sandbox its task agent runs in (or, with
  // ?sandbox=, the commit agent's, which inherits the task's capabilities).
  wm.screen("/projects/:id/tasks/:name", (params, main) => cardScreen(main, { project: params.id, task: params.name, sandbox: new URLSearchParams(location.search).get("sandbox") || "" }));

  // Any sandbox's card, by name. A task's or commit's sandbox shows its
  // task's card.
  wm.screen("/sandboxes/:name", (params, main) => cardScreen(main, { sandbox: params.name }));

  async function cardScreen(main, want) {
    main.classList.add("wide");
    const box = h("div", { class: "card-page" }, h("p", { class: "muted" }, "Loading…"));
    main.append(box);
    let projectId = want.project || "";
    let taskName = want.task || "";

    const load = async () => {
      const [status, sbs, sessions, audit] = await Promise.all([
        maybe(wm.api.get("/status")),
        maybe(wm.api.list("/sandboxes")),
        wm.api.list("/sessions"),
        maybe(wm.api.list("/audit?event=step_up_op&limit=500")),
      ]);
      let sb = sbs ? sbs.items.find((x) => x.name === want.sandbox) : null;
      let session = want.sandbox ? sessions.items.find((s) => s.sandbox && s.sandbox.name === want.sandbox) : null;
      if (!projectId && (sb || session)) {
        const owner = session || sb;
        const role = (session && session.kind) || (sb && sb.role);
        if (owner.project && owner.task && (role === "task" || role === "commit")) {
          projectId = owner.project;
          taskName = owner.task;
        }
      }
      let task = null, project = null, tasks = null, graph = null;
      if (projectId && taskName) {
        [task, project, tasks, graph] = await Promise.all([
          wm.api.get("/projects/" + enc(projectId) + "/tasks/" + enc(taskName)),
          wm.api.get("/projects/" + enc(projectId)),
          maybe(wm.api.list("/projects/" + enc(projectId) + "/tasks")),
          maybe(wm.api.get("/projects/" + enc(projectId) + "/graph")),
        ]);
        const name = want.sandbox || task.sandbox;
        if (!session) {
          session = task.session && !want.sandbox ? sessions.items.find((s) => s.id === task.session) : sessions.items.find((s) => s.sandbox && s.sandbox.name === name);
        }
        if (!sb && sbs) sb = sbs.items.find((x) => x.name === name) || null;
      } else if (!sb && !session) {
        if (sbs) throw new wm.APIError(404, "not_found", "no sandbox " + want.sandbox + " in sbx ls, and no session uses it");
        throw new wm.APIError(503, "unavailable", "sbx ls is unavailable and no session uses " + want.sandbox);
      }
      if (session && !project && session.project) project = await maybe(wm.api.get("/projects/" + enc(session.project)));
      const prompt = session ? await maybe(wm.api.get("/sessions/" + enc(session.id) + "/prompt")) : null;
      const c = wl.card({
        task,
        tasks: tasks ? new Map(tasks.items.map((t) => [t.name, t])) : null,
        project,
        session: session || null,
        sandbox: sb || null,
        prompt,
        audit: audit ? audit.items : [],
        roots: (status && status.daemon && status.daemon.roots) || [],
        maxAttempts: graph && graph.max_attempts,
      });
      c.sandboxFallback = want.sandbox || (task && task.sandbox) || "";
      c.sbxDown = !sbs;
      return { c, task, project, session };
    };

    // The transcript outlives the card's redraws: it keeps polling its
    // session, and keeps what it loaded, labelled lost, once that ends.
    let transcript = null;
    const transcriptFor = (session) => {
      if (session && (!transcript || transcript.sid !== session.id)) {
        if (transcript) transcript.stop();
        transcript = wm.transcriptView({ sid: session.id, session, mode: "preview" });
      } else if (session) {
        transcript.setSession(session);
      } else if (transcript) {
        transcript.end();
      }
      return transcript;
    };
    const draw = async () => {
      try {
        const r = await load();
        box.replaceChildren(drawCard(r, transcriptFor(r.session)));
      } catch (e) {
        box.replaceChildren(wm.errorNotice(e));
      }
    };
    await draw();
    const stop = follow(["task.status", "task.updated", "session.started", "session.updated", "session.ended", "queue.updated", "project.live"], draw);
    return () => {
      main.classList.remove("wide");
      stop();
      if (transcript) transcript.stop();
    };
  }

  function drawCard({ c, task, project, session }, transcript) {
    const name = c.name || c.sandboxFallback;
    const crumbs = [wm.link("/projects", "Projects"), " / "];
    if (project) crumbs.push(wm.link("/projects/" + enc(project.id), project.work_stream), " / ");
    if (task) crumbs.push("tasks / ", wm.link("/projects/" + enc(task.project) + "?task=" + enc(task.name), task.name), " / ");
    else crumbs.push(wm.link("/sandboxes", "sandboxes"), " / ");
    crumbs.push("sandbox");
    const g = c.header.grants;
    const head = h("div", { class: "card-head" },
      h("div", { class: "crumbs muted small" }, crumbs),
      h("div", { class: "card-title" },
        h("h1", {}, h("code", {}, name || "—")),
        h("span", { class: "live live-" + c.header.liveClass }, c.header.live),
        h("span", { class: "grants big" + (g.open ? " open" : ""), title: g.items.map((i) => (i.open ? "⚠ " : "") + i.text).join("\n") },
          g.count + " grant" + (g.count === 1 ? "" : "s") + " beyond baseline", g.open ? " · " + g.open + " open-ended" : "")),
      h("div", { class: "muted small" },
        ["kind " + (c.kind || "unknown"),
          c.header.session ? "session " + c.header.session : "no live session",
          c.header.started ? "started " + clock(c.header.started) : "",
          c.sbxDown ? "sbx ls unavailable" : ""].filter(Boolean).join(" · ")),
      h("div", { class: "actions" }, c.actions.map(action))
    );
    const cols = h("div", { class: "card-cols" },
      column("in", "① INPUTS", "what the workload was asked to do", inputs(c, task)),
      h("div", { class: "card-arrow", "aria-hidden": "true" }, "→"),
      column("caps", "② CAPABILITIES", "everything this sandbox can reach — nothing else", capabilities(c)),
      h("div", { class: "card-arrow", "aria-hidden": "true" }, "→"),
      column("out", "③ OUTPUT", "what leaves the sandbox", output(c, task, project))
    );
    const tr = h("section", { class: "card-transcript" },
      h("h2", {}, "Transcript"),
      transcript
        ? transcript.el
        : h("p", { class: "muted small" }, "No live session in this sandbox. A transcript is lost when its session ends: the wrapper deletes the session directory with its stream.log (R.4 Q4)."));
    return h("div", {}, head, cols, tr, legend(c));
  }

  function column(cls, title, sub, body) {
    return h("section", { class: "card-col col-" + cls }, h("h2", {}, title), h("div", { class: "muted small sub" }, sub), body);
  }

  const kv = (k, ...v) => h("div", { class: "kv" }, h("span", { class: "k" }, k), h("span", { class: "v" }, ...v));
  const block = (title, src, ...body) => h("div", { class: "cblock" }, h("h3", {}, title, src ? h("span", { class: "src" }, src) : null), ...body);

  function inputs(c, task) {
    const out = [];
    const p = c.inputs.prompt;
    if (p) {
      const pre = (label, pv) =>
        h("div", { class: "prompt-file" },
          h("div", { class: "mono small muted" }, label),
          h("pre", { class: "prompt" }, pv.lines.join("\n") + (pv.more ? "\n…" : "")),
          pv.total ? null : h("div", { class: "muted small" }, "(missing)"));
      out.push(block("Input prompt", "GET …/sessions/{sid}/prompt", pre(".orch/instructions.md", p.instructions), pre(".orch/context.yaml", p.context)));
    } else {
      out.push(block("Input prompt", "", h("p", { class: "muted small" },
        "The rendered .orch/instructions.md and .orch/context.yaml are read from a live session's working directory; there is none now. The opening prompt is always \"Read .orch/instructions.md and .orch/context.yaml, then follow the instructions.\"")));
    }
    const ti = c.inputs.task;
    if (ti) {
      const rows = ti.rows.map(([k, v]) =>
        k === "depends_on"
          ? kv(k, ti.deps.length ? ti.deps.map((d, i) => [i ? ", " : "", h("span", { class: "dep dep-" + d.status }, d.name + " " + d.mark)]) : "none")
          : kv(k, String(v)));
      const desc = h("details", {}, h("summary", {}, "description"), h("pre", { class: "desc" }, ti.description || "(none)"));
      out.push(block("Task metadata", "tasks/" + task.name + ".yaml", ...rows, desc));
    } else if (c.facts) {
      out.push(block("Agent", "", kv("role", c.facts.title), kv("does", c.facts.purpose), c.facts.mode ? kv("mode", c.facts.mode) : null));
    }
    const m = c.inputs.mounts;
    out.push(block("Mounts", "",
      ...(m.rows.length ? m.rows.map((r) => h("div", { class: "mount mono small" + (r.open ? " open" : "") }, h("span", { class: "mode" }, r.mode), " ", r.path, r.note ? h("span", { class: "muted" }, "  (" + r.note + ")") : null)) : [h("div", { class: "muted small" }, "unknown: no live session or sbx record")]),
      m.absent.length ? h("div", { class: "mount mono small absent" }, h("span", { class: "mode" }, "—"), " ", m.absent.join(", ")) : null,
      h("div", { class: "muted small" }, "(nothing else from the host is visible)")));
    if (c.inputActions.length) {
      out.push(h("div", { class: "cnote small" },
        "Inputs are read-only in the GUI. To change them, queue intake (the planner re-plans) or edit description / depends_on / touches / areas while the task is not running (§A.4.4).",
        h("div", { class: "actions" }, c.inputActions.map(action))));
    }
    return out;
  }

  function capabilities(c) {
    const k = c.caps;
    const out = [h("div", { class: "boundary-label" }, "SANDBOX BOUNDARY — sbx microVM")];
    out.push(block("Harness", "", h("div", { class: "mono" }, k.harness.image || "(image unknown)"),
      h("div", { class: "muted small" }, "harness: " + k.harness.harness + " (only one exists)" + (k.harness.kit ? " · kit: " + k.harness.kit : "") + (k.harness.resolved ? " · resolved_harness" : ""))));
    out.push(block("Model", "", h("div", { class: "mono" }, k.model.model + (k.model.resolved ? " → " + k.model.resolved : "")),
      h("div", { class: "muted small" }, "model → resolved_model · LLM credentials stay on the host (sbx secret)")));
    out.push(block("MCP servers (via gateway)", "gateway URL never shown",
      h("div", { class: "chips" }, k.mcp.chips.map((ch) => h("span", { class: "chip chip-" + ch.state, title: { granted: "granted", absent: "in the catalog, not granted", dynamic: "dynamic gateway: mcp-find / mcp-add" }[ch.state] }, ch.state === "dynamic" ? "⚠ dynamic: mcp-add" : ch.name))),
      ...k.mcp.notes.map((n) => h("div", { class: "muted small" }, n)),
      h("div", { class: "muted small" }, k.mcp.source === "gateway" ? "solid = granted · hatched = not granted" : k.mcp.source)));
    out.push(block("Skills", "", h("div", { class: "muted small" }, k.skills)));
    if (k.network) {
      out.push(block("Network policy", "task policies + gateway rules",
        ...k.network.map((r) => h("div", { class: "rule mono small rule-" + r.cls }, h("span", { class: "act" }, r.action), " network ", r.resource, r.note ? h("span", { class: "muted" }, "  (" + r.note + ")") : null))));
      out.push(block("Filesystem policy", "",
        ...(k.filesystem.length ? k.filesystem.map((r) => h("div", { class: "rule mono small rule-" + r.cls }, h("span", { class: "act" }, r.action), " filesystem ", r.resource)) : [h("div", { class: "muted small" }, "default (mounts only)")])));
    } else {
      out.push(block("Policies", "", h("div", { class: "muted small" }, k.policyNote)));
    }
    out.push(block("Runtime changes", "step_up_op",
      ...(k.runtime.length
        ? k.runtime.map((r) => h("div", { class: "rule mono small " + (r.status === "applied" ? "rule-allow" : "rule-deny") }, clock(r.time), " ", r.op, " ", r.resource, " → ", r.status, h("div", { class: "muted" }, r.detail)))
        : [h("div", { class: "muted small" }, "none. A step-up grant would appear here, with the persisted policies: rule it added.")])));
    return [h("div", { class: "boundary" }, out)];
  }

  function output(c, task, project) {
    const o = c.output;
    if (!o) {
      return [block("Output", "", h("p", { class: "small" }, c.outputNote || "not a task workload: no branch or commit queue"))];
    }
    const out = [];
    out.push(block("Task branch", "", h("div", { class: "mono" }, o.branch || "—"),
      h("div", { class: "muted small" }, o.commits.length ? o.commits.length + " commit" + (o.commits.length === 1 ? "" : "s") + " landed" : "commits on the task branch enter the queue on success")));
    out.push(block("Result", "", h("div", {}, wm.status(o.status), " ", h("span", { class: "small" }, o.result))));
    out.push(block("Integration (commit agent)", "",
      h("ol", { class: "stepper" }, o.stepper.steps.map((s) => h("li", { class: "step step-" + s.state }, s.label))),
      h("div", { class: "muted small" }, o.stepper.note),
      o.integration ? h("div", { class: "notice warn small" }, wm.pv.integrationText(o.integration), o.integration.output ? h("pre", { class: "tailout" }, o.integration.output) : null) : null,
      o.commitSandbox ? h("div", { class: "small" }, "own sandbox: ", wm.link(taskHref(task.project, task.name) + "?sandbox=" + enc(o.commitSandbox), h("code", {}, o.commitSandbox)), h("span", { class: "muted" }, " (inherits this task's capabilities)")) : null));
    out.push(block("Lands on", "", h("div", { class: "mono" }, (o.landsOn || "—") + "  (project branch)"),
      o.commits.length ? h("ul", { class: "commits mono small" }, o.commits.map((x) => h("li", {}, String(x.hash).slice(0, 7) + " " + x.repo))) : h("div", { class: "muted small" }, "commits[] · summary · created_files once committed"),
      o.summary ? h("p", { class: "small" }, o.summary) : null,
      o.created.length ? h("div", { class: "muted small" }, "created: " + o.created.join(", ")) : null));
    out.push(block("Sandbox after exit", "", h("div", { class: "small" }, o.after.rule), h("div", { class: "small" }, o.after.now),
      h("div", { class: "actions" }, wm.disabledAction("Keep sandbox " + (o.after.keep ? "☑" : "☐"), "edit save_sandbox in tasks/" + task.name + ".yaml"))));
    out.push(o.touches ? h("div", { class: "notice warn small" }, o.touches) : h("div", { class: "muted small cnote" }, "touches_actual is compared to touches after landing: touches_exceeded would show here as a warning."));
    return out;
  }

  function legend(c) {
    const g = c.header.grants;
    return h("div", { class: "legend card-legend" },
      h("strong", {}, "Least privilege, visually"),
      h("span", { class: "lg" }, h("span", { class: "chip chip-granted" }, "x"), " granted capability"),
      h("span", { class: "lg" }, h("span", { class: "chip chip-absent" }, "x"), " in the catalog but NOT granted"),
      h("span", { class: "lg" }, h("span", { class: "chip chip-dynamic" }, "x"), " open-ended (dynamic gateway, allow **, rw roots)"),
      h("span", { class: "lg muted" }, "baseline = harness, model, gateway port, LLM proxy." + (g.items.length ? " Grants here: " + g.items.map((i) => i.text).join(", ") + "." : " Nothing beyond it."))
    );
  }

  function clock(ts) {
    const d = new Date(ts);
    if (isNaN(d)) return "";
    return String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0");
  }

  wm.cardHref = cardHref;
  wm.taskCardHref = taskHref;
})();
