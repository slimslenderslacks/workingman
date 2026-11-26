// workingman GUI: the agents map (docs/proposals/workingman-gui.md §G.4).
// The three workload helpers (workingman, sleepy-head, step-up) above the
// project workflow agents (project → planning → task → commit → branch,
// with review, archive and the wolf), the helpers' file-write edges into the
// workflow, and the gaps. Live parts come from GET /agents and /sessions;
// the capability facts are static, in workload.js (wm.wl.AGENT_FACTS).
"use strict";

(function () {
  const wm = window.wm;
  const { h, wl } = wm;
  const enc = encodeURIComponent;

  wm.screen("/agents", (_, main) => {
    main.classList.add("wide");
    const scope = h("select", { "aria-label": "project" }, h("option", { value: "" }, "every project"));
    const box = h("div", {}, h("p", { class: "muted" }, "Loading…"));
    main.append(
      h("div", { class: "page-head" },
        h("h1", {}, "Agents"),
        h("span", { class: "muted small" }, "who builds, runs and polices task-agent workloads · GET /api/v1/agents + /sessions"),
        h("label", { class: "muted small" }, "sandboxes for ", scope)),
      box
    );
    let projects = [];
    let timer = 0;
    const load = async () => {
      try {
        const [agents, sessions, plist, audit] = await Promise.all([
          wm.api.list("/agents"),
          wm.api.list("/sessions"),
          wm.api.list("/projects").catch(() => ({ items: [] })),
          wm.api.list("/audit?event=step_up_op&limit=500").catch(() => ({ items: [] })),
        ]);
        projects = plist.items;
        const keep = scope.value;
        scope.replaceChildren(h("option", { value: "" }, "every project"), ...projects.map((p) => h("option", { value: p.id }, p.work_stream)));
        scope.value = projects.some((p) => p.id === keep) ? keep : "";
        const project = projects.find((p) => p.id === scope.value) || null;
        box.replaceChildren(drawMap(wl.agentsMap(agents.items, sessions.items, project, audit.items), project));
      } catch (e) {
        box.replaceChildren(wm.errorNotice(e));
      }
    };
    scope.addEventListener("change", load);
    const reload = () => {
      clearTimeout(timer);
      timer = setTimeout(load, 200);
    };
    const offs = ["session.started", "session.updated", "session.ended", "project.created", "project.removed", "reset"].map((t) => wm.stream.on(t, reload));
    load();
    return () => {
      main.classList.remove("wide");
      clearTimeout(timer);
      offs.forEach((off) => off());
    };
  });

  const chip = (c) => h("span", { class: "chip chip-" + (c.cls === "open" ? "dynamic" : c.cls === "ok" ? "granted" : "plain") }, c.text);

  function drawMap(m, project) {
    const helpers = h("section", { class: "lane-box lane-helpers" },
      h("h2", {}, "Workload helpers ", h("span", { class: "muted small" }, "always-on / on-demand, belong to no project")),
      h("div", { class: "agent-row" }, m.helpers.map(helperBox)));

    const edges = h("div", { class: "agent-edges" },
      h("span", { class: "muted small" }, "helpers write files (the same contract as the GUI):"),
      m.edges.map((e) => h("span", { class: "edge-chip" }, e.from, " → ", h("code", {}, e.via), " → ", e.to)));

    const wf = new Map(m.workflow.map((a) => [a.kind, a]));
    const box = (kind) => workflowBox(wf.get(kind), project);
    const arrow = (via) => h("div", { class: "flow-arrow", title: via }, h("span", {}, "→"), h("span", { class: "small muted" }, via));
    const workflow = h("section", { class: "lane-box lane-workflow" },
      h("h2", {}, "Project workflow agents ", h("span", { class: "muted small" }, "one sandbox each, per project" + (project ? " · " + project.work_stream : ""))),
      h("div", { class: "flow" },
        box("project"), arrow("project.yaml"),
        box("planning"), arrow("tasks/*.yaml"),
        box("task"), arrow("success → queue"),
        box("commit"), arrow("ff-only"),
        h("div", { class: "agent-box branch-box" }, h("strong", {}, "project branch"), h("div", { class: "mono small" }, m.branch || "<branch>"))),
      h("div", { class: "flow flow-after" },
        box("review"), h("span", { class: "small muted" }, "← PR on the branch · fix / push tasks → task"),
        box("archive"), h("span", { class: "small muted" }, "← cleanup: true")),
      h("div", { class: "flow flow-wolf" },
        box("wolf"), h("span", { class: "small muted" }, "← any block: retries exhausted, partial integration, :wolf")));

    const facts = h("section", { class: "lane-box facts" },
      h("h2", {}, "What this screen shows"),
      h("ul", { class: "small" },
        h("li", {}, "Capability is decided in four places: planning (by prompt), the daemon (hard-coded review / archive / step-up grants), the runner (mounts, which kinds get a dynamic gateway) and step-up (at runtime, operator-approved)."),
        h("li", {}, "Tool permissions inside a sandbox are bypassed; the sandbox, its policies and the MCP gateway are the only boundary."),
        h("li", {}, "Clicking a live sandbox opens its card (inputs → capabilities → output)."),
        h("li", {}, "Green = granted by a narrow rule; amber = open-ended (rw roots, mcp-add).")),
      h("p", { class: "muted small" }, "Source: survey §7.1–7.4, §10, §11."));

    const gaps = h("section", { class: "lane-box gaps" },
      h("h2", {}, "Gaps the GUI shows honestly"),
      h("ul", { class: "small" },
        h("li", {}, "None of workingman / sleepy-head / step-up shapes a task's initial capabilities; planning does, by prompt, with no review step."),
        h("li", {}, "Task agents get a dynamic gateway by default (mcp-add over the whole catalog), so static_mcps is a floor, not a ceiling, unless --mcp-gateway-static."),
        h("li", {}, "Skills: Plan.Skills exists but nothing sets it; the card shows \"none\"."),
        h("li", {}, "Only the claude-acp harness exists; \"harness choice\" is one value today."),
        h("li", {}, "Step-up only reaches sandboxes granted post-office at planning time.")),
      h("p", { class: "future small" }, "Possible future (not today, R.4 Q7): a capability review gate between planning and task, where workingman or a step-up-style check compares the planner's grants with the task before launch."));

    return h("div", { class: "agents-map" }, helpers, edges, workflow, h("div", { class: "agents-notes" }, facts, gaps));
  }

  function helperBox(a) {
    return h("div", { class: "agent-box helper" + (a.kind === "step-up" ? " grants" : "") + (a.enabled ? "" : " off") },
      h("div", { class: "agent-head" }, h("strong", {}, a.title), h("span", { class: "agent-state st-" + (a.state === "running" ? "on" : a.enabled ? "idle" : "off") }, a.state)),
      h("p", { class: "small" }, a.purpose),
      h("div", { class: "mono small muted" }, "sandbox ",
        a.session ? wm.link("/sandboxes/" + enc(a.sandbox), a.sandbox) : a.sandbox,
        " · " + a.mode),
      h("div", { class: "chips" }, a.chips.map(chip)),
      a.extra.map((x) => h("div", { class: "small" }, x)),
      h("p", { class: "gap small" }, (a.kind === "step-up" ? "" : "GAP: ") + a.gap));
  }

  function workflowBox(a, project) {
    const live = a.live.map((s) =>
      h("li", {}, wm.link(s.task && (a.kind === "task" || a.kind === "commit") && s.project ? wm.taskCardHref(s.project, s.task) + (a.kind === "commit" ? "?sandbox=" + enc(s.sandbox) : "") : "/sandboxes/" + enc(s.sandbox), s.task || s.sandbox || s.id),
        !project && s.project ? h("span", { class: "muted" }, " · " + s.project.replace(/^p_\d+\./, "")) : null));
    return h("div", { class: "agent-box wf wf-" + a.kind + (a.highlight ? " decides" : "") + (a.live.length ? " has-live" : "") },
      h("div", { class: "agent-head" }, h("strong", {}, a.title), a.live.length ? h("span", { class: "agent-state st-on" }, a.live.length + " live") : null),
      h("div", { class: "small" }, a.purpose),
      h("div", { class: "mono small muted" }, a.sandbox),
      a.highlight ? h("div", { class: "decides-note small" }, a.highlight) : null,
      h("div", { class: "chips" }, a.chips.map(chip)),
      live.length ? h("ul", { class: "live-list small" }, live) : null);
  }
})();
