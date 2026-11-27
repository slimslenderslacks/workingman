// workingman GUI: the transcript view (docs/proposals/workingman-gui.md
// §G.2, P.3, P.5) and the session watch page. wm.transcriptView is the one
// transcript panel: the project view's selected-task strip and the sandbox
// card embed it as a preview, and /sessions/{sid} shows it whole.
//
// A live session is read over the session relay (P.5): a WebSocket on
// GET /api/v1/sessions/{sid}/acp, subprotocol workingman.acp.v1, which
// replays stream.log and then streams its frames as they are written. The
// relay keeps lifetime parity with the TUI (§A.5.2), so watching a one-shot
// session does not change when it ends. Where there is no live session to
// attach to, or the login lacks the attach scope, or the upgrade is
// refused, the view polls GET /api/v1/sessions/{sid}/transcript (NDJSON,
// paged by byte offset) as in P.3. Both read the same byte offsets of
// stream.log, so a view can move from one to the other without a gap.
//
// Prompt (§G.2): on the watch page with ?prompt=1, for a persistent session
// a human may type into (wolf, workingman, sleepy-head) and a login with the
// interact scope, the view attaches with mode=interactive and shows a
// composer: {"t":"prompt"} and {"t":"cancel"} are all it sends. Everything
// else is observe only.
//
// A transcript is lost when its session ends, because the wrapper deletes
// the session directory (survey §8.2). The view then keeps what it had
// loaded, labels it lost, and still offers "Save transcript", which saves
// that copy from the browser (R.4 Q4).
"use strict";

