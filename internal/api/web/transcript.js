// workingman GUI: the transcript model (docs/proposals/workingman-gui.md
// §G.2, P.3, P.5). Pure functions over the frames of a session's
// stream.log, as the session relay (GET /api/v1/sessions/{sid}/acp, P.5)
// and GET /api/v1/sessions/{sid}/transcript (NDJSON, P.3) return them,
// redacted: the entries of the transcript view, its plain-text form for
// "Save transcript", and the label that says whether the transcript is live
// or lost and how it is read. watch.js draws what these return.
//
// stream.log holds what the agent wrote (the wrapper tees agent frames),
// so the prompts a client sent are not in it. Like the TUI's replay
// (internal/tui/acpwatch.go: replayPriorContext), a primed session gets the
// opening prompt as its first entry, and a prompt this page sent is kept as
// a local frame ({wm_local: "prompt", text}) where it was sent.
//
// Like graph.js, the file runs in the browser, where it becomes wm.tx, and
// under node for its tests (internal/api/testdata/transcript_test.js).
"use strict";

(function (factory) {
  const tx = factory();
  if (typeof module === "object" && module.exports) module.exports = tx;
  else window.wm.tx = tx;
})(function () {
  // OPENING_PROMPT is runner.initialPrompt, the one message every session
  // is primed with.
  const OPENING_PROMPT = "Read .orch/instructions.md and .orch/context.yaml, then follow the instructions.";

  // MAX_FRAMES bounds what a view keeps in memory; the oldest frames go
  // first, and the view says how many it dropped.
  const MAX_FRAMES = 20000;

  // parseNDJSON parses a transcript page; lines that are not JSON (a
  // redacted non-JSON line) are skipped.
  function parseNDJSON(text) {
    const out = [];
    for (const line of String(text).split("\n")) {
      if (!line.trim()) continue;
      try {
        out.push(JSON.parse(line));
      } catch (e) {
        // nothing to show
      }
    }
    return out;
  }

  // entries turns frames into the transcript's entries, oldest first:
  //   {kind: "prompt"|"text"|"thought"|"tool"|"plan"|"ask"|"turn", ...}
  // Consecutive message (or thought) chunks are joined into one entry. A
  // tool call is one entry, updated in place by its tool_call_updates. A
  // response carrying a stopReason ends a turn. session holds prompt_count,
  // for the opening prompt.
  function entries(frames, session) {
    const out = [];
    if (session && session.prompt_count > 0) out.push({ kind: "prompt", text: OPENING_PROMPT, note: "opening prompt" });
    const tools = new Map();
    let last = null;
    const chunk = (kind, text) => {
      if (!text) return;
      if (last && last.kind === kind) {
        last.text += text;
        return;
      }
      last = { kind, text };
      out.push(last);
    };
    for (const f of frames) {
      if (!f || typeof f !== "object") continue;
      if (f.wm_local === "prompt") {
        out.push({ kind: "prompt", text: String(f.text || ""), note: "you (from this page)" });
        last = null;
        continue;
      }
      if (f.method === "session/update" && f.params && f.params.update) {
        const u = f.params.update;
        switch (u.sessionUpdate) {
          case "agent_message_chunk":
            chunk("text", u.content && u.content.text);
            continue;
          case "agent_thought_chunk":
            chunk("thought", u.content && u.content.text);
            continue;
          case "tool_call":
          case "tool_call_update": {
            let t = u.toolCallId ? tools.get(u.toolCallId) : null;
            if (!t) {
              t = { kind: "tool", id: u.toolCallId || "", title: "", tool: "", status: "", output: "" };
              if (t.id) tools.set(t.id, t);
              out.push(t);
            }
            if (u.title) t.title = u.title;
            if (u.kind) t.tool = u.kind;
            if (u.status) t.status = u.status;
            const text = toolText(u.content);
            if (text) t.output = text;
            if (!t.title) t.title = t.tool || "tool call";
            break;
          }
          case "plan":
            out.push({ kind: "plan", items: (u.entries || []).map((e) => ({ text: String(e.content || ""), status: e.status || "" })) });
            break;
          default:
            continue;
        }
        last = null;
        continue;
      }
      if (f.method === "session/request_permission") {
        const tc = (f.params && f.params.toolCall) || {};
        out.push({ kind: "ask", text: "asked permission: " + (tc.title || "a tool call") });
        last = null;
        continue;
      }
      if (f.method === undefined && f.result && typeof f.result === "object" && f.result.stopReason) {
        out.push({ kind: "turn", text: "turn ended: " + f.result.stopReason });
        last = null;
        continue;
      }
      if (f.method === undefined && f.error) {
        out.push({ kind: "error", text: "error: " + (f.error.message || JSON.stringify(f.error)) });
        last = null;
      }
    }
    return out;
  }

  // toolText is the text a tool call's content blocks carry (as
  // acpclient.flattenToolOutput reads it).
  function toolText(blocks) {
    let s = "";
    for (const c of blocks || []) {
      if (c && c.content && c.content.text) s += c.content.text;
      else if (c && c.text) s += c.text;
    }
    return s;
  }

  // TOOL_MARK is a tool call's status glyph.
  const TOOL_MARK = { completed: "✓", failed: "✗", in_progress: "◐", pending: "○" };

  // asText is the transcript as plain text: what "Save transcript" writes.
  function asText(list, header) {
    const out = [];
    if (header) out.push(header, "");
    for (const e of list) {
      switch (e.kind) {
        case "prompt":
          out.push("> " + e.text);
          break;
        case "text":
          out.push(e.text.trim());
          break;
        case "thought":
          out.push("(thinking) " + e.text.trim());
          break;
        case "tool":
          out.push("▸ " + e.title + (e.status ? " " + (TOOL_MARK[e.status] || e.status) : ""));
          if (e.output) out.push(indent(e.output.trim()));
          break;
        case "plan":
          out.push("plan:", ...e.items.map((p, i) => "  " + (i + 1) + ". " + p.text + (p.status ? " [" + p.status + "]" : "")));
          break;
        default:
          out.push("-- " + e.text);
      }
      out.push("");
    }
    return out.join("\n");
  }

  function indent(s) {
    return s.split("\n").map((l) => "    " + l).join("\n");
  }

  // label says what the transcript is: live and not kept, lost because the
  // session ended (the wrapper deletes the session directory, survey §8.2),
  // or gone before anything was loaded. state is "live", "lost" or "gone";
  // how says how a live one is read: via "relay" (the session relay's
  // WebSocket, P.5) or "poll" (GET …/transcript, the default), and
  // interactive when this page may prompt it.
  function label(state, session, how) {
    const h = how || {};
    if (state === "lost") {
      return {
        cls: "lost",
        text: "session ended — transcript lost",
        detail: "the wrapper deleted the session directory and its stream.log; shown is what this page had loaded. Save transcript keeps a copy (R.4 Q4).",
      };
    }
    if (state === "gone") {
      return { cls: "lost", text: "transcript lost", detail: "the session's stream.log is gone: it is deleted when the session ends." };
    }
    const kind = (session && session.kind) || "";
    const what = session && session.persistent ? "persistent " + kind + " session" : "one-shot " + (kind || "agent") + " session";
    let mode = "read-only (observe), replayed from stream.log";
    if (h.via === "relay") mode = h.interactive ? "interactive over the session relay: prompts go into the conversation" : "observe over the session relay, live";
    return { cls: "live", text: "live — not kept after the session ends", detail: what + " · " + mode };
  }

  // turnText is the turn chip of a relayed transcript: the relay's turn
  // message ({state, stop_reason}), or "" before one arrived.
  function turnText(turn) {
    if (!turn || !turn.state) return "";
    if (turn.state === "running") return "turn running…";
    return "idle" + (turn.stop_reason ? " · " + turn.stop_reason : "");
  }

  // promptable reports whether a human may type into a session (§G.2): the
  // persistent kinds the TUI lets you type into, wolf, workingman and
  // sleepy-head. Step-up is persistent too, but it is the escalation path
  // and takes no typed turns; the relay refuses the same sessions.
  function promptable(session) {
    return !!(session && session.persistent && session.kind !== "step-up");
  }

  // fileName is "Save transcript"'s file name.
  function fileName(sid, now) {
    const d = new Date(now);
    const p = (n) => String(n).padStart(2, "0");
    const stamp = isNaN(d) ? "" : "-" + d.getFullYear() + p(d.getMonth() + 1) + p(d.getDate()) + "-" + p(d.getHours()) + p(d.getMinutes());
    return String(sid || "session").replace(/[^A-Za-z0-9._-]+/g, "_") + stamp + ".transcript.txt";
  }

  return { OPENING_PROMPT, MAX_FRAMES, TOOL_MARK, parseNDJSON, entries, toolText, asText, label, turnText, promptable, fileName };
});
