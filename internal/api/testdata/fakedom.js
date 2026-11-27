// A minimal fake browser for the GUI's DOM tests (projects_dom_test.js,
// workload_dom_test.js): just enough DOM, location, history, fetch and
// EventSource to load the real bundle into a node vm, render a route and
// feed it stream events.
"use strict";

const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

// ---- a minimal DOM -------------------------------------------------------

class Node {
  constructor() {
    this.childNodes = [];
    this.parentNode = null;
  }
  append(...nodes) {
    for (const n of nodes) {
      // As in a browser, anything that is not a node becomes text, so a
      // stray null shows up as "null" (checked below).
      const c = n instanceof Node ? n : new Text(String(n));
      if (c.parentNode) c.remove();
      c.parentNode = this;
      this.childNodes.push(c);
    }
  }
  prepend(...nodes) {
    const keep = this.childNodes;
    this.childNodes = [];
    this.append(...nodes);
    this.childNodes.push(...keep);
  }
  replaceChildren(...nodes) {
    for (const c of this.childNodes) c.parentNode = null;
    this.childNodes = [];
    this.append(...nodes);
  }
  remove() {
    if (!this.parentNode) return;
    const p = this.parentNode;
    p.childNodes = p.childNodes.filter((c) => c !== this);
    this.parentNode = null;
  }
  get textContent() {
    return this.childNodes.map((c) => c.textContent).join("");
  }
  set textContent(v) {
    this.replaceChildren(new Text(String(v)));
  }
  get childElementCount() {
    return this.childNodes.filter((c) => c instanceof Element).length;
  }
  get lastElementChild() {
    return this.childNodes.filter((c) => c instanceof Element).pop() || null;
  }
}
class Text extends Node {
  constructor(t) {
    super();
    this.data = t;
  }
  get textContent() {
    return this.data;
  }
}
class Element extends Node {
  constructor(tag, ns) {
    super();
    this.tagName = tag.toUpperCase();
    this.namespaceURI = ns || "http://www.w3.org/1999/xhtml";
    this.attributes = {};
    this.listeners = {};
    this.style = {};
    this.dataset = {};
    this.hidden = false;
    this.value = "";
    this.checked = false;
    const self = this;
    this.classList = {
      add: (c) => self.setAttribute("class", [...new Set((self.className + " " + c).trim().split(/\s+/))].join(" ")),
      remove: (c) => self.setAttribute("class", self.className.split(/\s+/).filter((x) => x && x !== c).join(" ")),
      toggle: (c, on) => (on ? self.classList.add(c) : self.classList.remove(c)),
      contains: (c) => self.className.split(/\s+/).includes(c),
    };
  }
  get className() {
    return this.attributes.class || "";
  }
  set className(v) {
    this.attributes.class = v;
  }
  setAttribute(k, v) {
    this.attributes[k] = String(v);
    if (k === "id") this.id = String(v);
  }
  getAttribute(k) {
    return k in this.attributes ? this.attributes[k] : null;
  }
  removeAttribute(k) {
    delete this.attributes[k];
  }
  addEventListener(t, fn) {
    (this.listeners[t] = this.listeners[t] || []).push(fn);
  }
  dispatch(t, ev) {
    for (const fn of this.listeners[t] || []) fn(Object.assign({ preventDefault() {}, stopPropagation() {}, target: this }, ev));
  }
  focus() {}
  closest() {
    return null;
  }
  get disabled() {
    return "disabled" in this.attributes;
  }
  // all walks the subtree, depth first.
  *all() {
    for (const c of this.childNodes) {
      if (c instanceof Element) {
        yield c;
        yield* c.all();
      }
    }
  }
  find(pred) {
    for (const e of this.all()) if (pred(e)) return e;
    return null;
  }
  findAll(pred) {
    return [...this.all()].filter(pred);
  }
}

const byClass = (c) => (e) => e.className.split(/\s+/).includes(c);

