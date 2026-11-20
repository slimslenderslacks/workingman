# Research: async requests in the `post-office` MCP server

How `post-office`'s send-message tools should handle a **request**, a message
whose answer may take minutes or hours to arrive, or may never arrive. This
note looks at what the MCP specification offers, what Claude Code (our client,
running behind ACP in a sandbox) actually supports, what the Go SDKs can do,
and the standalone-codemode gateway that sits between the two. It ends with a
concrete tool surface and a hosting and identity plan.

Contents: [TL;DR](#tldr) ·
[What the spec offers](#1-what-the-mcp-spec-offers) ·
[What Claude Code supports](#2-what-claude-code-supports-today) ·
[Go SDKs](#3-go-sdks) ·
[The gateway in the middle](#4-the-gateway-in-the-middle) ·
[Recommended tool surface](#5-recommended-tool-surface) ·
[Hosting and caller identity](#6-hosting-and-caller-identity) ·
[Verified behaviour](#7-verified-behaviour-gateway-and-harness) ·
[Sources](#sources)

Research date: 2026-11-20. Spec pages were read from the spec's GitHub source
(`modelcontextprotocol/modelcontextprotocol`, `main`). The modelcontextprotocol.io
URLs below are the rendered versions of the same pages.

## TL;DR

- **The spec's answer to "a tool that waits for a human" is Tasks.** Tasks
  were added in 2025-11-25 as an experimental core primitive (SEP-1686). In
  **2026-07-28** they became the `io.modelcontextprotocol/tasks` **extension**
  (SEP-2663). The extension drops `tasks/result` and `tasks/list`, adds
  `tasks/update`, and has the server decide when a call becomes a task.
  "Human-in-the-loop approval gates" is a use case the extension names
  explicitly.
- **Claude Code supports the 2026-07-28 Tasks extension, in public beta**
  (v2.1.304+), but only for servers that speak 2026-07-28 and only when the
  call is made from the main conversation. It does not use the legacy
  2025-11-25 task flow.
- **Neither Go SDK can serve the 2026-07-28 Tasks extension today.**
  `modelcontextprotocol/go-sdk`, which standalone-codemode already uses, has no
  task support at all. `mark3labs/mcp-go` has only the legacy 2025-11-25
  model, which Claude Code does not use.
- **Every call goes through the standalone-codemode gateway**
  (`/gateways/<id>`, built on `codemode/mcpruntime`). Measured since (§7):
  it drops `_meta.progressToken`, so no progress reaches the agent; it
  imposes no per-call timeout; and through it a call may block up to
  Claude Code's 300 s tool idle timeout. Recommended `max_wait`: 240 s.
- **So build the practical fallback now: a pending handle plus a bounded-wait
  tool.**
  - `send_request` waits a short, bounded time and returns either the answer
    or a `request_id`.
  - `wait_for_response(request_id, timeout_seconds)` blocks for up to about
    50 s per call, and the agent loops on it. `check_response` and
    `cancel_request` complete the set.
  - Requests are persisted in orch's inbox and outbox directories, not in the
    MCP session, so they survive reconnects, gateway restarts and agent
    restarts.
  - Send progress notifications while blocked, as a best-effort keep-alive.
  - Treat the Tasks extension as a later upgrade path. The tool contract
    stays the same.
- **Hosting:** a standalone HTTP MCP binary, `workingman/cmd/post-office`,
  registered in the standalone-codemode catalog as `post-office`. Tasks opt in
  with `static_mcps: [post-office]`.
- **Caller identity:** the gateway forwards nothing that identifies the
  sandbox today. The cleanest fix is a small gateway change that injects the
  sub-gateway id as a header (`X-Codemode-Gateway-Id`). post-office then maps
  that id to sandbox and task through `~/.workingman/gateways/*.json`.

## 1. What the MCP spec offers

### Timeouts (every revision)

From 2025-06-18 and 2025-11-25 *Basic › Lifecycle › Timeouts*, unchanged in
substance in 2026-07-28, where the section moved to *Basic › Patterns ›
Cancellation*:

> Implementations **SHOULD** establish timeouts for all sent requests … When
> the request has not received a success or error response within the timeout
> period, the sender **SHOULD** issue a cancellation notification for that
> request and stop waiting for a response. SDKs and other middleware
> **SHOULD** allow these timeouts to be configured on a per-request basis.
> Implementations **MAY** choose to reset the timeout clock when receiving a
> progress notification … However, implementations **SHOULD** always enforce a
> maximum timeout, regardless of progress notifications.

What this means for us: a plain `tools/call` that blocks for hours is
explicitly at the mercy of client and middleware timeouts. Progress can extend
the timeout, but only until the client's hard cap.

### Progress

- **How it works:** the requester sets `_meta.progressToken`. The server
  **MAY** send `notifications/progress {progressToken, progress, total?,
  message?}`, and `progress` **MUST** increase with each notification.
  ([2025-11-25 progress](https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/progress))
- **Change in 2026-07-28:** the structure is the same, but progress flows only
  from server to client.
  ([2026-07-28 progress](https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/progress))
- **Use for post-office:** a heartbeat while a tool call blocks, with
  `message` set to something like "waiting for operator (12m elapsed)".

### Cancellation

- **2025-11-25:** either side sends `notifications/cancelled {requestId,
  reason?}`. The receiver **SHOULD** stop work and send no response. Requests
  that were made as tasks must use `tasks/cancel` instead.
  ([2025-11-25 cancellation](https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/cancellation))
- **2026-07-28:** only the client cancels. On Streamable HTTP, a client
  disconnect **MUST** be treated as cancellation.
  ([2026-07-28 cancellation](https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/cancellation))
- **What this means for post-office:** a cancelled `wait_for_response`
  **call** must not cancel the underlying **message request**. Only an
  explicit `cancel_request` does that. Otherwise every client timeout would
  withdraw the question from the operator.

### Tasks, 2025-11-25 (experimental core, SEP-1686)

([2025-11-25 tasks](https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/tasks))

- **Server capability:** `capabilities.tasks = {list:{}, cancel:{},
  requests:{tools:{call:{}}}}`.
- **Per-tool opt-in:** `execution.taskSupport: "forbidden" | "optional" |
  "required"`.
- **Starting a task:** the client adds `"task": {"ttl": <ms>}` to `tools/call`.
  The server returns `CreateTaskResult {task:{taskId, status:"working", ttl,
  pollInterval, …}}` straight away. It can also give the model text to see
  immediately via `_meta["io.modelcontextprotocol/model-immediate-response"]`.
- **Statuses:** `working`, `input_required`, `completed`, `failed`,
  `cancelled`.
- **Methods:**
  - `tasks/get` polls for status and should respect `pollInterval`.
  - `tasks/result` **MUST block** until the task is terminal.
  - `tasks/list` lists tasks.
  - `tasks/cancel` cancels.
  - `notifications/tasks/status` is optional, and clients **MUST NOT** rely
    on it.

### Tasks, 2026-07-28 (extension, SEP-2663)

([extension overview](https://modelcontextprotocol.io/extensions/tasks/overview),
[spec repo](https://github.com/modelcontextprotocol/ext-tasks),
[SEP-2663](https://github.com/modelcontextprotocol/modelcontextprotocol/pull/2663),
[2026-07-28 changelog](https://modelcontextprotocol.io/specification/2026-07-28/changelog))

- **Declaring it:** the client declares `io.modelcontextprotocol/tasks` in
  each request's `_meta["io.modelcontextprotocol/clientCapabilities"]`. The
  server advertises it through `server/discover`.
- **Server-directed:** the server decides for each request whether to return
  `CreateTaskResult` (`resultType:"task"`). There is no `task` parameter and
  no `execution.taskSupport` negotiation.
- **Methods:**
  - `tasks/get` polls. A terminal response carries the result or the error.
  - `tasks/update` sends `inputResponses` when the task is `input_required`.
  - `tasks/cancel` cancels.
  - `tasks/result` was removed (the SEP calls it "a blocking trap"), and so
    was `tasks/list`.
- **Renamed fields:** `ttlMs` and `pollIntervalMs`.
- **Scope:** only `tools/call` can become a task. The extension is not
  defined for 2025-11-25 sessions.
- **The overview names our use case:** "Human-in-the-loop workflows. Approval
  gates, review steps… The task moves to `input_required`". It also notes that
  intermediary timeouts make blocking "impractical beyond a few seconds".

**The spec-recommended pattern** for "block minutes to hours on a human" is
therefore:

1. Return a `CreateTaskResult` from `send_request` right away.
2. Keep the task `working` until the recipient answers.
3. Complete the task with the answer, or fail or cancel it when the TTL
   expires.

### Elicitation

- **2025-06-18:** form mode only.
- **2025-11-25:** adds URL mode. The server sends `elicitation/create` and the
  client asks *its own user*.
  ([2025-11-25 elicitation](https://modelcontextprotocol.io/specification/2025-11-25/client/elicitation))
- **2026-07-28:** server-initiated `elicitation/create` is replaced by MRTR
  (multi round-trip). The tool returns `InputRequiredResult` with
  `inputRequests`, and the client retries the call with `inputResponses`.
  ([MRTR](https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/mrtr))

**Not a fit for post-office.** Elicitation asks the person driving the
*client*. Our agents run headless under ACP, where Claude Code turns an
unanswered elicitation into a timeout error (changelog 2.1.310) or
auto-declines it (Agent SDK default). The human operator is reached through
orch channels (the TUI inbox, Slack, Signal, WhatsApp), not through the
client. post-office sits on the *other* side: it is the thing that delivers
the question to the human.

### Resource subscriptions

- **2025-11-25:** `resources/subscribe {uri}` leads to
  `notifications/resources/updated {uri}`.
  ([resources](https://modelcontextprotocol.io/specification/2025-11-25/server/resources))
- **2026-07-28:** replaced by `subscriptions/listen` with
  `resourceSubscriptions` filters.
  ([subscriptions](https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/subscriptions))

**Not a fit as the primary mechanism.** An update notification is not tied to
a tool call, so the model is not woken by it. We also found no evidence that
Claude Code acts on `resources/updated`. Exposing `post-office://requests/<id>`
as a readable resource is still cheap and handy for debugging.

### History

- [SEP-1391 "Long-Running Operations"](https://github.com/modelcontextprotocol/modelcontextprotocol/issues/1391)
  was closed in favour of
  [SEP-1686 "Tasks"](https://github.com/modelcontextprotocol/modelcontextprotocol/issues/1686).
- SEP-1686 was then reworked into SEP-2663, the Tasks extension.
- The 2026-07-28 GA blog post frames Tasks as "support for reliable,
  long-running agents".

## 2. What Claude Code supports today

Claude Code v2.1.329 (2026-11-19). Sources:
[MCP docs](https://code.claude.com/docs/en/mcp),
[env vars](https://code.claude.com/docs/en/env-vars),
[CHANGELOG](https://github.com/anthropics/claude-code/blob/main/CHANGELOG.md).

| Mechanism | Behaviour | Effect on post-office |
|---|---|---|
| `MCP_TOOL_TIMEOUT` / per-server `timeout` | Hard wall-clock cap per tool call. The default is 100,000,000 ms (about 28 h). Progress does **not** extend it. | The default is generous. A per-server `timeout` would override it. |
| HTTP first-byte timer | **60 s** per request until the server's first response byte. Raised only if `MCP_TOOL_TIMEOUT` or the per-server `timeout` is above 60000. | **Binding for a go-sdk HTTP server reached directly.** Through the gateway it is not: the gateway's SSE `prime` event is the first byte, so the 5 min idle timeout below binds instead (§7). |
| `CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT` | Aborts the call if there is no response *and no progress* within 5 min (network) or 30 min (stdio). `0` disables it. | Progress heartbeats every 30–60 s would keep a long call alive past this, but the gateway drops the progress token, so through it this is the binding limit (§7). |
| Auto-backgrounding | A main-conversation MCP call still running after 2 min moves to `/tasks`, and the model gets the result later (`CLAUDE_CODE_MCP_AUTO_BACKGROUND_MS`). | A long `wait_for_response` may be backgrounded. This is harmless, but the agent may move on in the meantime. |
| Progress notifications | Rendered in the tool view and in background tasks. | Useful for heartbeats. |
| **MCP Tasks** | Public beta, v2.1.304+. Supports **only the 2026-07-28 extension**, for HTTP, stdio or connector servers on that revision, in terminal, Agent SDK or GitHub Actions runs, for main-conversation calls. `pollIntervalMs` is clamped to 250 ms–30 s. Supports `input_required`. In headless mode a task that asks for input is cancelled. Disabled by `CLAUDE_CODE_DISABLE_MCP_TASKS`. | This is the eventual "right" answer, but every link in the chain (go-sdk, gateway, harness version) has to support it first. |
| Elicitation | Form and URL modes. Headless runs time out or decline. | Not usable for our flow (see above). |
| `resources/updated` | Not verified. | Do not rely on it. |

**ACP adapter.** The harness is `slimslenderslacks/claude-code-acp:0.90.0`, a
build of `agentclientprotocol/claude-agent-acp`, which was formerly
`zed-industries/claude-code-acp`.

- `src/mcp-servers.ts` maps ACP MCP servers to only `url` plus `headers`, so
  **no per-server `timeout` is passed through**.
- Environment variables (`MCP_TOOL_TIMEOUT`, `CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT`)
  do reach Claude Code through `process.env` and `settings.json` `env`. This
  is inferred from the source, not tested.
- In our setup, acp-kit registers a single server, `okta-gateway`, so any
  timeout tuning applies to **every** tool behind the gateway.

**Which Claude Code version runs in the harness:** 2.1.293 (measured, §7),
older than 2.1.304, so there are no Tasks. The timeout variables above
behave as described there.

## 3. Go SDKs

### Current usage

- **workingman:** no MCP SDK. `internal/mcpgateway/session.go` is a
  hand-rolled JSON-RPC client that speaks protocol `2025-06-18` and allows
  60 s per request.
- **standalone-codemode:** `github.com/modelcontextprotocol/go-sdk
  v1.8.1-0.20260918074105-3b917b466cc5`, plus
  `github.com/slimslenderslacks/codemode` (`mcpruntime`, the gateway core).

### Capability matrix

| Feature | go-sdk (v1.8.x; v1.9.0-pre.1 2026-11-17) | mark3labs/mcp-go (v1.6.0, 2026-11-19) |
|---|---|---|
| Streamable HTTP server | ✅ `NewStreamableHTTPHandler`. Use `Stateless: true` for 2026-07-28; otherwise clients negotiate down to 2025-11-25. | ✅ |
| Progress (server → client) | ✅ `ServerSession.NotifyProgress`, `CallToolParams.GetProgressToken()` | ✅ |
| Cancellation | ✅ the request `ctx` is cancelled (`mcp/transport.go`); opt-in `PropagateRequestCancellation` for HTTP | ✅ |
| Tasks, 2025-11-25 | ❌ ([#626](https://github.com/modelcontextprotocol/go-sdk/issues/626) open; draft PR #1335 adds types only) | ✅ since v0.44.0 (`WithTaskSupport`, `AddTaskTool`, `tasks/get\|list\|result\|cancel`) |
| Tasks extension, 2026-07-28 | ❌ ([#942](https://github.com/modelcontextprotocol/go-sdk/issues/942) closed as "no work, not implemented yet") | ❌ (open PR #987 / issue #980) |
| Elicitation / MRTR | ✅ `ServerSession.Elicit` on ≤ 2025-11-25; MRTR `InputRequests` on 2026-07-28 (`mcp/mrtr.go`) | ✅ |
| Resource subscriptions | ✅ `SubscribeHandler`, `Server.ResourceUpdated`, `subscriptions/listen` | ✅ |
| Session id / HTTP headers in handlers | ✅ `ServerSession.ID()`, `req.Extra.Header` (`mcp/shared.go`) | ✅ |

**Recommendation: use `modelcontextprotocol/go-sdk`**, the same SDK and the
same pinned version as standalone-codemode. It covers everything the fallback
design needs (HTTP, progress, cancellation, headers). Picking mcp-go to get
legacy tasks would buy nothing, because Claude Code does not use the
2025-11-25 task flow. When go-sdk ships SEP-2663 we add Tasks on top without
changing the tools.

## 4. The gateway in the middle

```
claude (sandbox) ──HTTP──▶ standalone-codemode /gateways/<id>  ──HTTP──▶ post-office
                           (codemode/mcpruntime proxy)                (127.0.0.1:<port>/mcp)
```

- **The client is the gateway, not Claude.** Claude Code never talks to
  post-office directly. It sees `okta-gateway`, and the gateway re-exposes
  post-office's tools. Every async feature has to survive this proxy.
- **Measured in §7:** `mcpruntime` drops `_meta.progressToken` (so no
  `notifications/progress` reaches the client), propagates a client cancel
  to a stateful backend but not to a stateless one, and applies no per-call
  timeout of its own. Whether it passes `CreateTaskResult` and `tasks/*`
  through was not tested; it rebuilds `CallToolParams` from name and
  arguments only, so it almost certainly does not.
- **Gateway-side timeouts we found:**
  - backend attach, 60 s (`aggregategw.DefaultAttachTimeout`);
  - catalog resolve, 30 s.
- **Code mode:** not in play on sub-gateways (§7). Backend tools are listed
  as plain tools with their own names, schemas and content.

**This is why the design must be correct with no protocol support at all:**
short bounded calls, plain-text results, and state held server-side behind a
handle. Everything else is an optimisation.

## 5. Recommended tool surface

### Principles

1. **A request is a durable object, not a tool call.**
   - Each request gets a `request_id` (`req_` + ULID) and is written to orch's
     inbox directory.
   - The response arrives through the outbox directory (TUI) or a channel
     (Slack, Signal, WhatsApp).
   - Tool calls only *observe* the request, so a cancelled or timed-out tool
     call never loses or withdraws it.
2. **Every blocking call is bounded below about 55 s by default**, to stay
   under the 60 s HTTP first-byte timer and the gateway's own limits. A
   server flag `--max-wait` raises it. §7 measured that, through the
   gateway, calls survive up to Claude Code's 300 s idle timeout, and
   recommends `--max-wait 240s`.
3. **Send heartbeats while blocked.** When a call blocks and the request
   carried a `progressToken`, send `notifications/progress` every 15 s with a
   human-readable message. If the gateway drops them, nothing breaks.
4. **Results are plain text with a fixed first line**, so the model can branch
   on it even when the gateway flattens structured content. We also return
   `structuredContent` with the same fields.
5. **The default recipient is the human operator.** An omitted or empty
   `recipient`, or `"operator"`, routes to the human through the TUI inbox and
   any active channels. Other values name agents, for example `"step-up"`.
6. **The sender is never a parameter.** It is derived from caller identity
   (see §6), so an agent cannot impersonate another.

### Tools

#### `send_notification`

```
send_notification(subject: string, body: string, recipient?: string) -> {message_id}
```

Description text:

> Send a one-way notification. No reply is expected and this call returns
> immediately. Use it for status updates, FYIs, or to tell someone that you
> took an action. `recipient` defaults to the human operator; set it to an
> agent name (for example `"step-up"`) to notify that agent instead. Do not
> use this when you need an answer. Use `send_request` instead.

#### `send_request`

```
send_request(body: string, subject?: string, recipient?: string,
             wait_seconds?: int = 45, expires_in_minutes?: int = 1440,
             options?: string[])
  -> one of
     status: answered   request_id, response, responder, channel
     status: pending    request_id, expires_at
     status: rejected   reason        (e.g. unknown recipient, not permitted)
```

- `wait_seconds` is clamped to `[0, max_wait]` (default `max_wait` = 50).
- `expires_in_minutes` is the request's TTL. After it passes, the request
  becomes `expired`, and every channel and the TUI inbox mark it "expired —
  no longer waiting".
- `options` is an optional list of suggested answers. Channels can render
  them as buttons or numbered choices.

Description text:

> Ask a question that needs an answer before you can continue, such as
> missing information, an approval, or help. `recipient` defaults to the
> human operator; set it to an agent name (for example `"step-up"`) to ask
> that agent.
>
> This call waits up to `wait_seconds` (max 50) for an answer.
> - If the first line of the result is `status: answered`, the `response`
>   field holds the reply.
> - If it is `status: pending`, the question has been delivered and is still
>   open. Keep the `request_id`. The answer may take minutes or hours.
>
> **To wait for the answer**, call `wait_for_response` with the `request_id`.
> Each call waits up to about 50 seconds. If it returns `status: pending`
> again, call it again. Repeat for as long as you are willing to wait. If you
> can make progress on other work in the meantime, do that and use
> `check_response` occasionally instead of waiting continuously.
>
> **If you decide to stop waiting**, call `cancel_request` with the
> `request_id`. The recipient is told that you no longer need an answer. Then
> continue with your best judgement and mention in your output that the
> request went unanswered.
>
> The request expires automatically after `expires_in_minutes` (default
> 24 h). Do not send the same request twice. Reuse the `request_id` you
> already have.

#### `wait_for_response`

```
wait_for_response(request_id: string, timeout_seconds?: int = 45)
  -> status: answered | pending | expired | cancelled
     (+ response, responder, channel when answered; elapsed, expires_at)
```

Description text:

> Block for up to `timeout_seconds` (max 50) waiting for the answer to a
> request you made with `send_request`.
> - `status: answered`: the reply is in `response`.
> - `status: pending`: still no answer. Call `wait_for_response` again to
>   keep waiting, or `cancel_request` to give up.
> - `status: expired` or `status: cancelled`: no answer will come. Continue
>   without it.
>
> If this tool call itself is interrupted or times out, the request is **not**
> cancelled. Just call `wait_for_response` again.

#### `check_response`

```
check_response(request_id: string) -> same shape as wait_for_response, never blocks
```

Description text:

> Check, without waiting, whether a request made with `send_request` has been
> answered. Use this between other work. Use `wait_for_response` when you have
> nothing else to do.

#### `cancel_request`

```
cancel_request(request_id: string, reason?: string) -> status: cancelled | answered
```

Description text:

> Stop waiting for a request made with `send_request`. The recipient sees
> that you no longer need an answer, and the request is removed from the
> operator's inbox. If an answer had already arrived, this returns it with
> `status: answered` instead.

#### `list_requests` (optional)

```
list_requests(status?: "pending"|"answered"|"all") -> [{request_id, subject, recipient, status, created_at}]
```

This exists for an agent that has lost track of its handles, for example after
context compaction. Results are scoped to the calling agent.

### Receiver side (Step-Up and other agents)

An agent that *receives* requests needs a matching pair of tools, exposed only
to agents with the receive capability:

- `next_message(timeout_seconds)` uses the same bounded long-poll pattern.
- `reply(request_id, body)` answers a request.

These are a natural follow-on but are out of scope for this note. An
agent-to-agent request is the same durable object with an agent named in
`recipient`.

### Upgrade path: the 2026-07-28 Tasks extension

Once go-sdk implements SEP-2663, the gateway passes tasks through, and the
harness's Claude Code is v2.1.304 or later:

- `send_request` (when the client declares `io.modelcontextprotocol/tasks`)
  returns `CreateTaskResult` with `taskId = request_id` and `ttlMs` set from
  `expires_in_minutes`.
- The task completes with the same `status: answered …` text, or fails as
  `expired` or `cancelled`.
- `tasks/cancel` maps to `cancel_request`.
- Claude Code then polls and backgrounds the task itself, and the agent never
  has to loop on `wait_for_response`.
- The handle tools stay for clients without Tasks, and for code-mode use.

## 6. Hosting and caller identity

### Options

| Option | Pros | Cons |
|---|---|---|
| **A. Standalone binary `workingman/cmd/post-office`, HTTP on `127.0.0.1:<port>/mcp`, registered in the standalone-codemode catalog** | It matches how every other MCP reaches sandboxes. Tasks opt in with `static_mcps: [post-office]`, so the capability equals the catalog entry being on the task's sub-gateway. It is independent of orch daemon restarts. It can be run and tested alone. | One more process (launchd plist). It needs the orch state dir to resolve identity and to share inbox and outbox. |
| B. Served by the orch daemon on an extra HTTP listener | No extra process, and direct access to project, task and channel state in memory. | It couples MCP uptime to orch restarts. In-flight long-polls drop when orch restarts, although that is survivable because requests are durable. It mixes concerns in `cmd/orch`. |

**Recommendation: Option A.**

- Put the logic in `internal/postoffice`, with a `doc.go` for `task docs`, and
  a thin `cmd/post-office/main.go`.
- Add a Taskfile build task and a launchd plist next to the existing ones.
- Exchange state with orch only through files:
  - `~/.workingman/post-office/inbox/<request_id>.json` is written by
    post-office and read by the TUI and channels;
  - `~/.workingman/post-office/outbox/<request_id>.json` is written by the TUI
    or channels and read by post-office.

  Use atomic write-then-rename, and watch both directories with `fsnotify`,
  with a poll as fallback.
- Keeping the code in `internal/` means orch could embed it later (Option B)
  without a rewrite.

### Catalog entry

Add the entry to `standalone-codemode/mcp-servers.json`:

```json
{
  "name": "post-office",
  "url": "http://127.0.0.1:7077/mcp",
  "header": {"Authorization": "Bearer ${post_office_token}"}
}
```

The bearer token stops other local processes from posting as the gateway.
post-office rejects any caller without it.

### Restricting it to agents with the messaging capability

- **Enforce it in workingman:** add `post-office` to a task's `static_mcps`,
  or to an agent definition's default set (Step-Up). Do not add it to the
  gateway's `--mcp-gateway-servers` default.
- **Watch for `dynamic: true` gateways:** they expose the whole catalog via
  `mcp-add`, so a dynamic sandbox could add post-office itself. Either have
  post-office reject callers whose sandbox's recorded `servers` do not include
  `post-office` (see identity, next), or add a catalog-level "not
  dynamically addable" flag to standalone-codemode.

### Identifying the calling agent or sandbox

Today the gateway sends backends nothing that identifies the sub-gateway or
sandbox. Catalog headers are static per entry. In order of preference:

1. **Gateway injects `X-Codemode-Gateway-Id: <sub-gateway id>`** on every
   backend request.
   - This is a small change in standalone-codemode (`aggregategw` or
     mcpruntime header hook).
   - post-office maps the id to sandbox via
     `~/.workingman/gateways/<sandbox>.json` (fields `id`, `sandbox`,
     `servers`).
   - It maps sandbox to project and task via orch's state.
   - It cannot be spoofed by the agent, because the agent never talks to
     post-office directly and the id is the gateway's own.
   - It also enables the capability check above.
   - **This is the recommendation.**
2. Per-sandbox catalog entries or per-gateway header overrides, for example
   `POST /gateways {"servers": [...], "headers": {"post-office": {"X-Sandbox":
   "…"}}}`. This is more flexible but a bigger API change.
3. Infer from `Mcp-Session-Id`. This is fragile: post-office sees the
   gateway's backend session, not the sandbox, and it is not established
   whether sub-gateways share backend sessions.
4. Have the agent pass its own name as a tool argument. This is rejected
   because it can be spoofed.

Every stored request records `from: {sandbox, project, task, agent}`, so a
reply can be routed back. Notifications to the operator render that
attribution in the TUI and channels.

## 7. Verified behaviour: gateway and harness

Measured 2026-11-20 by task `verify-gateway-async-passthrough`
(messaging-design §9 step 1). This section replaces the earlier list of open
questions; each answer below is a measurement, not an inference.

### Setup

- **Backend:** a throwaway go-sdk server (same pinned go-sdk as
  standalone-codemode) with one tool, `block(seconds, progress_every)`. It
  blocks for `seconds`, optionally sends `notifications/progress` every
  `progress_every` seconds when the call carries a `progressToken`, and logs
  the token it received, the `X-Codemode-Gateway-Id` header, progress sends
  and context cancellation. Run both stateful (the go-sdk default) and
  stateless (`Stateless: true`, as post-office runs).
- **Gateway:** standalone-codemode at `c18d384b` (`gatewayapp.Build`, the
  real `/gateways` handler, in-memory secrets), with the test server as its
  only catalog entry, and one static and one dynamic sub-gateway minted
  through `POST /gateways`.
- **Clients:** a go-sdk probe client (sends a progress token, can cancel);
  `curl` for the raw SSE stream; the harness's own Claude Code binary in
  `-p` mode with `--mcp-config` pointing at the sub-gateway; and
  `claude-agent-acp` driven over stdio by a minimal ACP client, which is the
  path orch uses.
- **Sandbox proxy:** a raw socket held open and silent through the sbx HTTP
  proxy (`gateway.docker.internal:3128`) to the real host gateway on `:3333`.

The spike code is not committed (no production code in this step).

### Results

| Question | Answer |
|---|---|
| Harness Claude Code version | **2.1.293** (`claude-agent-sdk` 0.3.293, `@agentclientprotocol/claude-agent-acp` 0.88.0 inside image `slimslenderslacks/claude-code-acp:0.90.0`). Older than 2.1.304, so **no MCP Tasks**. |
| Protocol negotiated | Claude Code → gateway: `2025-11-25`. Gateway → backend: `2025-11-25` (pinned, `backendProtocolVersion` in `codemode`). |
| `_meta.progressToken` forwarding | **Dropped.** Claude Code sends a token (a direct call arrives with `progressToken=2`), the go-sdk probe sends `probe-tok-1`, but through the gateway the backend always sees no token. `codemode` builds a fresh `CallToolParams{Name, Arguments}` for the backend (`gateway.go` per-tool handler, `mcp_exec.go`), so `_meta` is lost. |
| `notifications/progress` relay | **None.** With no token the backend cannot send progress. Direct to the backend, progress reaches both the probe and Claude Code. post-office's 15 s heartbeats are therefore never sent in production; this is harmless (§4.6 already treats them as best effort). |
| Client cancel propagation | **Reaches a stateful backend, not a stateless one.** A go-sdk client cancel, a Claude Code `MCP_TOOL_TIMEOUT`, and an ACP `session/cancel` all cancel the backend handler's context within milliseconds when the backend is stateful. With a **stateless** backend (post-office), the handler runs to completion: go-sdk's stateless handler only ties the handler to the HTTP request on protocol ≥ 2026-07-28 (`PropagateRequestCancellation` docs), and the gateway speaks 2025-11-25. So a cancelled `wait_for_response` keeps blocking in post-office until `max_wait` and its result is discarded. The request itself is unaffected (it is durable and only `cancel_request` cancels it). |
| Gateway per-call timeout | **None found, none observed.** Calls of 300 s and 420 s pass through unchanged. The only gateway timeouts are backend attach (60 s) and catalog resolve (30 s); the backend HTTP transport has dial and keep-alive timeouts but no response timeout. |
| First-byte limit (60 s) | **Not binding through the gateway.** On a 2025-11-25 session the gateway's go-sdk handler (which has an `EventStore`) writes an SSE `event: prime` frame the moment the POST arrives, before the backend answers, and that frame counts as Claude Code's first byte. go-sdk writes it only when the handler has an event store and the session's protocol is ≥ 2025-11-25 and < 2026-07-28 (`mcp/streamable.go`, SEP-1699). Direct to a go-sdk backend with no event store, a silent 92 s call fails at 60 s (`The operation timed out`), and the same call with progress every 15 s succeeds. |
| Code mode on sub-gateways | **Not in play.** A static sub-gateway lists the backend's tool under its own name (`block`) with its schema unchanged, and returns its content unchanged. A dynamic sub-gateway adds `mcp-find` and `mcp-add`; there is no `mcp-exec` or code-mode tool. Text-first results reach the model verbatim. |
| `X-Codemode-Gateway-Id` | Arrives on every backend call through a sub-gateway; absent on direct calls. |
| Auto-backgrounding (ACP) | **Not triggered.** 150 s and 240 s calls through `claude-agent-acp` returned inline in the same turn (`stopReason: end_turn`). While a call runs, the adapter emits a `tool_call_update` with `elapsedTimeSeconds` every 30 s, which counts as ACP activity for orch's 20 min stream-idle reaper (`internal/daemon/reaper.go`). |
| sbx proxy idle | A request held silent for 130 s and 330 s through the sbx proxy to the host gateway was not dropped; the response arrived as soon as the request completed. |

**Blocking calls through the gateway, by duration** (`p` = backend asked to
send progress every 15 s, which the gateway drops anyway):

| Call | Path | Result |
|---|---|---|
| 50 s | gateway, Claude Code `-p` | ok (54 s wall) |
| 90 s, 91 s p, 120 s | gateway, Claude Code `-p` | ok |
| 150 s, 240 s | gateway, `claude-agent-acp` | ok, inline |
| 300 s p | gateway, Claude Code `-p` | ok, at the limit (result at 300.0 s) |
| 330 s, 330 s p | gateway, Claude Code `-p` | **fail at 300 s**: `sent no response or progress for 300s; aborting` |
| 330 s, 420 s | gateway, `CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT=0` | ok |
| 90 s | gateway, `MCP_TOOL_TIMEOUT=20000` | fail at 20 s; backend cancelled (stateful) |
| 92 s | direct, no progress | **fail at 60 s** (first-byte timer) |
| 93 s p, 301 s p | direct, progress | ok |

**The binding limit is Claude Code's 300 s tool idle timeout**
(`CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT`). Progress would reset it, but the
gateway drops the progress token, so nothing post-office can do extends it.
The 60 s first-byte timer is satisfied by the gateway's `prime` event, and
neither the gateway nor the sbx proxy imposes a shorter limit.

### Recommendation: `max_wait` = 240 s

Run post-office with **`--max-wait 240s`**, leaving 60 s of margin under the
300 s idle timeout for gateway and scheduling latency. Compared with 50 s it
cuts an agent's `wait_for_response` loop, and the model turns it costs, by
about 5×, and nothing in the measured chain breaks.

Follow-ups for whoever changes the default (this step changes no code):

1. `DefaultMaxWait` and the `defaultWait` schema default (45 s) in
   `internal/postoffice/mcpserver/server.go`; the tool descriptions take the
   value from `Config.MaxWait` already. The defaults should be close to
   `max_wait` (for example `max_wait − 10 s`), or agents will keep looping
   at 45 s.
2. Shutdown: `post-office serve` gives in-flight calls `max_wait + 5 s`, but
   the launchd plist's `ExitTimeOut` is 60 s, so launchd kills it first.
   That is safe (the requests are durable and the agent's next call picks
   up) but `ExitTimeOut` should be raised, or the grace capped, so the log
   is not misleading.
3. Do **not** set `CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT=0` in the sandbox to go
   higher. acp-kit registers one server for the whole gateway, so it would
   remove the hang guard from every tool.

**What would invalidate this**, so re-measure when any of these change:

- The gateway stops sending the `prime` event (moves agents to protocol
  2026-07-28, or drops its `EventStore`). The 60 s first-byte timer then
  applies again, and `max_wait` must go back to 50 s unless progress is
  forwarded.
- The gateway starts forwarding `_meta.progressToken`. post-office's
  heartbeats would then reset the idle timer, and `max_wait` could go well
  past 300 s.
- The harness moves to Claude Code 2.1.304 or later. Tasks become possible
  (once go-sdk and the gateway support them), and auto-backgrounding may
  apply to long calls.

### Still open

- **go-sdk SEP-2663 timing** (go-sdk issue #942/#626), for the Tasks upgrade.
- **Gateway fixes worth asking for:** forward `_meta` (at least
  `progressToken`) and relay `notifications/progress` to the calling agent
  session; forward cancellation to stateless backends, or have post-office
  run stateful.

## Sources

**Spec**
- Lifecycle and timeouts (2025-06-18): https://modelcontextprotocol.io/specification/2025-06-18/basic/lifecycle
- Lifecycle and timeouts (2025-11-25): https://modelcontextprotocol.io/specification/2025-11-25/basic/lifecycle
- Cancellation and timeouts (2026-07-28): https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/cancellation
- Progress (2025-11-25): https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/progress
- Progress (2026-07-28): https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/progress
- Cancellation (2025-11-25): https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/cancellation
- Tasks (2025-11-25): https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/tasks
- Tasks extension: https://modelcontextprotocol.io/extensions/tasks/overview and https://github.com/modelcontextprotocol/ext-tasks
- Extension client matrix: https://modelcontextprotocol.io/extensions/client-matrix
- Elicitation (2025-11-25): https://modelcontextprotocol.io/specification/2025-11-25/client/elicitation
- MRTR (2026-07-28): https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/mrtr
- Resources and subscriptions (2025-11-25): https://modelcontextprotocol.io/specification/2025-11-25/server/resources
- Subscriptions (2026-07-28): https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/subscriptions
- 2026-07-28 changelog: https://modelcontextprotocol.io/specification/2026-07-28/changelog
- Draft changelog: https://modelcontextprotocol.io/specification/draft/changelog

**SEPs**
- SEP-1391: https://github.com/modelcontextprotocol/modelcontextprotocol/issues/1391
- SEP-1686: https://github.com/modelcontextprotocol/modelcontextprotocol/issues/1686
- SEP-2663: https://github.com/modelcontextprotocol/modelcontextprotocol/pull/2663

**Claude Code**
- MCP docs: https://code.claude.com/docs/en/mcp
- Environment variables: https://code.claude.com/docs/en/env-vars
- Changelog: https://github.com/anthropics/claude-code/blob/main/CHANGELOG.md
- ACP adapter: https://github.com/agentclientprotocol/claude-agent-acp (`src/mcp-servers.ts`, `src/elicitation.ts`)

**Go SDKs**
- go-sdk releases: https://github.com/modelcontextprotocol/go-sdk/releases
- go-sdk protocol docs: https://github.com/modelcontextprotocol/go-sdk/blob/main/docs/protocol.md
- go-sdk issues: #626, #942
- mcp-go: https://github.com/mark3labs/mcp-go (task docs at `www/docs/pages/servers/tasks.mdx`)
- Local: `standalone-codemode/go.mod`, `workingman/internal/mcpgateway/session.go`,
  `workingman/docs/mcp-gateway.md`, `standalone-codemode/docs/catalog.md`
