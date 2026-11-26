// workingman GUI: the read-only transcript view (docs/proposals/workingman-gui.md
// §G.2, P.3) and the session watch page. wm.transcriptView polls
// GET /api/v1/sessions/{sid}/transcript (NDJSON, redacted, paged by byte
// offset) while the session lives; the project view's selected-task strip
// and the sandbox card embed it as a preview, and /sessions/{sid} shows it
// whole. There is no WebSocket yet (P.5): nothing is sent to the session.
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

  // POLL_MS is how often a live transcript is re-read.
  const POLL_MS = 2000;
  // PREVIEW_BYTES is how far before the end of stream.log a preview starts.
  const PREVIEW_BYTES = 200000;
  // PAGE is the frames asked for per request.
  const PAGE = 2000;

  // transcriptView makes a transcript panel for session sid. opts:
  //   session  the session resource (kind, persistent, prompt_count)
  //   mode     "preview" (the tail, as one-line entries) or "full"
  //   link     in preview mode, show "Full transcript ▸"
  // It returns {el, sid, setSession, end, stop}: end() marks the session
  // ended (the view reads once more, then labels the transcript lost);
  // stop() stops polling and listening, for a screen's cleanup.
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
    };
    const badge = h("span", { class: "t-state" });
    const lines = h("div", { class: full ? "t-entries" : "t-lines" }, h("div", { class: "t-line muted" }, "loading…"));
    const detail = h("span", { class: "muted small" });
    const meta = h("span", { class: "muted small" });
    const save = h("button", { class: "action local", "data-local": "true", title: "save what this page has loaded as a text file; nothing is sent to the daemon", onclick: () => saveTranscript() }, "Save transcript");
    const el = h("div", { class: "transcript" + (full ? " transcript-full" : "") },
      h("div", { class: "t-head" }, h("span", {}, full ? "transcript" : "transcript (observe, replayed from stream.log)"), " ", badge),
      lines,
      h("div", { class: "t-foot" }, detail, " ", meta, " ", save,
        !full && opts.link !== false ? [" ", wm.link("/sessions/" + enc(tr.sid), "Full transcript ▸")] : null)
    );

    const draw = () => {
      const lb = tx.label(tr.state, tr.session);
      badge.className = "t-state t-" + lb.cls;
      badge.textContent = lb.text;
      detail.textContent = lb.detail;
      meta.textContent = tr.frames.length ? tr.frames.length + " frames" + (tr.dropped ? " (" + tr.dropped + " older dropped)" : "") + (!full && tr.startedAt > 0 ? " · from byte " + tr.startedAt : "") : "";
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

    async function pollTranscript(tr) {
      if (tr.stopped) return;
      try {
        if (tr.next < 0) {
          // A preview starts near the end: it shows only the tail. A first
          // line cut in half is not JSON and is skipped.
          tr.next = 0;
          if (!full) {
            const head = await wm.api.text("/sessions/" + enc(tr.sid) + "/transcript?limit=1");
            const size = Number(head.headers.get("X-Transcript-Size")) || 0;
            tr.next = Math.max(0, size - PREVIEW_BYTES);
          }
          tr.startedAt = tr.next;
        }
        for (;;) {
          const page = await wm.api.text("/sessions/" + enc(tr.sid) + "/transcript?after=" + tr.next + "&limit=" + PAGE);
          if (tr.stopped) return;
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
          tr.frames.push(...got);
          if (tr.frames.length > tx.MAX_FRAMES) {
            tr.dropped += tr.frames.length - tx.MAX_FRAMES;
            tr.frames.splice(0, tr.frames.length - tx.MAX_FRAMES);
          }
          // A full page means more is waiting: read on now.
          if (got.length < PAGE || !isFinite(size) || tr.next >= size) break;
        }
      } catch (e) {
        if (tr.stopped) return;
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
      // One last read: the wrapper may not have removed the directory yet.
      tr.ending = true;
      clearTimeout(tr.timer);
      pollTranscript(tr);
    };
    const off = wm.stream.on("session.ended", (ev) => {
      if (ev.data && ev.data.id === tr.sid) end();
    });

    draw();
    pollTranscript(tr);
    return {
      el,
      sid: tr.sid,
      get state() {
        return tr.state;
      },
      setSession(s) {
        tr.session = s;
      },
      end,
      stop() {
        tr.stopped = true;
        clearTimeout(tr.timer);
        off();
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
    const view = wm.transcriptView({ sid, session, mode: "full" });
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
        h("div", { class: "actions" }, wm.wl.cardActions(session, session && session.sandbox && session.sandbox.name).map((a) => wm.disabledAction(a.label, a.command, a.refusal, a.danger)))
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
