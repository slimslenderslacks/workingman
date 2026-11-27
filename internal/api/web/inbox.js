// workingman GUI: the inbox (docs/proposals/workingman-gui.md §G.5, §A.4.5),
// the operator's post-office messages, open first. A request is answered
// with one of its options or a free-text reply (POST /messages/{id}/answer);
// a notification is dismissed (POST /messages/{id}/dismiss). Both are a
// post-office claim with responder via "gui", the same Store.Claim the TUI
// inbox makes, and need the answer scope. The first answer wins: when the
// TUI, a phone or another tab answered first, the server says so (409 with
// the winning resolution) and the row shows it.
//
// A step-up request's body is the exact CHANGE line the executor will
// apply, so it is drawn verbatim in a monospace block, and there is no
// "approve all": each request is answered on its own. The model is
// secondary.js (wm.sv.inboxItem).
"use strict";

(function () {
  const wm = window.wm;
  const { h, sv } = wm;
  const enc = encodeURIComponent;

  wm.screen("/inbox", (_, main) => {
    const list = h("div", { class: "inbox" }, h("p", { class: "muted" }, "Loading…"));
    const meta = h("p", { class: "muted small" });
    const show = h("select", { "aria-label": "show" }, ["open", "all"].map((v) => h("option", { value: v }, v === "open" ? "open messages" : "open and closed")));
    main.append(
      h("div", { class: "page-head" }, h("h1", {}, "Inbox"), h("span", { class: "muted small" }, "GET /api/v1/messages · message.*")),
      h("div", { class: "filters" }, show),
      list,
      meta
    );
    // notes keeps each message's last outcome across reloads; replies, the
    // text being typed.
    const notes = new Map();
    const replies = new Map();
    let items = [];

    const answerable = () => wm.can("answer");

    function card(m) {
      const it = sv.inboxItem(m, Date.now());
      const note = notes.get(it.id);
      const out = h("section", { class: "inbox-item" + (it.open ? "" : " closed") + (it.stepUp ? " step-up" : "") },
        h("div", { class: "inbox-head" },
          h("strong", {}, it.subject), " ",
          h("span", { class: "muted small" }, it.kind, " from ", it.sandbox ? wm.link("/sandboxes/" + enc(it.sandbox), it.from) : it.from),
          it.open && it.expires ? h("span", { class: "muted small" }, " · expires ", it.expires) : null,
          !it.open ? h("span", { class: "small inbox-closed" }, " · ", it.refusal.replace(/^closed: /, "")) : null
        ),
        it.stepUp ? h("p", { class: "small warn" }, "Step-up request: approving lets Step-Up apply exactly the CHANGE line below, and nothing else.") : null,
        h("pre", { class: "inbox-body" }, it.body)
      );
      if (it.open) out.append(controls(it));
      if (note) out.append(h("p", { class: "small " + note.cls }, note.text));
      return out;
    }

    function controls(it) {
      const row = h("div", { class: "actions" });
      const cmd = it.canReply ? "the TUI inbox (I): 1-9 picks an option, enter replies" : "the TUI inbox (I): d dismisses";
      if (!answerable()) {
        if (it.canReply) for (const o of it.options) row.append(wm.disabledAction(o, cmd, "this session lacks the answer scope"));
        row.append(wm.disabledAction(it.canReply ? "Reply" : "Dismiss", cmd, "this session lacks the answer scope"));
        return row;
      }
      const button = (label, cls, fn) => h("button", { class: "action" + (cls ? " " + cls : ""), type: "button", title: "in the TUI: " + cmd, onclick: (ev) => fn(ev && ev.target) }, label);
      if (it.canDismiss) {
        row.append(button("Dismiss", "", (b) => send(it, "/dismiss", undefined, b)));
        return row;
      }
      for (const o of it.options) {
        row.append(button(o, /^(deny|no|reject)/i.test(o) ? "danger" : "", (b) => send(it, "/answer", { option: o }, b)));
      }
      const text = h("input", { type: "text", "aria-label": "reply to " + it.id, placeholder: it.options.length ? "or reply in your own words" : "reply", value: replies.get(it.id) || "" });
      text.addEventListener("input", () => replies.set(it.id, text.value));
      row.append(text, button("Reply", "", (b) => {
        const v = text.value.trim();
        if (!v) {
          notes.set(it.id, { cls: "error", text: "type a reply first" });
          draw();
          return;
        }
        send(it, "/answer", { response: v }, b);
      }));
      return row;
    }

    // send claims the message. Each click is its own Idempotency-Key: a
    // second claim of the same message is refused by post-office anyway.
    async function send(it, path, body, b) {
      if (b) b.setAttribute("disabled", "");
      try {
        const res = await wm.api.post("/messages/" + enc(it.id) + path, body, {});
        replies.delete(it.id);
        notes.set(it.id, { cls: "ok", text: sv.closedText(res.message && res.message.resolution) + " · post-office delivers it" });
        replace(res.message);
      } catch (e) {
        if (e.status === 409 && e.current) {
          notes.set(it.id, { cls: "warn", text: "already closed elsewhere: " + sv.closedText(e.current.resolution) });
          replace(e.current);
        } else {
          notes.set(it.id, { cls: "error", text: wm.actionError(e) });
          draw();
        }
      }
    }

    function replace(m) {
      if (!m || !m.id) return draw();
      const i = items.findIndex((x) => x.id === m.id);
      if (i >= 0) items[i] = m;
      draw();
      later();
    }

    function draw() {
      const shown = sv.inboxOrder(items).filter((m) => show.value === "all" || m.state === "open" || notes.has(m.id));
      list.replaceChildren(...(shown.length ? shown.map(card) : [h("p", { class: "notice" }, show.value === "all" ? "No messages." : "Nothing is waiting for you.")]));
      meta.textContent = answerable() ? "Answers are post-office claims via gui; the first answer from any channel wins." : "Read-only: this session lacks the answer scope.";
    }

    async function load() {
      try {
        const l = await wm.api.list("/messages" + (show.value === "all" ? "?state=all" : ""));
        items = l.items;
        draw();
      } catch (e) {
        list.replaceChildren(wm.errorNotice(e));
      }
    }
    let timer = 0;
    function later() {
      clearTimeout(timer);
      timer = setTimeout(load, 150);
    }
    show.addEventListener("change", load);
    const offs = ["message.created", "message.closed", "reset"].map((t) => wm.stream.on(t, later));
    load();
    return () => {
      clearTimeout(timer);
      offs.forEach((off) => off());
    };
  });
})();
