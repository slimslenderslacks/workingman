// workingman GUI: the task actions (docs/proposals/workingman-gui.md
// §A.4.4, §A.4.6, P.4), shared by the project view's selected-task strip
// (projects.js) and the sandbox card (sandboxes.js).
//
// For a session with the write scope: Retry… (R1: a confirm row with
// "reset attempts" and what the retry does to the project), Keep sandbox
// (save_sandbox), Edit task… (a merge-patch of description, depends_on,
// touches and areas, which the server checks with taskgraph.Load), and the
// host tabs, Open in my terminal and Shell: worktree, which open in the
// terminal the daemon runs in. Every write sends the task's etag as
// If-Match: a write that landed since the task was loaded is a 412, and the
// task as it is now replaces the stale one. Without the scope each control
// is the disabled button with its TUI command.
"use strict";

(function () {
  const wm = window.wm;
  const { h } = wm;

  const list = (v) => String(v || "").split(",").map((x) => x.trim()).filter(Boolean);
  const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);

  // taskControls keeps one task's controls across redraws: the confirm row
  // or edit form that is open, what was typed in it, and the last outcome.
  // onTask(task) is called with the task as a write left it (or as a 412
  // found it), and redraw() after any change; the caller redraws from them.
  wm.taskControls = function (onTask, redraw) {
    let task = null, project = null, session = null;
    let open = ""; // "" | "retry" | "edit"
    const note = h("p", { class: "small action-note" });
    const reset = h("input", { type: "checkbox", "aria-label": "reset attempts" });
    reset.checked = true;
    const retryNote = h("input", { type: "text", "aria-label": "retry note", placeholder: "optional: a note for the audit log", maxlength: 500 });
    const desc = h("textarea", { rows: 6, "aria-label": "description" });
    const deps = h("input", { type: "text", "aria-label": "depends_on", placeholder: "task names, comma-separated" });
    const touches = h("input", { type: "text", "aria-label": "touches", placeholder: "globs, comma-separated (repo dir first)" });
    const noTouches = h("input", { type: "checkbox", "aria-label": "no touches hint" });
    const areas = h("input", { type: "text", "aria-label": "areas", placeholder: "kebab-case tags, comma-separated" });

    function set(t, p, s) {
      if (t && task && t.name !== task.name) {
        open = "";
        note.textContent = "";
      }
      task = t;
      project = p;
      session = s;
    }

    function fillEdit() {
      desc.value = task.description || "";
      deps.value = (task.depends_on || []).join(", ");
      noTouches.checked = task.touches === null || task.touches === undefined;
      touches.value = (task.touches || []).join(", ");
      areas.value = (task.areas || []).join(", ");
    }

    // patch is the merge-patch of what the form changed, {} when nothing.
    function patch() {
      const out = {};
      if (desc.value !== (task.description || "")) out.description = desc.value;
      if (!same(list(deps.value), task.depends_on || [])) out.depends_on = list(deps.value);
      const was = task.touches === null || task.touches === undefined ? null : task.touches;
      const now = noTouches.checked ? null : list(touches.value);
      if (!same(now, was)) out.touches = now;
      if (!same(list(areas.value), task.areas || [])) out.areas = list(areas.value);
      return out;
    }

    async function run(action, body, button) {
      if (button) button.setAttribute("disabled", "");
      note.className = "small muted action-note";
      note.textContent = action + ": sending…";
      try {
        const res = action === "edit" ? await wm.editTask(task, body) : await wm.taskAction(task, action, body);
        open = "";
        if (res && res.task && res.task.name) {
          task = Object.assign({}, task, res.task);
          onTask(task);
        }
        note.className = "small ok action-note";
        note.textContent = action + ": " + (action === "edit" ? (res && res.noop ? "nothing changed" : "saved tasks/" + task.name + ".yaml · watch for task.updated") : wm.pv.actionDone(action, res));
      } catch (e) {
        if (e.current && e.current.name) {
          task = Object.assign({}, task, e.current);
          onTask(task);
          if (open === "edit") fillEdit();
        }
        note.className = "small error action-note";
        note.textContent = action + ": " + wm.actionError(e, "task");
      }
      redraw();
    }

    const toggle = (what) => () => {
      open = open === what ? "" : what;
      if (open === "edit") fillEdit();
      redraw();
    };

    // button draws one action of wm.pv.taskActions / wm.wl.cardActions /
    // inputActions: a task action the API serves (a: {action, body,
    // confirm, tab}), a link to a form (href), or the disabled button.
    function button(a) {
      if (a.href) return wm.writeAction(a.label, a.command, a);
      if (!a.action || !task) return wm.disabledAction(a.label, a.command, a.refusal, a.danger);
      const o = Object.assign({}, a, { title: a.tab ? wm.pv.TAB_NOTE : a.command + " in the TUI" });
      if (a.action === "retry" || a.action === "edit") o.onclick = toggle(a.action);
      else o.onclick = (ev) => run(a.action, a.body || {}, ev && ev.target);
      return wm.writeAction(a.label, a.command, o);
    }

    // keep is the Keep sandbox toggle: save_sandbox.
    function keep() {
      const on = !!(task && task.capabilities && task.capabilities.save_sandbox);
      return button({ label: "Keep sandbox " + (on ? "☑" : "☐"), command: "edit save_sandbox in tasks/" + (task ? task.name : "<task>") + ".yaml", action: "save-sandbox", body: { save: !on }, refusal: "" });
    }

    // panel is the open confirm row or edit form, and the last outcome.
    function panel() {
      const out = [];
      if (open === "retry" && task) {
        const hint = wm.pv.retryHint(task, project || {}, session);
        out.push(h("div", { class: "wolf-confirm notice warn small" },
          "Retry ", h("code", {}, task.name), ": back to ready, failure_reason and blocked_reason cleared; ",
          hint.effect || hint.refusal, ". The worktree is reused as it is. ",
          h("label", {}, reset, " reset attempts (now " + (task.attempts || 0) + ")"), " ", retryNote, " ",
          h("button", { class: "action danger", type: "button", onclick: (ev) => run("retry", retryBody(), ev && ev.target) }, "Retry task"), " ",
          h("button", { class: "action", type: "button", onclick: toggle("retry") }, "Cancel")));
      }
      if (open === "edit" && task) {
        out.push(h("div", { class: "task-edit notice small" },
          h("div", { class: "muted" }, "Edit tasks/" + task.name + ".yaml: only these inputs. The server loads the task graph with the edit in place and refuses one that would break it (an unknown dependency, a cycle, a bad glob or tag)."),
          h("label", {}, "description", desc),
          h("label", {}, "depends_on", deps),
          h("label", {}, "touches", touches, h("span", { class: "muted" }, " "), noTouches, " no touches hint (exclusive unless it has areas)"),
          h("label", {}, "areas", areas),
          h("div", {},
            h("button", { class: "action primary", type: "button", onclick: (ev) => editSave(ev && ev.target) }, "Save"), " ",
            h("button", { class: "action", type: "button", onclick: toggle("edit") }, "Cancel"))));
      }
      out.push(note);
      return out;
    }

    function retryBody() {
      const b = { reset_attempts: !!reset.checked };
      const n = retryNote.value.trim();
      if (n) b.note = n;
      return b;
    }

    function editSave(btn) {
      const p = patch();
      if (!Object.keys(p).length) {
        note.className = "small muted action-note";
        note.textContent = "edit: nothing changed";
        open = "";
        redraw();
        return;
      }
      run("edit", p, btn);
    }

    return { set, button, keep, panel, patch, get open() { return open; } };
  };
})();
