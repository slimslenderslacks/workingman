// workingman GUI: the app shell (docs/proposals/workingman-gui.md §G.0, P.3).
//
// A deliberately small, dependency-free script: no build step, no framework,
// nothing loaded from another origin. It provides what the screens build on:
//
//   - wm.api     the API client (cookie session, CSRF header on non-GET,
//                Idempotency-Key and If-Match on writes)
//   - wm.stream  one SSE subscription to /api/v1/events, with resume
//   - wm.route   a history-API router; screens register with wm.screen()
//   - wm.h       a DOM builder that never parses strings as HTML
//
// Writes (P.4) are enabled only for a session with the write scope; without
// it every write control stays disabled, labelled with its TUI command.
"use strict";

(function () {
  const PREFIX = "/api/v1";
  const wm = (window.wm = {});

  // ---- DOM ---------------------------------------------------------------

  // h(tag, attrs, ...children) builds an element. Strings become text
  // nodes, so data from the API is never interpreted as markup.
  function h(tag, attrs, ...children) {
    const el = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v === null || v === undefined || v === false) continue;
      if (k === "class") el.className = v;
      else if (k.startsWith("on") && typeof v === "function") el.addEventListener(k.slice(2), v);
      else el.setAttribute(k, v === true ? "" : String(v));
    }
    for (const c of children.flat(Infinity)) {
      if (c === null || c === undefined || c === false) continue;
      el.append(c instanceof Node ? c : document.createTextNode(String(c)));
    }
    return el;
  }
  wm.h = h;

  // link is an in-app link the router handles without a page load.
  wm.link = (href, ...children) => h("a", { href, "data-link": true }, ...children);

  wm.status = (s) => h("span", { class: "status status-" + (s || "unknown") }, s || "—");

  // disabledAction is a write action the read-only GUI shows but cannot do
  // yet, labelled with the TUI command that does it today (P.3). refusal,
  // when set, is the TUI's own text for why that command would refuse right
  // now; danger marks an action that interrupts work (the wolf).
  wm.disabledAction = (label, tuiCommand, refusal, danger) =>
    h(
      "button",
      {
        class: "action" + (refusal ? " refused" : "") + (danger ? " danger" : ""),
        disabled: true,
        title: (refusal ? refusal + "\n" : "") + "read-only for now; in the TUI: " + tuiCommand,
      },
      label,
      h("span", { class: "cmd" }, tuiCommand)
    );

  // ---- API client --------------------------------------------------------

  class APIError extends Error {
    constructor(status, code, message) {
      super(message || "HTTP " + status);
      this.status = status;
      this.code = code;
    }
  }
  wm.APIError = APIError;

  let csrf = "";

  async function request(method, path, body, raw, extra) {
    const headers = Object.assign({ Accept: "application/json" }, extra || {});
    const init = { method, headers, credentials: "same-origin", cache: "no-cache" };
    if (method !== "GET" && method !== "HEAD") {
      if (csrf) headers["X-WM-CSRF"] = csrf;
      if (body !== undefined) {
        headers["Content-Type"] = "application/json";
        init.body = JSON.stringify(body);
      }
    }
    if (raw) headers.Accept = "*/*";
    const resp = await fetch(PREFIX + path, init);
    if (resp.status === 204) return null;
    let data = null;
    const text = await resp.text();
    if (raw && resp.ok) return { text, headers: resp.headers };
    if (text) {
      try {
        data = JSON.parse(text);
      } catch (e) {
        data = null;
      }
    }
    if (!resp.ok) {
      const err = (data && data.error) || {};
      if (resp.status === 401 && path !== "/session") onLoggedOut();
      const e = new APIError(resp.status, err.code, err.message);
      // A 412 or 409 may carry the resource as it is now (§A.4.1).
      if (err.current) e.current = err.current;
      throw e;
    }
    return data;
  }

  // idemKey is a fresh Idempotency-Key: a write retried with the same key
  // (a double click, a resend after a dropped response) is done once.
  function idemKey() {
    const b = new Uint8Array(16);
    crypto.getRandomValues(b);
    return Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
  }

  // write sends a write action. opts: {ifMatch, key}; a caller that may
  // resend the same write passes the same key.
  function write(method, path, body, opts) {
    const o = opts || {};
    const extra = { "Idempotency-Key": o.key || idemKey() };
    if (o.ifMatch) extra["If-Match"] = o.ifMatch;
    return request(method, path, body, false, extra);
  }

  wm.api = {
    get: (path) => request("GET", path),
    // list fetches a list endpoint and returns its envelope
    // {items, generated_at, cursor}.
    list: (path) => request("GET", path),
    // text fetches a non-JSON body (a transcript's NDJSON) and returns
    // {text, headers}.
    text: (path) => request("GET", path, undefined, true),
    session: () => request("GET", "/session"),
    login: (code) => request("POST", "/session", { code }),
    logout: () => request("DELETE", "/session"),
    post: (path, body, opts) => write("POST", path, body === undefined ? {} : body, opts),
    del: (path, opts) => write("DELETE", path, undefined, opts),
    idemKey,
  };

  // can reports whether the session holds a scope (§A.6.2): "write" for the
  // project and task actions, intake and new projects; "answer" for the
  // inbox.
  wm.can = (scope) => !!(wm.session && (wm.session.scopes || []).includes(scope));

  // writeAction is an enabled write control: a button that runs fn, or a
  // link to the form that does the write when href is set. Without the
  // write scope it is the disabled button, with its TUI command.
  wm.writeAction = (label, tuiCommand, opts) => {
    const o = opts || {};
    if (!wm.can("write") || o.refusal) return wm.disabledAction(label, tuiCommand, o.refusal, o.danger);
    if (o.href) return h("a", { href: o.href, "data-link": true, class: "action action-link" + (o.danger ? " danger" : "") }, label);
    return h("button", { class: "action" + (o.danger ? " danger" : ""), type: "button", title: o.title || "", onclick: o.onclick }, label);
  };

  // projectAction runs a project action (POST /projects/{id}/actions/{a},
  // §A.4.2) with the project's etag as If-Match, so a write that landed
  // since the GUI loaded the project is a 412, not a lost update. body is
  // {} except for wolf's {reason}. It resolves to the response; a refusal
  // rejects with the APIError, whose current (412, 409) is the project as
  // it is now.
  wm.projectAction = (project, action, body) =>
    wm.api.post("/projects/" + encodeURIComponent(project.id) + "/actions/" + action, body || {}, { ifMatch: project.etag });

  // actionError is a refused write's status-line text.
  wm.actionError = (e) => {
    if (e && e.status === 412) return "the project changed since it was loaded (" + e.message + "); it is reloaded: check it and try again";
    if (e instanceof APIError) return e.status + " " + (e.code || "") + ": " + e.message;
    return String((e && e.message) || e);
  };

  // ---- event stream ------------------------------------------------------

  // One EventSource drives every screen (§G.0 principle 5). Listeners get
  // each event's parsed envelope {seq, time, type, project, cause, data}.
  // The browser reconnects by itself with Last-Event-ID; when the server
  // refuses (401, 503) the source closes and we retry with backoff.
  const listeners = new Map(); // type ("*" for all) → Set<fn>
  const STREAM_TYPES = [
    "hello", "heartbeat", "reset",
    "project.created", "project.updated", "project.status", "project.removed", "project.live",
    "task.created", "task.updated", "task.status", "task.removed",
    "queue.updated",
    "session.started", "session.updated", "session.ended",
    "intake.created", "intake.updated", "intake.processed", "intake.removed",
    "pr.updated", "message.created", "message.closed",
    "audit",
  ];
  // The server's default subscription leaves audit out; the timeline
  // (§G.5) needs it, and one stream serves every screen.
  const STREAM_URL = PREFIX + "/events?types=" + encodeURIComponent("project.*,task.*,queue.*,session.*,intake.*,pr.*,message.*,audit");

  const stream = {
    source: null,
    state: "off", // off | connecting | live | retrying
    cursor: 0,
    epoch: "",
    retry: 1000,
    timer: 0,
    on(type, fn) {
      if (!listeners.has(type)) listeners.set(type, new Set());
      listeners.get(type).add(fn);
      return () => listeners.get(type).delete(fn);
    },
    emit(ev) {
      for (const key of [ev.type, "*"]) {
        for (const fn of listeners.get(key) || []) {
          try {
            fn(ev);
          } catch (e) {
            console.error("wm.stream listener", e);
          }
        }
      }
    },
    start() {
      this.stop();
      this.setState("connecting");
      const src = new EventSource(STREAM_URL);
      this.source = src;
      const handle = (msg) => {
        let ev;
        try {
          ev = JSON.parse(msg.data);
        } catch (e) {
          return;
        }
        if (ev.seq > this.cursor) this.cursor = ev.seq;
        if (ev.type === "hello") {
          this.epoch = ev.data.epoch;
          this.retry = 1000;
          this.setState("live");
        }
        this.emit(ev);
      };
      for (const t of STREAM_TYPES) src.addEventListener(t, handle);
      src.onerror = () => {
        if (src.readyState === EventSource.CLOSED) {
          this.source = null;
          this.setState("retrying");
          // A closed source may mean the session ended: ask.
          wm.api.session().then(
            () => this.schedule(),
            (e) => (e.status === 401 ? onLoggedOut() : this.schedule())
          );
        } else {
          this.setState("connecting");
        }
      };
    },
    schedule() {
      clearTimeout(this.timer);
      this.timer = setTimeout(() => this.start(), this.retry);
      this.retry = Math.min(this.retry * 2, 30000);
    },
    stop() {
      clearTimeout(this.timer);
      if (this.source) this.source.close();
      this.source = null;
      this.setState("off");
    },
    setState(s) {
      this.state = s;
      renderPill();
    },
  };
  wm.stream = stream;

  function renderPill() {
    const pill = document.getElementById("status-pill");
    if (!pill) return;
    const label = {
      off: "offline",
      connecting: "connecting…",
      live: "live · seq " + stream.cursor,
      retrying: "reconnecting…",
    }[stream.state];
    pill.textContent = label;
    pill.className = "pill " + (stream.state === "live" ? "pill-ok" : stream.state === "off" ? "pill-off" : "pill-warn");
  }
  stream.on("*", () => {
    if (stream.state === "live") renderPill();
  });

  // ---- router ------------------------------------------------------------

  // Screens register a path pattern ("/projects/:id") and a render function
  // (params, container) that may return a cleanup function, called when the
  // user navigates away (for example to drop stream listeners).
  const screens = [];
  let cleanup = null;

  wm.screen = (pattern, render) => {
    const names = [];
    const re = new RegExp(
      "^" +
        pattern.replace(/\/:([a-z_]+)/g, (_, n) => {
          names.push(n);
          return "/([^/]+)";
        }) +
        "/?$"
    );
    screens.push({ pattern, re, names, render });
  };

  function navigate(path, replace) {
    if (replace) history.replaceState(null, "", path);
    else history.pushState(null, "", path);
    route();
  }
  wm.navigate = navigate;

  async function route() {
    if (cleanup) {
      try {
        cleanup();
      } catch (e) {
        console.error(e);
      }
      cleanup = null;
    }
    const path = location.pathname;
    const main = document.getElementById("main");
    main.replaceChildren();
    for (const a of document.querySelectorAll("#nav a")) {
      a.classList.toggle("active", path === "/" + a.dataset.route || path.startsWith("/" + a.dataset.route + "/"));
    }
    if (path !== "/login" && !wm.session) {
      main.append(h("p", { class: "notice" }, "Not logged in. Run ", h("code", {}, "orch gui"), " to open a logged-in window."));
      return;
    }
    for (const s of screens) {
      const m = s.re.exec(path);
      if (!m) continue;
      const params = {};
      s.names.forEach((n, i) => (params[n] = decodeURIComponent(m[i + 1])));
      try {
        const c = await s.render(params, main);
        if (typeof c === "function") cleanup = c;
      } catch (e) {
        main.replaceChildren(errorNotice(e));
      }
      main.focus();
      return;
    }
    main.append(h("p", { class: "notice" }, "No such page: ", h("code", {}, path)));
  }

  function errorNotice(e) {
    return h("p", { class: "notice error" }, e instanceof APIError ? `${e.status} ${e.code || ""}: ${e.message}` : String(e));
  }
  wm.errorNotice = errorNotice;

  document.addEventListener("click", (e) => {
    const a = e.target.closest("a");
    if (!a || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const url = new URL(a.href, location.href);
    if (url.origin !== location.origin || url.pathname.startsWith("/api/")) return;
    e.preventDefault();
    navigate(url.pathname + url.search);
  });
  window.addEventListener("popstate", route);

  // ---- session -----------------------------------------------------------

  wm.session = null;

  function onLoggedOut() {
    wm.session = null;
    csrf = "";
    stream.stop();
    if (location.pathname !== "/login") route();
  }

  function setSession(s) {
    wm.session = s;
    csrf = s.csrf || "";
  }

  // /login#t=<code>: the code is in the fragment, which never reaches the
  // server or its logs. Spend it, drop it from the address bar and history,
  // and go to the page the login asked for (?next=, same-origin paths only).
  wm.screen("/login", async (_, main) => {
    const code = new URLSearchParams(location.hash.slice(1)).get("t");
    const next = new URLSearchParams(location.search).get("next");
    const dest = next && next.startsWith("/") && !next.startsWith("//") ? next : "/";
    history.replaceState(null, "", "/login");
    if (!code) {
      if (wm.session) return navigate(dest, true);
      main.append(h("p", { class: "notice" }, "Run ", h("code", {}, "orch gui"), " to log in. Login links work once and expire after a minute."));
      return;
    }
    main.append(h("p", { class: "muted" }, "Logging in…"));
    try {
      setSession(await wm.api.login(code));
    } catch (e) {
      main.replaceChildren(
        h("p", { class: "notice error" }, "Login failed: ", e.message || String(e), ". Run ", h("code", {}, "orch gui"), " again.")
      );
      return;
    }
    stream.start();
    refreshInbox();
    navigate(dest, true);
  });

  // ---- placeholder screens -----------------------------------------------
  // Minimal list screens over the read endpoints. The P.3 screens (§G.1–G.5)
  // are elsewhere: the Projects screens (§G.1) in projects.js, the Sandboxes
  // list and sandbox card (§G.2, §G.3) in sandboxes.js, the agents map
  // (§G.4) in agents.js, the transcript view and session watch (§G.2) in
  // watch.js, the timeline and pull requests (§G.5) in timeline.js and
  // pulls.js, the inbox (§G.5) in inbox.js.

  // liveList renders a list endpoint as a table and refetches it when one
  // of the given event types arrives (debounced).
  function liveList(main, title, path, types, columns) {
    const body = h("tbody");
    const meta = h("p", { class: "muted" });
    main.append(
      h("h1", {}, title),
      h("table", {}, h("thead", {}, h("tr", {}, columns.map((c) => h("th", {}, c[0])))), body),
      meta
    );
    let timer = 0;
    const load = async () => {
      try {
        const list = await wm.api.list(path);
        body.replaceChildren(
          ...(list.items.length
            ? list.items.map((it) => h("tr", {}, columns.map((c) => h("td", {}, c[1](it)))))
            : [h("tr", {}, h("td", { colspan: columns.length, class: "muted" }, "none"))])
        );
        meta.textContent = "cursor " + list.cursor;
      } catch (e) {
        body.replaceChildren(h("tr", {}, h("td", { colspan: columns.length }, errorNotice(e))));
      }
    };
    const offs = types.concat(["reset"]).map((t) =>
      stream.on(t, () => {
        clearTimeout(timer);
        timer = setTimeout(load, 150);
      })
    );
    load();
    return () => {
      clearTimeout(timer);
      offs.forEach((off) => off());
    };
  }
  wm.liveList = liveList;

  // Inbox badge: open operator requests.
  async function refreshInbox() {
    const badge = document.getElementById("inbox-badge");
    try {
      const list = await wm.api.list("/messages");
      const open = list.items.filter((m) => m.state === "open").length;
      badge.textContent = String(open);
      badge.hidden = open === 0;
    } catch (e) {
      badge.hidden = true;
    }
  }
  let inboxTimer = 0;
  for (const t of ["message.created", "message.closed", "reset"]) {
    stream.on(t, () => {
      clearTimeout(inboxTimer);
      inboxTimer = setTimeout(refreshInbox, 150);
    });
  }

  // ---- boot --------------------------------------------------------------

  async function boot() {
    if (location.pathname !== "/login") {
      try {
        setSession(await wm.api.session());
      } catch (e) {
        wm.session = null;
      }
      if (wm.session) {
        stream.start();
        refreshInbox();
      }
    }
    route();
  }

  // The screens' scripts (projects.js, ...) are deferred after this one, and
  // register on wm before DOMContentLoaded: boot only then.
  if (document.readyState === "complete") boot();
  else document.addEventListener("DOMContentLoaded", boot);
})();