// boot loads scripts (from webDir) into a fresh fake browser at url.
// routes maps an API path to a function returning its JSON body; raw(u),
// when set, may answer a request first with a whole response ({ok, status,
// headers, text}). It returns the page's main element, the requests made,
// the EventSource, settle() (let promises and queued timers run; skip
// lets a timer function through only when it returns false), navigate()
// and start() (fire DOMContentLoaded).
function boot({ webDir, scripts, url, routes, raw, skip }) {
  const body = new Element("body");
  const main = new Element("main");
  main.setAttribute("id", "main");
  const pill = new Element("span");
  pill.setAttribute("id", "status-pill");
  const badge = new Element("span");
  badge.setAttribute("id", "inbox-badge");
  const nav = new Element("nav");
  nav.setAttribute("id", "nav");
  body.append(nav, pill, badge, main);

  const docListeners = {};
  const document = {
    readyState: "loading",
    createElement: (t) => new Element(t),
    createElementNS: (ns, t) => new Element(t, ns),
    createTextNode: (t) => new Text(t),
    getElementById: (id) => body.find((e) => e.id === id),
    querySelectorAll: (sel) => (sel === "#nav a" ? nav.findAll((e) => e.tagName === "A") : []),
    addEventListener: (t, fn) => (docListeners[t] = docListeners[t] || []).push(fn),
  };

  const loc = new URL(url);
  const location = {
    get pathname() {
      return loc.pathname;
    },
    get search() {
      return loc.search;
    },
    get hash() {
      return loc.hash;
    },
    get href() {
      return loc.href;
    },
    origin: loc.origin,
  };
  const history = {
    replaceState: (_, __, p) => (loc.href = new URL(p, loc.href).href),
    pushState: (_, __, p) => (loc.href = new URL(p, loc.href).href),
  };

  const requests = [];
  async function fetch(u, init) {
    const parsed = new URL(u, loc.href);
    requests.push({
      method: (init && init.method) || "GET",
      path: parsed.pathname + parsed.search,
      headers: (init && init.headers) || {},
      body: init && init.body !== undefined ? JSON.parse(init.body) : undefined,
    });
    const r0 = raw && raw(parsed);
    if (r0) return r0;
    const r = routes[parsed.pathname];
    if (!r) return { ok: false, status: 404, headers: new Map(), text: async () => '{"error":{"code":"not_found","message":"no"}}' };
    let v;
    try {
      v = r(parsed, init || {});
    } catch (e) {
      // A thrown error's current (a 412's or 409's resource) goes in the
      // body, as the server sends it.
      return { ok: false, status: e.status || 500, headers: new Map(), text: async () => JSON.stringify({ error: { code: e.code || "internal", message: e.message, current: e.current } }) };
    }
    return { ok: true, status: 200, headers: new Map(), text: async () => JSON.stringify(v) };
  }

  const es = { source: null };
  class EventSource {
    constructor(u) {
      this.url = u;
      this.readyState = 1;
      this.listeners = {};
      es.source = this;
    }
    addEventListener(t, fn) {
      (this.listeners[t] = this.listeners[t] || []).push(fn);
    }
    close() {
      this.readyState = 2;
    }
    send(ev) {
      for (const fn of this.listeners[ev.type] || []) fn({ data: JSON.stringify(ev) });
    }
  }
  EventSource.CLOSED = 2;

  const timers = [];
  const popListeners = [];
  const ctx = vm.createContext({
    document, location, history, fetch, EventSource, URL, URLSearchParams, Node, console, Date, Math, JSON, Promise, Map, Set,
    Uint8Array, Array, crypto: { getRandomValues: (b) => require("node:crypto").getRandomValues(b) },
    setTimeout: (fn) => (timers.push(fn), timers.length),
    clearTimeout: () => {},
    setInterval: () => 0,
    clearInterval: () => {},
    isFinite, isNaN, Number, String,
    addEventListener: (t, fn) => t === "popstate" && popListeners.push(fn),
  });
  ctx.window = ctx;
  for (const f of scripts) {
    vm.runInContext(fs.readFileSync(path.join(webDir, f), "utf8"), ctx, { filename: f });
  }

  const tick = () => new Promise((r) => setImmediate(r));
  async function settle() {
    for (let i = 0; i < 20; i++) {
      await tick();
      const due = timers.splice(0);
      for (const fn of due) if (!(skip && skip(fn))) fn();
    }
  }
  return {
    main,
    requests,
    es,
    settle,
    get location() {
      return loc;
    },
    start() {
      document.readyState = "interactive";
      for (const fn of docListeners.DOMContentLoaded || []) fn();
    },
    // navigate goes to p as a click on an in-app link would.
    async navigate(p) {
      ctx.wm.navigate(p);
      await settle();
    },
  };
}

// SCRIPTS is the bundle in index.html's order.
const SCRIPTS = ["app.js", "graph.js", "projects.js", "workload.js", "sandboxes.js", "agents.js", "transcript.js", "watch.js", "secondary.js", "timeline.js", "pulls.js", "intake.js", "inbox.js"];

// buttons lists el's buttons that stand for a write action (every one but
// the local ones, such as Save transcript, which only save in the
// browser); label is a button's own label, without the command line under
// it. Enabled write actions that open a form are links (class action-link).
const buttons = (el) => el.findAll((e) => e.tagName === "BUTTON" && e.getAttribute("data-local") === null);
const localButtons = (el) => el.findAll((e) => e.tagName === "BUTTON" && e.getAttribute("data-local") !== null);
const label = (b) => b.childNodes[0].textContent;

module.exports = { Node, Text, Element, byClass, boot, buttons, localButtons, label, SCRIPTS };