(function () {
  const wm = window.wm;
  const { h } = wm;
  const enc = encodeURIComponent;

  // POLL_MS is how often a polled transcript is re-read.
  const POLL_MS = 2000;
  // PREVIEW_BYTES is how far before the end of stream.log a preview starts.
  const PREVIEW_BYTES = 200000;
  // PAGE is the frames asked for per request.
  const PAGE = 2000;
  // SUBPROTOCOL is the relay's WebSocket subprotocol (api.SubprotocolACP).
  const SUBPROTOCOL = "workingman.acp.v1";
  // RETRY_MS and RETRY_MAX_MS bound the pause before re-attaching to the
  // relay after a connection that had started dropped.
  const RETRY_MS = 1000;
  const RETRY_MAX_MS = 15000;

  // relayable reports whether a session can be read over the relay from
  // this page: a live ACP session, a login with the attach scope, and a
  // browser with WebSockets.
  const relayable = (s) => !!(s && s.acp && s.acp.attachable && wm.can("attach") && typeof WebSocket !== "undefined");

  // transcriptView makes a transcript panel for session sid. opts:
  //   session  the session resource (kind, persistent, prompt_count, acp)
  //   mode     "preview" (the tail, as one-line entries) or "full"
  //   link     in preview mode, show "Full transcript ▸"
  //   prompt   attach interactively and show the composer, when the
  //            session takes prompts and the login has the interact scope
  // It returns {el, sid, via, setSession, end, stop}: end() marks the
  // session ended (the view reads once more, then labels the transcript
  // lost); stop() stops reading and listening, for a screen's cleanup.
  wm.transcriptView = (opts) => {
    const tx = wm.tx;
    const full = opts.mode === "full";
    const tr = {
      sid: opts.sid,
      session: opts.session || null,
      frames: [],
      dropped: 0,
      next: -1,
      timer: 0,
      state: "live", // live | lost | gone
      ending: false,
      stopped: false,
      // via is "relay" while the view reads the session relay, else "poll".
      via: relayable(opts.session) ? "relay" : "poll",
      interactive: !!opts.prompt && tx.promptable(opts.session) && wm.can("interact"),
      ws: null,
      hello: null, // the relay's hello, once this connection has one
      offset: -1, // the stream.log offset after the last relayed frame
      turn: null,
      retry: RETRY_MS,
      note: "",
    };
    const badge = h("span", { class: "t-state" });
    const turnChip = h("span", { class: "t-turn muted small" });
    const lines = h("div", { class: full ? "t-entries" : "t-lines" }, h("div", { class: "t-line muted" }, "loading…"));
    const detail = h("span", { class: "muted small" });
    const meta = h("span", { class: "muted small" });
    const save = h("button", { class: "action local", "data-local": "true", title: "save what this page has loaded as a text file; nothing is sent to the daemon", onclick: () => saveTranscript() }, "Save transcript");
    const composer = h("div", { class: "t-compose" });
    const title = h("span", {});
    const el = h("div", { class: "transcript" + (full ? " transcript-full" : "") },
      h("div", { class: "t-head" }, title, " ", turnChip, " ", badge),
      lines,
      composer,
      h("div", { class: "t-foot" }, detail, " ", meta, " ", save,
        !full && opts.link !== false ? [" ", wm.link("/sessions/" + enc(tr.sid), "Full transcript ▸")] : null)
    );

    // ---- the composer (interactive only) ----
    const input = h("textarea", { rows: 3, "aria-label": "prompt", placeholder: "a message into the conversation" });
    const sendBtn = h("button", { class: "action primary", type: "button", onclick: () => sendPrompt() }, "Send");
    const cancelBtn = h("button", { class: "action danger", type: "button", title: "session/cancel for the turn in flight", onclick: () => sendCancel() }, "Cancel turn");
    const composeNote = h("span", { class: "small muted" });

    const live = () => tr.via === "relay" && tr.ws && tr.hello;
    const canType = () => live() && tr.interactive && tr.hello.mode === "interactive" && tr.state === "live";
    const running = () => !!(tr.turn && tr.turn.state === "running");

    function drawComposer() {
      if (!tr.interactive || tr.state !== "live") {
        composer.replaceChildren();
        return;
      }
      if (canType() && !running()) sendBtn.removeAttribute("disabled");
      else sendBtn.setAttribute("disabled", "");
      if (canType() && running()) cancelBtn.removeAttribute("disabled");
      else cancelBtn.setAttribute("disabled", "");
      composeNote.textContent = tr.note || (!live() ? "connecting to the session…" : running() ? "a turn is running: wait for it to end, or cancel it" : "prompts go into the live conversation; the text is not logged");
      composer.replaceChildren(input, h("div", { class: "t-compose-row" }, sendBtn, " ", cancelBtn, " ", composeNote));
    }

    function sendPrompt() {
      const text = input.value;
      if (!canType() || running() || !text.trim()) return;
      tr.ws.send(JSON.stringify({ t: "prompt", text }));
      // stream.log holds only the agent's frames: keep what was sent here.
      tr.frames.push({ wm_local: "prompt", text });
      input.value = "";
      tr.note = "";
      tr.turn = { state: "running" };
      draw();
    }

    function sendCancel() {
      if (!canType()) return;
      tr.ws.send(JSON.stringify({ t: "cancel" }));
      tr.note = "cancel sent";
      drawComposer();
    }

    const draw = () => {
      const lb = tx.label(tr.state, tr.session, { via: tr.via, interactive: !!(live() && tr.hello.mode === "interactive") });
      badge.className = "t-state t-" + lb.cls;
      badge.textContent = lb.text;
      title.textContent = full ? "transcript" : tr.via === "relay" ? "transcript (live over the session relay)" : "transcript (observe, replayed from stream.log)";
      turnChip.textContent = tr.via === "relay" && tr.state === "live" ? tx.turnText(tr.turn) : "";
      detail.textContent = lb.detail;
      meta.textContent = tr.frames.length ? tr.frames.length + " frames" + (tr.dropped ? " (" + tr.dropped + " older dropped)" : "") + (!full && tr.startedAt > 0 ? " · from byte " + tr.startedAt : "") : "";
      drawComposer();
      if (!tr.frames.length) {
        lines.replaceChildren(h("div", { class: "t-line muted" }, tr.state === "live" ? "waiting for output…" : "nothing was loaded before the session ended"));
        return;
      }
      if (full) {
        lines.replaceChildren(...tx.entries(tr.frames, tr.session).map(entryEl));
        return;
      }
      const ls = wm.pv.transcriptLines(tr.frames, 12, 90);
      lines.replaceChildren(...(ls.length ? ls.map((l) => h("div", { class: "t-line t-" + l.kind }, l.text)) : [h("div", { class: "t-line muted" }, "waiting for output…")]));
    };

    const keep = (got) => {
      tr.frames.push(...got);
      if (tr.frames.length > tx.MAX_FRAMES) {
        tr.dropped += tr.frames.length - tx.MAX_FRAMES;
        tr.frames.splice(0, tr.frames.length - tx.MAX_FRAMES);
      }
    };

    // startAt sets where a preview starts reading, once: near the end of
    // stream.log, so it shows only the tail. A first line cut in half is
    // not JSON and is skipped. A full view starts at 0.
    async function startAt() {
      if (tr.next >= 0) return;
      tr.next = 0;
      if (!full) {
        const head = await wm.api.text("/sessions/" + enc(tr.sid) + "/transcript?limit=1");
        const size = Number(head.headers.get("X-Transcript-Size")) || 0;
        tr.next = Math.max(0, size - PREVIEW_BYTES);
      }
      tr.startedAt = tr.next;
    }

    // ---- the relay (P.5) ----

    // attach opens the relay's WebSocket: from where the view started on
    // its first connection, from the last frame it has on a reconnect.
    async function attach() {
      if (tr.stopped) return;
      try {
        await startAt();
      } catch (e) {
        if (tr.stopped) return;
        tr.next = 0;
        tr.startedAt = 0;
      }
      if (tr.stopped) return;
      const from = tr.offset >= 0 ? tr.offset : tr.next;
      const q = "mode=" + (tr.interactive ? "interactive" : "observe") + "&replay=" + (from > 0 ? "after:" + from : "all");
      let ws;
      try {
        ws = new WebSocket(wm.wsURL("/sessions/" + enc(tr.sid) + "/acp", q), [SUBPROTOCOL]);
      } catch (e) {
        fallBack();
        return;
      }
      tr.ws = ws;
      tr.hello = null;
      ws.onmessage = (ev) => {
        if (tr.ws !== ws) return;
        let m;
        try {
          m = JSON.parse(ev.data);
        } catch (e) {
          return;
        }
        onMessage(m);
      };
      ws.onclose = () => {
        if (tr.ws === ws) onClose();
      };
    }

    function onMessage(m) {
      switch (m.t) {
        case "hello":
          tr.hello = m;
          tr.retry = RETRY_MS;
          if (m.replay === "reset") {
            // The offset lay past the end of the log: another log, replayed whole.
            tr.frames = [];
            tr.dropped = 0;
          }
          if (tr.session && m.prompt_count > (tr.session.prompt_count || 0)) tr.session = Object.assign({}, tr.session, { prompt_count: m.prompt_count });
          draw();
          return;
        case "replay":
        case "frame":
          if (typeof m.offset === "number") tr.offset = m.offset;
          keep([m.frame]);
          if (m.t === "frame") draw();
          return;
        case "live":
          draw();
          return;
        case "turn":
          tr.turn = { state: m.state, stop_reason: m.stop_reason || "" };
          if (m.state !== "running") tr.note = "";
          draw();
          return;
        case "ended":
          tr.endedReason = m.reason || "ended";
          return;
        case "error":
          tr.note = (m.code ? m.code + ": " : "") + (m.message || "error");
          if (!tr.interactive) lines.append(h("div", { class: "t-line muted" }, tr.note));
          drawComposer();
          return;
      }
    }

    function onClose() {
      const hadHello = !!tr.hello;
      const why = tr.endedReason;
      tr.ws = null;
      tr.hello = null;
      tr.endedReason = "";
      if (tr.stopped) return;
      if (why && why !== "daemon_stopping") {
        // The session is over; the wrapper removes its directory.
        tr.state = "lost";
        draw();
        return;
      }
      if (!hadHello && tr.interactive && !tr.ending) {
        // Interactive was refused (the session takes no prompts after
        // all): watch it instead.
        tr.interactive = false;
        tr.note = "";
        draw();
        attach();
        return;
      }
      if (!hadHello || tr.ending) {
        // The upgrade was refused (an ended session, no attach scope), or
        // the session is ending: read the log by polling instead, and stay
        // with it.
        tr.noRelay = true;
        fallBack();
        return;
      }
      // A dropped connection (too slow, the daemon restarting): re-attach
      // from the last frame.
      draw();
      tr.timer = setTimeout(attach, tr.retry);
      tr.retry = Math.min(tr.retry * 2, RETRY_MAX_MS);
    }

    // fallBack moves the view to polling, from the relay's last offset.
    function fallBack() {
      if (tr.stopped) return;
      tr.via = "poll";
      if (tr.offset >= 0) tr.next = tr.offset;
      draw();
      pollTranscript(tr);
    }

    // ---- polling (P.3) ----

    async function pollTranscript(tr) {
      // A poll still in flight when the view moved to the relay is dropped.
      if (tr.stopped || tr.via !== "poll") return;
      try {
        await startAt();
        for (;;) {
          const page = await wm.api.text("/sessions/" + enc(tr.sid) + "/transcript?after=" + tr.next + "&limit=" + PAGE);
          if (tr.stopped || tr.via !== "poll") return;
          const next = Number(page.headers.get("X-Transcript-Next"));
          const size = Number(page.headers.get("X-Transcript-Size"));
          const got = tx.parseNDJSON(page.text);
          if (isFinite(next) && isFinite(size) && next < tr.next && size < tr.next) {
            // The log is shorter than our offset: another log. Start over.
            tr.frames = [];
            tr.next = 0;
            continue;
          }
          if (isFinite(next)) tr.next = next;
          keep(got);
          // A full page means more is waiting: read on now.
          if (got.length < PAGE || !isFinite(size) || tr.next >= size) break;
        }
      } catch (e) {
        if (tr.stopped || tr.via !== "poll") return;
        if (e.status === 404) {
          tr.state = tr.frames.length || tr.ending ? "lost" : "gone";
          draw();
          return;
        }
        lines.append(h("div", { class: "t-line muted" }, String(e.message || e)));
      }
      if (tr.ending) {
        tr.state = "lost";
        draw();
        return;
      }
      draw();
      tr.timer = setTimeout(() => pollTranscript(tr), POLL_MS);
    }

    function entryEl(e) {
      switch (e.kind) {
        case "prompt":
          return h("div", { class: "t-entry t-prompt" }, h("span", { class: "muted small" }, e.note || "prompt"), h("pre", {}, e.text));
        case "text":
          return h("div", { class: "t-entry t-text" }, h("pre", {}, e.text.trim()));
        case "thought":
          return h("details", { class: "t-entry t-thought" }, h("summary", {}, "thinking"), h("pre", {}, e.text.trim()));
        case "tool": {
          const mark = tx.TOOL_MARK[e.status] || "";
          const head = h("span", { class: "t-tool-head" + (e.status === "failed" ? " bad" : "") }, "▸ ", e.title, mark ? " " + mark : "", e.tool ? h("span", { class: "muted small" }, " " + e.tool) : null);
          return e.output
            ? h("details", { class: "t-entry t-tool" }, h("summary", {}, head), h("pre", { class: "t-out" }, e.output))
            : h("div", { class: "t-entry t-tool" }, head);
        }
        case "plan":
          return h("div", { class: "t-entry t-plan" }, h("span", { class: "muted small" }, "plan"),
            h("ol", {}, e.items.map((p) => h("li", { class: "plan-" + (p.status || "pending") }, p.text))));
        default:
          return h("div", { class: "t-entry t-note t-" + e.kind }, e.text);
      }
    }

    function saveTranscript() {
      const lb = tx.label(tr.state, tr.session);
      const s = tr.session || {};
      const header = ["workingman transcript: session " + tr.sid, s.kind ? "kind " + s.kind + (s.task ? " · task " + s.task : "") : "", lb.text, "saved " + new Date().toISOString() + " from the GUI (redacted by the daemon)"].filter(Boolean).join("\n");
      const text = tx.asText(tx.entries(tr.frames, tr.session), header);
      if (typeof Blob === "undefined" || !URL.createObjectURL) return;
      const url = URL.createObjectURL(new Blob([text], { type: "text/plain" }));
      const a = h("a", { href: url, download: tx.fileName(tr.sid, Date.now()) });
      document.body.append(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    }

    const end = () => {
      if (tr.stopped || tr.state !== "live" || tr.ending) return;
      tr.ending = true;
      if (tr.via === "relay" && tr.ws) {
        // The relay sends the frames written before the end, then ended.
        return;
      }
      // One last read: the wrapper may not have removed the directory yet.
      clearTimeout(tr.timer);
      if (tr.via === "relay") fallBack();
      else pollTranscript(tr);
    };
    const off = wm.stream.on("session.ended", (ev) => {
      if (ev.data && ev.data.id === tr.sid) end();
    });

    draw();
    if (tr.via === "relay") attach();
    else pollTranscript(tr);
    return {
      el,
      sid: tr.sid,
      get state() {
        return tr.state;
      },
      get via() {
        return tr.via;
      },
      setSession(s) {
        tr.session = s;
        if (tr.via === "poll" && !tr.noRelay && tr.state === "live" && !tr.ending && relayable(s)) {
          // The session became attachable (its wrapper is up): move to the
          // relay, from where polling got to.
          clearTimeout(tr.timer);
          tr.via = "relay";
          if (tr.next >= 0) tr.offset = tr.next;
          attach();
        }
      },
      end,
      stop() {
        tr.stopped = true;
        clearTimeout(tr.timer);
        off();
        if (tr.ws) {
          const ws = tr.ws;
          tr.ws = null;
          ws.close(1000, "page closed");
        }
      },
    };
  };

  // ---- session watch (§G.2, §G.6) ------------------------------------------

  wm.screen("/sessions/:sid", async (params, main) => {
    const sid = params.sid;
    main.classList.add("wide");
    const head = h("div", { class: "card-head" }, h("h1", {}, "Session ", h("code", {}, sid)));
    main.append(head);
    let session = null;
    try {
      session = await wm.api.get("/sessions/" + enc(sid));
    } catch (e) {
      if (e.status !== 404) {
        main.append(wm.errorNotice(e));
        return () => main.classList.remove("wide");
      }
    }
    // ?prompt=1 is the Prompt control: attach interactively, when the
    // session takes prompts and the login may send them.
    const wantPrompt = new URLSearchParams(location.search).get("prompt") === "1";
    const view = wm.transcriptView({ sid, session, mode: "full", prompt: wantPrompt });
    // The header's Watch / Prompt: the page itself is Watch, so it offers
    // Prompt (or, while prompting, a way back to observe only).
    const control = (a) => {
      if (a.label === "Watch (ACP)") {
        return wantPrompt ? wm.writeAction("Observe only", a.command, { href: "/sessions/" + enc(sid), scope: "attach" }) : null;
      }
      if (a.label === "Prompt" && wantPrompt) return null;
      return a.href ? wm.writeAction(a.label, a.command, a) : wm.disabledAction(a.label, a.command, a.refusal, a.danger);
    };
    const drawHead = () => {
      const s = session;
      const sb = s && s.sandbox && s.sandbox.name;
      const facts = s
        ? [
            "kind " + s.kind,
            s.project ? ["project ", wm.link("/projects/" + enc(s.project), s.project.replace(/^p_\d+\./, ""))] : null,
            s.task && s.project ? ["task ", wm.link("/projects/" + enc(s.project) + "?task=" + enc(s.task), s.task)] : null,
            sb ? ["sandbox ", wm.link(s.task && s.project && (s.kind === "task" || s.kind === "commit") ? wm.taskCardHref(s.project, s.task) + (s.kind === "commit" ? "?sandbox=" + enc(sb) : "") : "/sandboxes/" + enc(sb), h("code", {}, sb))] : null,
            s.started_at ? "started " + new Date(s.started_at).toLocaleTimeString() : null,
            s.prompt_count ? "turn " + s.prompt_count : null,
          ].filter(Boolean)
        : ["the daemon no longer tracks this session"];
      head.replaceChildren(
        h("div", { class: "crumbs muted small" }, wm.link("/sandboxes", "Sandboxes"), " / session"),
        h("div", { class: "card-title" }, h("h1", {}, "Session ", h("code", {}, sid))),
        h("div", { class: "muted small" }, facts.map((f, i) => [i ? " · " : "", f])),
        h("div", { class: "actions" }, wm.wl.cardActions(session, session && session.sandbox && session.sandbox.name).map(control))
      );
    };
    drawHead();
    main.append(view.el);
    const off = wm.stream.on("session.updated", (ev) => {
      if (ev.data && ev.data.id === sid && ev.data.session) {
        session = ev.data.session;
        view.setSession(session);
        drawHead();
      }
    });
    return () => {
      main.classList.remove("wide");
      off();
      view.stop();
    };
  });
})();
