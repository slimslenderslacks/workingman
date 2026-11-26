// workingman GUI: the intake pane and the new-project form
// (docs/proposals/workingman-gui.md §G.5, §A.4.3), the GUI's first writes
// (P.4). The intake pane is a markdown box that queues
// <project>/intake/<name>.md (POST /projects/{id}/intake), with a hint
// about what the daemon will do with it, the pending requests with a
// best-effort Withdraw (DELETE …/intake/{name}, If-Match the request's
// etag), and the processed ones. The new-project form writes
// <root>/<name>/project.md (POST /projects). Each submit carries an
// Idempotency-Key kept until it succeeds, so a resend is done once. The
// model is secondary.js (wm.sv).
"use strict";

(function () {
  const wm = window.wm;
  const { h, sv } = wm;
  const enc = encodeURIComponent;

  // field is a labelled form control with its error line.
  function field(label, control, hint) {
    const err = h("div", { class: "field-error small" });
    return { el: h("label", { class: "field" }, h("span", { class: "field-label" }, label), control, hint ? h("span", { class: "muted small" }, hint) : null, err), err };
  }

  // enable turns a control on or off; title explains a disabled one.
  function enable(el, on, title) {
    if (on) el.removeAttribute("disabled");
    else el.setAttribute("disabled", "");
    if (title !== undefined) el.setAttribute("title", title);
  }

  // submitter runs a write from a form: it disables the button while the
  // request is out and keeps the Idempotency-Key until the write succeeds,
  // so a resend after a dropped response is not done twice. run returns what
  // the status line shows: text or a node.
  function submitter(button, status, run) {
    let key = "";
    return async () => {
      if (button.disabled) return;
      key = key || wm.api.idemKey();
      enable(button, false);
      status.className = "small muted";
      status.textContent = "sending…";
      try {
        const msg = await run(key);
        key = "";
        status.className = "small ok";
        status.replaceChildren(msg);
      } catch (e) {
        // A refusal is final for this request: the next submit is a new one.
        if (e instanceof wm.APIError && e.status < 500) key = "";
        status.className = "small error";
        status.textContent = e instanceof wm.APIError ? e.status + " " + (e.code || "") + ": " + e.message : String(e);
      } finally {
        enable(button, true);
      }
    };
  }

  // ---- intake ----------------------------------------------------------------

  wm.screen("/projects/:id/intake", (params, main) => {
    const id = params.id;
    const box = h("div", {}, h("p", { class: "muted" }, "Loading…"));
    main.append(box);
    let project = null;
    const name = h("input", { type: "text", placeholder: "optional: add-sandbox-card", "aria-label": "request name", spellcheck: "false" });
    const body = h("textarea", { rows: 8, placeholder: "What should the planner do? Markdown.", "aria-label": "request" });
    const nameF = field("name", name, "lowercase kebab-case, without .md; left blank, the server picks gui-<time>-<hex>");
    const bodyF = field("request", body);
    const hint = h("p", { class: "small" });
    const send = h("button", { class: "action primary", type: "button" }, "Queue intake");
    const status = h("div", { class: "small" });
    const pending = h("div");
    const processed = h("div");
    // listNote is a Withdraw's outcome; it sits above the list, which a
    // reload redraws.
    const listNote = h("p", { class: "small" });

    name.addEventListener("input", () => (nameF.err.textContent = sv.intakeNameError(name.value)));
    send.addEventListener(
      "click",
      submitter(send, status, async (key) => {
        const n = name.value.trim();
        const nerr = sv.intakeNameError(n);
        nameF.err.textContent = nerr;
        bodyF.err.textContent = body.value.trim() ? "" : "the request must not be blank";
        if (nerr || !body.value.trim()) throw new Error("fix the form first");
        const res = await wm.api.post("/projects/" + enc(id) + "/intake", n ? { name: n, body: body.value } : { body: body.value }, { key });
        name.value = "";
        body.value = "";
        loadList();
        return "queued intake/" + res.intake.file + " · watch for intake.processed";
      })
    );

    function drawForm() {
      const hn = sv.intakeHint(project);
      hint.className = "small" + (hn.warn ? " warn" : "") + (hn.refuse ? " error" : "");
      hint.textContent = hn.refuse || hn.text;
      const allowed = wm.can("write") && !hn.refuse;
      enable(send, allowed, allowed ? "" : hn.refuse || "this session lacks the write scope; in the TUI: drop a .md file into the project's intake/ directory");
    }

    function row(r, canWithdraw) {
      const facts = [r.state, r.created_at ? new Date(r.created_at).toLocaleString() : ""].filter(Boolean).join(" · ");
      const out = h("li", { class: "intake-row" }, h("code", {}, r.file), " ", h("span", { class: "muted small" }, facts));
      if (canWithdraw) {
        const b = wm.writeAction("Withdraw", "rename intake/" + r.file + " to " + r.name + ".withdrawn", {
          title: "best-effort: the planner may take it first",
          onclick: () => withdraw(r, b),
        });
        out.append(" ", b);
      }
      return out;
    }

    async function withdraw(r, b) {
      enable(b, false);
      try {
        await wm.api.del("/projects/" + enc(id) + "/intake/" + enc(r.name), { ifMatch: r.etag });
        listNote.className = "small ok";
        listNote.textContent = "withdrew " + r.file + " (now " + r.name + ".withdrawn)";
        loadList();
      } catch (e) {
        listNote.className = "small error";
        listNote.textContent =
          e.status === 412
            ? r.file + " changed since it was loaded; the list is reloaded, check it and try again"
            : (e.status ? e.status + " " + (e.code || "") + ": " : "") + (e.message || String(e));
        enable(b, true);
        if (e.status === 412 || e.status === 409) loadList();
      }
    }

    async function loadList() {
      try {
        const l = await wm.api.list("/projects/" + enc(id) + "/intake");
        const g = sv.intakeGroups(l.items);
        pending.replaceChildren(
          h("h2", {}, "Pending (", String(g.pending.length), ")"),
          g.pending.length ? h("ul", { class: "intake-list" }, g.pending.map((r) => row(r, r.state === "pending"))) : h("p", { class: "muted small" }, "none")
        );
        processed.replaceChildren(
          h("h2", {}, "Processed (", String(g.processed.length), ")"),
          g.processed.length
            ? h("ul", { class: "intake-list" }, g.processed.map((r) => row(r, false)))
            : h("p", { class: "muted small" }, "none yet"),
          h("p", { class: "muted small" }, "A processed request does not record which tasks it produced (R.4 Q12); see the planning run in the timeline.")
        );
      } catch (e) {
        pending.replaceChildren(wm.errorNotice(e));
      }
    }

    const load = async () => {
      try {
        project = await wm.api.get("/projects/" + enc(id));
      } catch (e) {
        box.replaceChildren(wm.errorNotice(e));
        return;
      }
      drawForm();
      box.replaceChildren(
        h("div", { class: "crumbs muted small" }, wm.link("/projects", "Projects"), " / ", wm.link("/projects/" + enc(id), project.work_stream), " / intake"),
        h("div", { class: "page-head" }, h("h1", {}, "Intake · ", project.work_stream), wm.status(project.status)),
        h("section", { class: "form intake-form" }, nameF.el, bodyF.el, hint, h("div", { class: "actions" }, send, status)),
        listNote,
        pending,
        processed
      );
      loadList();
    };
    let timer = 0;
    const later = (fn) => () => {
      clearTimeout(timer);
      timer = setTimeout(fn, 150);
    };
    const offs = [
      ...["intake.created", "intake.updated", "intake.processed", "intake.removed"].map((t) => wm.stream.on(t, (ev) => ev.project === id && later(loadList)())),
      ...["project.status", "project.updated", "project.live"].map((t) =>
        wm.stream.on(t, (ev) => {
          if (ev.project !== id || !project) return;
          const p = (ev.data && (ev.data.project || ev.data)) || {};
          if (p.status !== undefined) project = Object.assign(project, p);
          drawForm();
        })
      ),
      wm.stream.on("reset", later(load)),
    ];
    load();
    return () => {
      clearTimeout(timer);
      offs.forEach((off) => off());
    };
  });

  // ---- new project -----------------------------------------------------------

  wm.screen("/new-project", async (_, main) => {
    let roots = [];
    let existing = [];
    try {
      const [st, pl] = await Promise.all([wm.api.get("/status"), wm.api.list("/projects?hidden=any")]);
      roots = (st.daemon && st.daemon.roots) || [];
      existing = pl.items;
    } catch (e) {
      main.append(wm.errorNotice(e));
      return;
    }
    const name = h("input", { type: "text", placeholder: "gui-wireframes", "aria-label": "project name", spellcheck: "false" });
    const root = h("select", { "aria-label": "root" }, roots.map((r, i) => h("option", { value: String(i) }, r)));
    const desc = h("textarea", { rows: 10, placeholder: "What is this work stream for? The project agent fills in repos, branch and the rest from it.", "aria-label": "description" });
    const nameF = field("name", name, "lowercase kebab-case; it becomes the directory, the branch default and the sandbox-name prefix");
    const rootF = field("root", root);
    const descF = field("description", desc);
    const send = h("button", { class: "action primary", type: "button" }, "Create project");
    const status = h("div", { class: "small" });
    const namesIn = () => existing.filter((p) => String(root.value || 0) === String(p.id.slice(2, p.id.indexOf(".")))).map((p) => p.work_stream);
    name.addEventListener("input", () => (nameF.err.textContent = name.value ? sv.projectNameError(name.value, namesIn()) : ""));
    if (!wm.can("write")) enable(send, false, "this session lacks the write scope; in the TUI: drop project.md under the orch root");
    send.addEventListener(
      "click",
      submitter(send, status, async (key) => {
        const n = name.value.trim();
        const nerr = sv.projectNameError(n, namesIn());
        nameF.err.textContent = nerr;
        descF.err.textContent = desc.value.trim() ? "" : "the project agent plans from the description: it must not be blank";
        if (nerr || !desc.value.trim()) throw new Error("fix the form first");
        const res = await wm.api.post("/projects", { root: Number(root.value || 0), name: n, description: desc.value }, { key });
        existing.push({ id: res.id, work_stream: n });
        return h("span", {},
          "seeded ", h("code", {}, res.path), ": the daemon writes project.yaml and the project agent fills it in. ",
          res.warning ? h("span", { class: "warn" }, res.warning, ". ") : null,
          wm.link("/projects/" + enc(res.id), "Open " + n + " ▸")
        );
      })
    );
    main.append(
      h("div", { class: "crumbs muted small" }, wm.link("/projects", "Projects"), " / new project"),
      h("h1", {}, "New project"),
      h("p", { class: "muted small" }, "Writes ", h("code", {}, "<root>/<name>/project.md"), ", the same seed a human drops by hand (§A.4.3). Nothing else: the daemon and the project agent do the rest."),
      h("section", { class: "form" }, nameF.el, roots.length > 1 ? rootF.el : null, descF.el, h("div", { class: "actions" }, send, status))
    );
  });
})();
