// workingman GUI: the timeline (docs/proposals/workingman-gui.md §G.5), the
// audit stream: GET /audit for what the daemon's ring still holds, then the
// event stream's audit events. Filterable by project, event family and
// highlight; each row links to the project, task, sandbox or session it
// names. The model is secondary.js (wm.sv).
"use strict";

(function () {
  const wm = window.wm;
  const { h, sv } = wm;

  wm.screen("/timeline", (_, main) => {
    main.classList.add("wide");
    const params = new URLSearchParams(location.search);
    const project = h("select", { "aria-label": "project" }, h("option", { value: "" }, "project: any"));
    const fam = h("select", { "aria-label": "events" }, h("option", { value: "" }, "events: all"), sv.FAMILIES.concat([{ key: "other", label: "other" }]).map((f) => h("option", { value: f.key }, "events: " + f.label)));
    const hi = h("input", { type: "checkbox" });
    const q = h("input", { type: "search", placeholder: "filter…", "aria-label": "filter events" });
    const body = h("tbody");
    const meta = h("p", { class: "muted small" });
    main.append(
      h("div", { class: "page-head" },
        h("h1", {}, "Timeline"),
        h("span", { class: "muted small" }, "the audit log · GET /api/v1/audit, then SSE audit events")),
      h("div", { class: "filters" }, project, fam, h("label", { class: "muted small" }, hi, " highlighted only"), q),
      h("table", { class: "timeline" }, h("thead", {}, h("tr", {}, ["time", "event", "", "links"].map((c) => h("th", {}, c)))), body),
      meta,
      h("p", { class: "muted small" },
        "Highlighted (orange): ", [...sv.HIGHLIGHT].join(", "), ". ",
        "api_write lines will appear here too once the GUI can write (P.4).")
    );
    const tl = new sv.Timeline(2000);
    let loaded = false;
    const pending = [];

    const filter = () => ({ project: project.value, family: fam.value, highlighted: hi.checked, q: q.value });
    const draw = () => {
      const rows = tl.rows(filter());
      body.replaceChildren(...(rows.length ? rows.slice(0, 500).map(drawRow) : [h("tr", {}, h("td", { colspan: 4, class: "muted" }, loaded ? "no events match" : "loading…"))]));
      const r = tl.range();
      meta.textContent = r
        ? `${rows.length} of ${tl.events.length} events · audit seq ${r.first}–${r.last}` + (rows.length > 500 ? " · showing the newest 500" : "") +
          (r.first > 1 ? " · older lines are only in the daemon's --audit-log file (the API serves its last 500)" : "")
        : "";
    };
    const drawRow = (r) =>
      h("tr", { class: "tl-row" + (r.highlight ? " hl" : "") },
        h("td", { class: "mono small", title: r.time }, clock(r.time)),
        h("td", { class: "mono" }, r.event, h("div", { class: "muted small" }, r.family)),
        h("td", {}, h("div", { class: "small" }, r.summary), r.detail ? h("div", { class: "small tl-detail" }, r.detail) : null),
        h("td", { class: "small" }, r.links.map((l, i) => [i ? " · " : "", wm.link(l.href, l.label)]))
      );

    const load = async () => {
      try {
        const [list, plist] = await Promise.all([wm.api.list("/audit?limit=500"), wm.api.list("/projects?hidden=any").catch(() => ({ items: [] }))]);
        const keep = project.value || params.get("project") || "";
        project.replaceChildren(h("option", { value: "" }, "project: any"), ...plist.items.map((p) => h("option", { value: p.id }, "project: " + p.work_stream)));
        project.value = plist.items.some((p) => p.id === keep) ? keep : "";
        tl.load(list.items);
        loaded = true;
        for (const ev of pending.splice(0)) tl.add(ev);
        draw();
      } catch (e) {
        body.replaceChildren(h("tr", {}, h("td", { colspan: 4 }, wm.errorNotice(e))));
      }
    };
    let timer = 0;
    const offs = [
      wm.stream.on("audit", (ev) => {
        if (!loaded) return void pending.push(ev.data);
        if (tl.add(ev.data)) {
          clearTimeout(timer);
          timer = setTimeout(draw, 100);
        }
      }),
      wm.stream.on("reset", () => {
        loaded = false;
        load();
      }),
    ];
    for (const el of [project, fam, hi]) el.addEventListener("change", draw);
    q.addEventListener("input", draw);
    if (params.get("family")) fam.value = params.get("family");
    load();
    return () => {
      main.classList.remove("wide");
      clearTimeout(timer);
      offs.forEach((off) => off());
    };
  });

  function clock(ts) {
    const d = new Date(ts);
    if (isNaN(d)) return "";
    const p = (n) => String(n).padStart(2, "0");
    return p(d.getHours()) + ":" + p(d.getMinutes()) + ":" + p(d.getSeconds());
  }
})();
