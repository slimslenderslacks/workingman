// workingman GUI: a project's pull requests (docs/proposals/workingman-gui.md
// §G.5): each PR in pull_requests[] with its state, the review poll's
// ladder, rung and next poll, the fix tasks sourced from it and the review
// budgets. The GUI never calls GitHub; it links out. The model is
// secondary.js (wm.sv).
//
// Read-only: "Poll now" is drawn disabled with its TUI command (P.3).
"use strict";

(function () {
  const wm = window.wm;
  const { h, sv } = wm;
  const enc = encodeURIComponent;

  wm.screen("/projects/:id/pulls", (params, main) => {
    const id = params.id;
    const box = h("div", {}, h("p", { class: "muted" }, "Loading…"));
    main.append(box);
    const load = async () => {
      const [project, pulls, tasks] = await Promise.all([
        wm.api.get("/projects/" + enc(id)),
        wm.api.list("/projects/" + enc(id) + "/pulls"),
        wm.api.list("/projects/" + enc(id) + "/tasks").catch(() => ({ items: [] })),
      ]);
      return { project, pulls: pulls.items, tasks: new Map(tasks.items.map((t) => [t.name, t])) };
    };
    const draw = async () => {
      try {
        const r = await load();
        box.replaceChildren(drawPage(r));
      } catch (e) {
        box.replaceChildren(wm.errorNotice(e));
      }
    };
    let timer = 0;
    const later = () => {
      clearTimeout(timer);
      timer = setTimeout(draw, 200);
    };
    const offs = ["pr.updated", "project.live", "project.updated", "task.status", "task.updated", "queue.updated", "reset"].map((t) =>
      wm.stream.on(t, (ev) => (ev.type === "reset" || ev.project === id) && later())
    );
    // "next poll in …" moves on without events.
    const tick = setInterval(draw, 30000);
    draw();
    return () => {
      clearTimeout(timer);
      clearInterval(tick);
      offs.forEach((off) => off());
    };
  });

  function drawPage({ project, pulls, tasks }) {
    const now = Date.now();
    // The project view's :review button, with the TUI's refusal text.
    const review = wm.pv.projectActions(project).find((a) => a.command === ":review");
    const poll = wm.disabledAction("Poll now", review.command, review.refusal);
    const cards = pulls.map((pr) => drawCard(sv.pullCard(pr, tasks, now), project.id));
    return h("div", { class: "pulls" },
      h("div", { class: "crumbs muted small" }, wm.link("/projects", "Projects"), " / ", wm.link("/projects/" + enc(project.id), project.work_stream), " / pull requests"),
      h("div", { class: "page-head" },
        h("h1", {}, "Pull requests · ", project.work_stream),
        h("span", { class: "muted small" }, "GET /api/v1/projects/{id}/pulls · pr.updated")),
      h("div", { class: "actions" }, poll, h("span", { class: "muted small" }, " enabled only on idle; writes review_now: true")),
      ...(cards.length ? cards : [h("p", { class: "notice" }, sv.pullsNote(project))]),
      h("p", { class: "muted small" },
        "The GUI never calls GitHub itself: the state is project.yaml pull_requests[] plus the daemon's review_poll. ",
        wm.link("/timeline?project=" + enc(project.id) + "&family=review", "Review events in the timeline ▸"))
    );
  }

  function drawCard(c, projectId) {
    return h("section", { class: "pr-card" },
      h("div", { class: "pr-head" },
        h("strong", { class: "mono" }, c.title), " ",
        h("span", { class: "pr-state pr-" + c.state }, c.state), " ",
        c.url ? h("a", { href: c.url, target: "_blank", rel: "noopener noreferrer" }, "open on GitHub ↗") : null),
      h("div", { class: "small" }, c.facts.join(" · "), " · poll ladder ",
        c.ladder.map((r, i) => [i ? "→" : "", r.current ? h("strong", { class: "rung current" }, "[" + r.label + "]") : h("span", { class: "rung" }, r.label)])),
      h("div", { class: "small muted" }, c.poll),
      h("h3", {}, "Fix tasks from review"),
      c.fixTasks.length
        ? h("ul", { class: "fix-tasks" }, c.fixTasks.map((f) => h("li", {}, wm.link("/projects/" + enc(projectId) + "?task=" + enc(f.name), f.name), h("span", { class: "muted small" }, " ", [f.kind, f.text].filter(Boolean).join(" · ")))))
        : h("p", { class: "muted small" }, "none yet: the review agent turns unresolved threads and failed checks into tasks"),
      h("div", { class: "budgets small" }, "budgets (the project blocks when one is exceeded): ",
        c.budgets.map((b, i) => [i ? " · " : "", h("span", { class: b.warn ? "warn" : "" }, b.text)]))
    );
  }
})();
