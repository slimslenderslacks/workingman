# Agent messaging: design and implementation plan

**Status: approved 2026-11-20**, with the operator's decisions recorded in
[§10.3](#103-decisions) and applied to the sections below. Nothing here is
implemented yet; the tasks in [§9](#9-implementation-task-breakdown) are
queued.

This document turns the project brief and three research notes into one plan:

- [research/mcp-async-requests.md](research/mcp-async-requests.md): the MCP
  spec, Claude Code, the Go SDKs, the tool surface, hosting and identity.
- [research/orch-integration.md](research/orch-integration.md): where each
  piece lands in orch, with `file:line` references.
- [research/slack-mcp.md](research/slack-mcp.md): what the Slack MCP behind
  the standalone-codemode gateway can and cannot do.

Where this document makes a decision, it gives the reason in one line and
links to the research for detail. Code references are relative to the
`workingman` repo unless prefixed `standalone-codemode/`.

Contents: [1. Overview](#1-overview) · [2. Message model](#2-message-model) ·
[3. On-disk layout](#3-on-disk-layout) ·
[4. post-office MCP server](#4-post-office-mcp-server) ·
[5. Delivery and fan-out](#5-delivery-and-fan-out) ·
[6. Slack channel](#6-slack-channel) · [7. TUI inbox](#7-tui-inbox) ·
[8. Step-Up agent](#8-step-up-agent) ·
[9. Task breakdown](#9-implementation-task-breakdown) ·
[10. Risks and open questions](#10-risks-and-open-questions)

---

## 1. Overview

```
 sandboxed agent (claude via ACP)
        │  tools/call send_request …
        ▼
 standalone-codemode  /gateways/<id>          injects X-Codemode-Gateway-Id
        │
        ▼
 post-office  (workingman/cmd/post-office, 127.0.0.1:7077/mcp)
        │  writes inbox/<id>.json, watches outbox/<id>.json
        ▼
 ~/.workingman/post-office/{inbox,outbox,done,deliveries}/
        ▲                  ▲                         ▲
        │                  │                         │
   orch TUI           orch daemon Dispatcher     Step-Up agent
   (inbox view,       (fan-out to channels:      (via post-office
    writes answers)    Slack, Signal, WhatsApp;   next_message/reply)
                       "answered elsewhere")
```

The pieces:

- **post-office** is a standalone Go HTTP MCP server in the workingman repo.
  It is the only component agents talk to. It turns tool calls into durable
  message files and turns answer files back into tool results.
- **The message store** is a directory tree under `~/.workingman/post-office/`,
  shared by three processes (post-office, the orch daemon and the TUI). It is
  the only integration point between them. There is no RPC.
- **The Dispatcher** runs inside the orch daemon. It fans operator-addressed
  messages out to every active channel and tells the other channels when one
  of them has answered.
- **The Slack channel** is a new `internal/channels` implementation that
  talks to the Slack MCP through a daemon-owned sub-gateway, and polls for
  replies.
- **The TUI inbox** reads the store directly, so it works with or without a
  daemon and with or without channels.
- **Step-Up** is a new always-on agent that receives requests from other
  agents, can ask the operator for approval through post-office, and asks the
  daemon to apply changes through a narrow, audited request directory.

Design rules that apply everywhere:

1. **A message is a durable file, not a tool call.** Tool calls only observe
   it. A dropped connection, a restarted gateway or a compacted agent context
   never loses or withdraws a request.
2. **One claim primitive.** Every terminal transition of a request (answered,
   cancelled, expired, dismissed) is an `O_CREAT|O_EXCL` create of
   `outbox/<id>.json`. Whoever creates it first wins. Everyone else gets
   `EEXIST` and reads the winner.
3. **One writer per file.** Each directory has exactly one owning writer
   (§3.2), apart from the outbox claim, where the exclusive create makes it
   safe to have several.
4. **The sender is never a parameter.** It is derived from the gateway that
   carried the call.

## 2. Message model

### 2.1 Kinds

| Kind | Reply expected | Lifetime | Terminal states |
|---|---|---|---|
| `notification` | No. Recipients may act on it. | Until dismissed (TUI) or expired (default 7 days). | `dismissed`, `expired` |
| `request` | Yes. The sender may wait, poll, or give up. | Until answered, cancelled by the sender, or expired (default 24 h). | `answered`, `cancelled`, `expired` |

### 2.2 Identifiers

- `id` is `msg_` + a ULID, for example `msg_01JD3Z8Q4X7V2K9P0M5N6R1T8W`. One
  prefix for both kinds, because they share directories and tooling. ULIDs
  sort by creation time, which gives the TUI and `list_requests` ordering for
  free.
- The tools call it `request_id` for requests and `message_id` for
  notifications. It is the same value.
- A **short id** for humans is the last 6 characters, lower-cased (`[req
  r1t8w]`). It appears in channel text so the operator can refer to a request
  in a top-level reply (§5.4). The full id is used everywhere else. Short ids
  are matched only against *open* requests, where a collision is very
  unlikely; on a collision, the reply is rejected with a request to reply in
  the thread instead.

### 2.3 Sender identity

`from` is filled in by post-office from the caller's gateway id (§4.3) and is
never taken from tool arguments:

```json
"from": {
  "agent": "task",                       // agent.Kind string: task, wolf, workingman, step-up, …
  "sandbox": "wm-workingman-messaging-design-messaging-plan",
  "project": "/Users/…/orch/workingman-messaging/project.yaml",
  "task": "design-messaging-plan",       // empty for agents without a task
  "session_id": "…"                      // the session.json that was live at send time
}
```

Rendered for humans as `task design-messaging-plan (workingman-messaging)`, or
`step-up`, or `workingman agent`.

### 2.4 Recipient

- Empty, omitted, or `"operator"` means the **human operator**. The message
  goes to the TUI inbox and every active channel (§5).
- Anything else must name a **registered agent recipient**. v1 has exactly
  one: `"step-up"`. The registry is a small static map in post-office
  (`recipient name → sandbox name`), not a free-form address, so a typo
  is rejected (`status: rejected`, `reason: unknown recipient "stepup"; known:
  operator, step-up`) instead of creating an unreadable message.
- Agent recipients receive messages through `next_message` (§4.5). They do
  **not** go to channels. They do appear in the TUI inbox as read-only rows
  under a "To agents" heading, so the operator can see agent-to-agent traffic
  (open question Q6).

### 2.5 Timeout and expiry

There are two separate clocks, and keeping them apart is the point:

- **Wait time** (`wait_seconds`, `timeout_seconds`) is how long one *tool
  call* blocks. It is capped at `max_wait` (default 50 s, §4.6). When it runs
  out, the call returns `status: pending` and nothing happens to the request.
- **Expiry** (`expires_in_minutes`) is the request's TTL. The default is 1440
  (24 h), the minimum 1 and the maximum 10080 (7 days). When it passes,
  post-office claims the outbox with `outcome: expired`, and everyone sees the
  request close.

Expiry is enforced by post-office: a ticker every 30 s, plus a sweep on
startup. If post-office is down, a request can outlive its expiry until
post-office comes back. Readers (TUI, Dispatcher) treat an entry whose
`expires_at` is in the past as expired for display purposes, even before
post-office claims it, so nothing looks answerable when it is not.

### 2.6 Cancellation by the sender

`cancel_request(request_id, reason?)`:

- Only the original sender may cancel. post-office compares the caller's
  identity with `from.sandbox` (and `from.task` when set).
- It attempts the outbox claim with `outcome: cancelled`. If an answer won the
  race, the call returns `status: answered` with the answer, as the tool
  description promises.
- Channels then show "⌛ no longer needed", and the TUI row disappears (§5.3).
- A cancelled or timed-out **tool call** (`wait_for_response` interrupted by
  the client) never cancels the request. That is the rule from
  mcp-async-requests.md §1.

### 2.7 Response format

An answer is free text plus metadata:

```json
// outbox/<id>.json
{
  "id": "msg_01JD…",
  "outcome": "answered",              // answered | cancelled | expired | dismissed
  "response": "Use the staging bucket, not prod.",
  "option": "",                        // set when the answer matched one of the request's options
  "responder": {"kind": "operator", "via": "slack", "name": "Jim Clark"},
                                       // kind: operator | agent; via: tui | slack | signal | whatsapp | step-up
  "reason": "",                        // for cancelled: the sender's reason
  "at": "2026-11-20T10:42:03Z"
}
```

- If the request had `options`, an answer that matches one (case-insensitive,
  trimmed, or by its 1-based number) sets `option` to the canonical option
  text. `response` keeps what the human actually typed. Non-matching answers
  are still accepted: options are suggestions, not a constraint.
- Answers are capped at 16 KiB. Longer answers are truncated with a marker.

### 2.8 States

```
           send_request
                │
                ▼
            pending ───────────────┬───────────────┬──────────────┐
                │                  │               │              │
   answer claims outbox   sender cancel_request   expires_at    (notification only)
                │                  │               │           TUI dismiss
                ▼                  ▼               ▼              ▼
            answered           cancelled        expired       dismissed
```

The record also carries a non-terminal **delivery** status for display only:
`delivered_to: ["tui", "slack", …]` from the Dispatcher's deliveries file, and
`received_at` when an agent recipient has taken the message with
`next_message`. These never block a terminal transition.

### 2.9 The inbox record

```json
// inbox/<id>.json — written only by post-office
{
  "version": 1,
  "id": "msg_01JD3Z8Q4X7V2K9P0M5N6R1T8W",
  "kind": "request",                    // request | notification
  "from": { … },                        // §2.3
  "recipient": "operator",              // normalised: operator | step-up
  "subject": "Which bucket for the export?",
  "body": "…markdown…",
  "options": ["staging", "prod"],       // optional
  "created_at": "2026-11-20T10:30:00Z",
  "expires_at": "2026-11-21T10:30:00Z",
  "received_at": null,                  // set when an agent recipient took it via next_message
  "in_reply_to": ""                     // optional: links a Step-Up approval request to the agent request it serves (§8.4)
}
```

## 3. On-disk layout

### 3.1 Location

`~/.workingman/post-office/`, derived from the sessions root the way
`mcpgateway.StateDir` does (`internal/mcpgateway/store.go:48-50`). It is
overridable with `$WORKINGMAN_POST_OFFICE_DIR` for tests.

The brief says "an inbox directory in orch". This is orch's host state
directory, next to `sessions/`, `gateways/` and `state/`. It is deliberately
*not* inside an orch root, because the daemon's root watcher reacts to writes
there and the workingman agent has every root mounted read-write
(orch-integration.md §2.3).

```
~/.workingman/post-office/
├── inbox/
│   ├── msg_01JD….json          open messages (requests and notifications)
│   └── .tmp/                   write-then-rename staging
├── outbox/
│   └── msg_01JD….json          terminal claim; exists only briefly
├── done/
│   └── 2026-11-20/
│       └── msg_01JD….json      inbox record + resolution, kept 30 days
├── deliveries/
│   └── msg_01JD….json          Dispatcher's per-channel send records
└── post-office.lock            flock held by the running post-office
```

Permissions: directories 0700, files 0600, matching `channel-conversations.json`.

### 3.2 Who owns which files

| Path | Writer | Readers | How |
|---|---|---|---|
| `inbox/<id>.json` | **post-office only** | TUI, Dispatcher, post-office | write `.tmp/<id>.<rand>`, `fsync`, `rename` |
| `outbox/<id>.json` | **first claimer**: TUI, Dispatcher (on behalf of a channel), post-office (cancel, expire, agent `reply`) | post-office | write the full JSON to `.tmp`, then `link(2)` it to `outbox/<id>.json`, which fails with `EEXIST` if it exists (an atomic exclusive create of a complete file); then remove the temp |
| `done/<day>/<id>.json` | **post-office only** | TUI (history), Dispatcher, Step-Up executor (approval check, §8.4) | write-then-rename |
| `deliveries/<id>.json` | **Dispatcher only** | TUI (shows "sent to slack, signal"), Dispatcher | write-then-rename |

Why `link(2)` and not `open(O_EXCL)` followed by `write`: with `O_EXCL` a
reader can see an empty or partial file between the create and the write.
`link` publishes a complete file atomically and still fails if the name
exists. Both directories are on the same filesystem, so this is safe.

### 3.3 Finalisation

post-office watches `outbox/` (fsnotify, with a 2 s poll as fallback). For
each claim it:

1. reads the claim and the inbox record;
2. writes `done/<day>/<id>.json` with `{…record, "resolution": {…claim}}`;
3. removes `inbox/<id>.json`, then `outbox/<id>.json`;
4. wakes any `wait_for_response` blocked on that id.

Every step is idempotent, and post-office re-runs this for every leftover
outbox file on startup, so a crash between steps converges. "Remove the
message from the TUI inbox" is simply "the file left `inbox/`".

A claim whose id has no inbox record (already finalised, or garbage) is moved
to `done/` as an orphan and logged.

### 3.4 Retention

- `done/` is pruned after 30 days by post-office.
- `deliveries/` entries are pruned by the Dispatcher once the matching `done/`
  record is older than 30 days.
- `inbox/` is never pruned. Expiry moves entries out.

## 4. post-office MCP server

### 4.1 Hosting

- **Code:** `internal/postoffice` holds the store and the model, with role
  `shared`. The identity resolver, the capability check and the tool handlers
  are in `internal/postoffice/mcpserver` (role `daemon`), so that the TUI and
  the daemon, which link `internal/postoffice`, do not pull in the MCP SDK.
  `cmd/post-office/main.go` is a thin `main`.
- **SDK:** `github.com/modelcontextprotocol/go-sdk`, at the version
  standalone-codemode pins. This is a new dependency for workingman, whose
  only MCP code today is the hand-rolled client in `internal/mcpgateway`.
  Reason: progress, cancellation and request headers are all supported, and
  the Tasks extension can be added on top later (mcp-async-requests.md §3).
- **Transport:** streamable HTTP on `127.0.0.1:7077/mcp` (`--listen`), in
  stateless mode, so no session state lives in the server.
- **Process:** a launchd plist next to the existing ones, plus a Taskfile
  build task. A `post-office.lock` flock stops two copies running at once.
- **Auth:** the server requires `Authorization: Bearer <token>`. The token is
  generated by `post-office setup` into
  `~/.workingman/post-office/token` (0600) and referenced by the catalog entry
  in standalone-codemode as `${post_office_token}`. This stops other local
  processes from posting as a gateway.
- **Why not inside the orch daemon:** post-office must keep serving long
  polls while orch restarts, and must not depend on the daemon at all for the
  TUI-only case (mcp-async-requests.md §6, option A). Keeping the logic in
  `internal/` means orch could embed it later.

### 4.2 Granting it only to agents with the messaging capability

The **capability is the gateway being minted with `post-office`**:

1. **Opt-in.** A task gets `static_mcps: [post-office]` (`internal/task/task.go:206`).
   The Step-Up plan hard-codes it, as review hard-codes `github`
   (`internal/daemon/review.go:246`).
2. **Never** add it to the `--mcp-gateway-servers` default.
3. **Close the dynamic gap.** Dynamic gateways (Task, Wolf and Workingman by
   default, `internal/runner/runner.go:1280-1288`) can `mcp-add` any catalog
   server. So post-office checks every call: the caller's gateway record
   (`~/.workingman/gateways/<sandbox>.json`, `Record.Servers`) must list
   `post-office`. `Record.Servers` holds only what the gateway was *minted*
   with, so `mcp-add` cannot satisfy it.
4. **Optional hardening** in standalone-codemode: a catalog flag
   `"dynamic_addable": false` on the `post-office` entry, so `mcp-add` refuses
   it outright. This is defence in depth; step 3 is the actual check.
5. The **planning agent's prompt** gets one paragraph saying when to add
   `post-office` to a task's `static_mcps`: when the task description says the
   agent may need to ask the operator or another agent something.

A rejected caller gets a JSON-RPC error on `tools/call` (and on
`resources/read`): `post-office is not enabled for this sandbox (add
static_mcps: [post-office] to the task)`. `tools/list` is refused only when
the caller's gateway record exists and lacks `post-office`, which is the
`mcp-add` case. A sub-gateway attaches its backends, and so lists their tools,
while `POST /gateways` is still running. That is before workingman saves the
record (after `sbx create`) and before any session exists, so refusing an
unknown id there would remove post-office from every sandbox. Listing tools
reveals nothing; every call is checked in full.

### 4.3 Caller identity

1. **standalone-codemode** injects `X-Codemode-Gateway-Id: <sub-gateway id>`
   on every backend request it makes for a sub-gateway. That means generating
   the UUID before `Build` (`aggregategw/registry.go:74-78`) and adding the
   header in `Manager.attachSpec` (`aggregategw/manager.go:762-779`) and on
   the dynamic path (`:399-402`). Calls through the parent `/mcp` carry no
   id. This is the only cross-repo change post-office needs.
2. **post-office** resolves the id:
   - `mcpgateway.Store.List()` → the record with that `id` → `{sandbox,
     servers}`;
   - `sandbox` → the singleton agents by fixed sandbox name (`workingman-agent`,
     `sleepy-head`, `step-up`), otherwise the newest **live** `session.json`
     under `~/.workingman/sessions/*` with that `sandbox_name`, which gives
     `kind`, `project_path` and `task_path`
     (`internal/session/session.go:52-117`).
   - The result is cached per gateway id for 30 s.
3. **Rejected:** no header, unknown id, a record without `post-office`, or a
   sandbox with no live session. Each is a JSON-RPC error with a precise
   message, and each is logged.

The agent cannot spoof this: it never talks to post-office directly, and the
id is the gateway's own.

### 4.4 Sender tools

All tools return a **text result whose first line is `status: <state>`**,
followed by `key: value` lines, so the model can branch on it even if the
gateway or code mode flattens structured content. The same fields are also in
`structuredContent`. Schemas below are the JSON Schema `inputSchema`.

#### `send_notification`

```json
{
  "type": "object",
  "properties": {
    "subject":   {"type": "string", "maxLength": 200, "description": "One-line summary shown in inbox lists and notifications."},
    "body":      {"type": "string", "maxLength": 16000, "description": "The message, in markdown."},
    "recipient": {"type": "string", "description": "Who to notify. Omit for the human operator, or give an agent name such as \"step-up\"."}
  },
  "required": ["subject", "body"]
}
```

Result: `status: sent`, `message_id`. Or `status: rejected`, `reason`.

Description:

> Send a one-way notification. No reply is expected and this call returns
> immediately. Use it for status updates, FYIs, or to say that you took an
> action someone should know about. `recipient` defaults to the human
> operator; set it to an agent name (for example `"step-up"`) to notify that
> agent instead. Do not use this when you need an answer — use `send_request`.

#### `send_request`

```json
{
  "type": "object",
  "properties": {
    "body":               {"type": "string", "maxLength": 16000, "description": "The question, with enough context for the recipient to answer without seeing your work."},
    "subject":            {"type": "string", "maxLength": 200, "description": "One-line summary. Defaults to the first line of body."},
    "recipient":          {"type": "string", "description": "Omit for the human operator, or give an agent name such as \"step-up\"."},
    "options":            {"type": "array", "items": {"type": "string", "maxLength": 100}, "maxItems": 10, "description": "Optional suggested answers. The recipient may still answer freely."},
    "wait_seconds":       {"type": "integer", "minimum": 0, "maximum": 50, "default": 45, "description": "How long this call waits for an answer before returning pending."},
    "expires_in_minutes": {"type": "integer", "minimum": 1, "maximum": 10080, "default": 1440, "description": "How long the request stays open before it expires."}
  },
  "required": ["body"]
}
```

Results:

```
status: answered            status: pending                 status: rejected
request_id: msg_…           request_id: msg_…               reason: unknown recipient "stepup"; known: operator, step-up
responder: operator         expires_at: 2026-11-21T10:30Z
via: slack                  delivered_to: tui, slack
option: staging
response:
Use the staging bucket.
```

`response` is always last, so multi-line answers need no escaping.

Description:

> Ask a question that needs an answer before you can continue: missing
> information, an approval, or help. `recipient` defaults to the human
> operator; set it to an agent name (for example `"step-up"`) to ask that
> agent.
>
> This call waits up to `wait_seconds` (max 50) for an answer.
> - If the first line of the result is `status: answered`, the reply is in
>   `response`.
> - If it is `status: pending`, the question has been delivered and is still
>   open. Keep the `request_id`. The answer may take minutes or hours.
>
> **To wait for the answer**, call `wait_for_response` with the `request_id`.
> Each call waits up to about 50 seconds. If it returns `status: pending`,
> call it again, for as long as you are willing to wait. If you can make
> progress on other work meanwhile, do that and call `check_response`
> occasionally instead.
>
> **If you decide to stop waiting**, call `cancel_request` with the
> `request_id`, so the recipient knows you no longer need an answer. Then
> continue with your best judgement and say in your output that the request
> went unanswered.
>
> The request expires automatically after `expires_in_minutes` (default
> 24 h). Do not send the same question twice; reuse the `request_id` you
> have. If you lose it, call `list_requests`.

#### `wait_for_response`

```json
{
  "type": "object",
  "properties": {
    "request_id":      {"type": "string", "description": "The request_id returned by send_request."},
    "timeout_seconds": {"type": "integer", "minimum": 1, "maximum": 50, "default": 45}
  },
  "required": ["request_id"]
}
```

Result: `status: answered | pending | cancelled | expired`, with `request_id`,
`elapsed` (since creation), `expires_at`, and on `answered` the `responder`,
`via`, `option` and `response` fields.

Description:

> Wait up to `timeout_seconds` (max 50) for the answer to a request you made
> with `send_request`.
> - `status: answered`: the reply is in `response`.
> - `status: pending`: no answer yet. Call `wait_for_response` again to keep
>   waiting, or `cancel_request` to give up.
> - `status: expired` or `status: cancelled`: no answer will come. Continue
>   without it.
>
> If this call is interrupted or times out, the request is **not** cancelled.
> Just call `wait_for_response` again.

#### `check_response`

Same input as `wait_for_response` without `timeout_seconds`, and the same
result. Never blocks.

> Check, without waiting, whether a request made with `send_request` has been
> answered. Use it between other work; use `wait_for_response` when you have
> nothing else to do.

#### `cancel_request`

```json
{
  "type": "object",
  "properties": {
    "request_id": {"type": "string"},
    "reason":     {"type": "string", "maxLength": 500, "description": "Shown to the recipient, e.g. \"found the answer in the docs\"."}
  },
  "required": ["request_id"]
}
```

Result: `status: cancelled`, or `status: answered` with the answer if one had
already won.

> Stop waiting for a request made with `send_request`. The recipient is told
> you no longer need an answer, and the request leaves the operator's inbox.
> If an answer had already arrived, this returns it with `status: answered`
> instead.

#### `list_requests`

```json
{
  "type": "object",
  "properties": {
    "status": {"type": "string", "enum": ["open", "closed", "all"], "default": "open"}
  }
}
```

Result: `status: ok`, then one line per request: `<request_id> <state>
<recipient> <created_at> <subject>`. Scoped to the caller (same sandbox and
task); closed requests come from `done/`, last 7 days, at most 50.

> List the requests you have sent, so you can recover a `request_id` you have
> lost track of (for example after your context was summarised).

Ownership checks: `wait_for_response`, `check_response`, `cancel_request` and
`list_requests` only see requests whose `from` matches the caller. Another
agent's id gives `status: rejected`, `reason: not your request`.

### 4.5 Receiver tools (agent recipients)

These are listed by `tools/list` **only** for callers whose sandbox is a
registered recipient (v1: Step-Up). Other callers do not see them.

**As built** (`internal/postoffice/mcpserver/receiver.go`): the registry is
`Config.Recipients` (`DefaultRecipients`: `step-up` → sandbox `step-up`).
Every receiver call is checked against it, and a non-recipient gets `status:
rejected`. `tools/list` hides the receiver tools and `send_request`'s
`in_reply_to` from callers post-office has **identified** as non-recipients.
A caller it cannot identify yet sees the full list. That covers every
gateway while standalone-codemode attaches post-office during `POST
/gateways`, before workingman saves the record (§4.2). The gateway keeps the
tool list it gets at attach time and only re-lists on reconnect or on a
`list_changed` notification, which a stateless server cannot send. Filtering
unidentified callers would therefore hide `next_message` from Step-Up for
good. In practice every gateway lists the receiver tools, each description
says they are for recipients only, and the call-time check is the
boundary. If that turns out to confuse task agents, there are two ways to
hide them properly: the gateway re-lists a backend after `POST /gateways`
returns, or workingman saves the record before it mints the gateway.

A received notification needs no reply, so `next_message` closes it as
`dismissed` (`responder: {agent, step-up}`) as soon as it hands it out.
Otherwise it would stay in `list_inbox` and the TUI's "To agents" rows until
it expired. `received_at` is set with a compare-and-set
(`Store.MarkReceived` returns `ErrAlreadyReceived`), so concurrent
`next_message` calls and a post-office restart never hand out a message
twice. `wait_for_response` and `check_response` show `received_at` to the
sender while the request is pending.

`send_request` from a recipient takes `in_reply_to`. When the argument is
omitted and the recipient holds exactly one received, unanswered request,
post-office sets it to that request and says so in a `note:` line. With
several held requests the call is rejected until it names one. An explicit
value must name an open request addressed to the caller. Non-recipients
that pass it are rejected. A recipient cannot send to itself.

#### `next_message`

```json
{
  "type": "object",
  "properties": {
    "timeout_seconds": {"type": "integer", "minimum": 0, "maximum": 50, "default": 45}
  }
}
```

Returns the oldest open message addressed to the caller that it has not yet
received, and sets `received_at` on it. Otherwise it waits up to
`timeout_seconds` and returns `status: empty`.

```
status: message
kind: request
request_id: msg_…
from: task fix-ci (workingman-messaging)
from_sandbox: wm-workingman-messaging-fix-ci
expires_at: …
subject: Need network access to proxy.golang.org
body:
…
```

> Fetch the next message addressed to you. Waits up to `timeout_seconds` (max
> 50) and returns `status: empty` if nothing arrives; call it again to keep
> listening. Each message is returned once. A `kind: request` must be answered
> with `reply`, or it will expire and the sender will carry on without you.
> Use `list_inbox` to see messages you have received but not yet answered.

#### `reply`

```json
{
  "type": "object",
  "properties": {
    "request_id": {"type": "string"},
    "response":   {"type": "string", "maxLength": 16000}
  },
  "required": ["request_id", "response"]
}
```

Claims the outbox with `responder: {kind: agent, via: step-up}`. Result:
`status: answered`, or `status: closed` with the outcome that won (the sender
cancelled, it expired, or — for requests visible to the operator too — the
operator answered first).

> Answer a request you received from `next_message`. The sender's
> `wait_for_response` returns your `response`. If the request was already
> closed (cancelled by the sender, or expired), this tells you so.

#### `list_inbox`

No input. Lists open messages addressed to the caller, received or not.

> List the open messages addressed to you, including ones you have already
> received but not yet answered. Use it after a restart to pick up work in
> progress.

### 4.6 The async flow, and why

The research found that the spec's answer to "a tool call that waits for a
human" is the **2026-07-28 Tasks extension**, but nothing in our chain can
serve it today: go-sdk has no task support, the gateway almost certainly does
not pass tasks through, and Claude Code only uses tasks for main-conversation
calls (mcp-async-requests.md §1-4). So v1 uses the fallback the research
recommends, and is designed to be correct with **no protocol support at
all**:

1. `send_request` writes the inbox record, then waits up to `wait_seconds` on
   an in-process notifier fed by the outbox watcher.
2. It returns `answered` or `pending` + `request_id`.
3. The agent loops on `wait_for_response` (≤ 50 s each) or polls
   `check_response`, and may `cancel_request` at any time.

The 50 s cap keeps each call under Claude Code's 60 s HTTP first-byte timer
and the gateway's own timeouts. `--max-wait` raises it once task
`verify-gateway-async-passthrough` (§9) shows that larger values survive.

**Heartbeats.** When a call blocks and carries `_meta.progressToken`,
post-office sends `notifications/progress` every 15 s with a message such as
`waiting for operator (12m since request)`. If the gateway drops them,
nothing breaks.

**Upgrade path.** When go-sdk implements SEP-2663 and the gateway forwards
tasks, `send_request` returns a `CreateTaskResult` with `taskId =
request_id`, and `tasks/cancel` maps to `cancel_request`. The handle tools
stay for clients without Tasks and for code mode. No change to the store.

### 4.7 Resources (debugging aid)

`post-office://messages/<id>` returns the inbox or done record as JSON, scoped
to the caller like the tools. Cheap to add, useful when debugging, not used by
any flow.

## 5. Delivery and fan-out

### 5.1 Who delivers what

| Recipient | No channels configured | Channels configured |
|---|---|---|
| operator | TUI inbox only | TUI inbox **and** every active channel |
| step-up (agent) | `next_message`, plus a read-only TUI row | same; never sent to channels |

"Active channel" means a channel the Registry started successfully, with a
route for the `post-office` topic. With no explicit `post-office` route,
`RoutesFor` falls back to the `*` routes (`internal/channels/registry.go:425-441`),
which is what "send to every channel" means in practice. A route can still be
added to send post-office traffic to a specific chat.

The TUI is not a channel; it reads the store directly, so the inbox entry
exists whether or not the daemon is running.

### 5.2 The Dispatcher

New code in `internal/postoffice/dispatch.go`, built by `cmd/orch/channels.go`
next to the router (`:124-140`) only when channels exist. It:

1. **Watches `inbox/`** (fsnotify plus a 5 s poll). For each operator-addressed
   record without a `deliveries/<id>.json`, it renders the outbound text
   (§5.5), calls `Registry.SendTopic(ctx, "post-office", text)`, and for each
   successful route:
   - calls `index.Register(route.Channel, messageID, ConversationTarget{Kind: "post-office", SessionKey: <msg id>})`,
     the same shape as `announceWolfStart` (`internal/daemon/channels_notify.go:127-169`);
   - records `{channel, chat, message_id, sent_at}` in `deliveries/<id>.json`.

   Failed routes are retried with backoff (30 s, 2 m, 10 m) and recorded as
   failed after that. The daemon restarting mid-send can cause at most one
   duplicate post per channel, which is acceptable.
2. **Watches `done/`.** For each request that has deliveries and no
   `closed_notified` marker, it sends a closing note to **every delivery
   except the one that answered** (`via` + chat), and records
   `closed_notified`. Notifications get no closing note (it would be noise).
3. On startup, it does both passes over existing files, so nothing is missed
   while the daemon was down.

### 5.3 First answer wins

All answer paths converge on `postoffice.Store.Claim(id, resolution)`:

| Path | Who calls `Claim` |
|---|---|
| TUI inbox reply | the TUI process |
| Reply on Slack, Signal, WhatsApp | the router, via the Dispatcher's store |
| Agent `reply` | post-office |
| Sender `cancel_request`, expiry | post-office |

`Claim` returns `ErrAlreadyClosed` with the winning resolution when it loses.
The loser shows "already answered via slack" (TUI status line, or a reply in
the channel). The winner's answer is the only one the sender ever sees.

Closing notes, sent as a reply to each delivery's message ID:

- answered elsewhere: `✅ Answered via <tui|slack|signal|…>: <first 200 chars>`
- cancelled: `⌛ No longer needed — <reason>`
- expired: `⌛ Expired — the agent stopped waiting`

The TUI needs no note: the inbox file disappears, so the row disappears.

To let channels do something better than a reply, add an optional interface
next to `Chunker` and `Typer` (`internal/channels/channel.go:67-75`):

```go
// Resolver lets a channel mark a previously sent message as closed. The
// Dispatcher uses it instead of a plain reply when available.
type Resolver interface {
    Resolve(ctx context.Context, chatID, messageID string, r Resolution) error
}
```

Slack implements it as a thread reply plus a reaction (§6.5). Signal and
WhatsApp use the default plain reply.

### 5.4 Router change

One new branch in `router.process` (`internal/router/router.go:388-400`):
when `Index.Lookup(channel, ReplyToID)` returns `Kind == "post-office"`, call
`Store.Claim(id, {outcome: answered, response: text, responder: {operator,
via: channel, name: SenderName}})` instead of `resolveReplyTarget`. Then
reply `✅ Sent to <sender>` on success, or `Already answered via <x>` on
`ErrAlreadyClosed`.

Two additions so the phone is usable:

- **Top-level replies with a short id.** A message starting with `#<short>`
  (for example `#r1t8w staging`) answers that request. This matters on
  transports where quoting is awkward.
- **`/inbox`** lists open operator requests with their short ids.

Add `ConversationKindPostOffice = "post-office"` next to
`internal/channels/conversation.go:15-21`.

### 5.5 Outbound rendering

```
❓ [req r1t8w] task fix-ci (workingman-messaging)
Which bucket for the export?

…body…

Options: 1) staging  2) prod
Reply to this message to answer. Expires in 24h.
```

Notifications use `📣` and omit the last two lines. Channels with a size limit
use `Chunker` as today. The body is markdown; Signal and WhatsApp get it
verbatim.

## 6. Slack channel

### 6.1 What we can rely on

From slack-mcp.md:

- `slack_send_message` posts (top level or `thread_ts` reply) and is the only
  tool with structured output, including the `ts`.
- `slack_read_thread(channel, message_ts, oldest)` returns the parent plus
  replies newer than `oldest` (exclusive). `slack_read_channel` lists
  top-level messages with a per-thread reply count.
- `slack_add_reaction` exists. **No edit, no delete, no reaction removal, no
  push of any kind.**
- **Posts are made as the operator's own user**, so author id cannot tell our
  posts from theirs.
- **Parallel calls fail** in the host header script; calls must be serialized.
- Read results are text in a "detailed" format that must be parsed.

### 6.2 Connection

The daemon reaches Slack the way it reaches GitHub for reviews:

1. **Generalise** `internal/daemon/review_gateway.go` into a reusable
   `mcpgateway.DaemonGateway{Key, Servers}` (mint with
   `Create(ctx, servers, false)`, record under `~/.workingman/gateways/daemon/`,
   re-mint with backoff on `ErrSessionLost`, delete on shutdown). Review
   switches to it with no behaviour change.
2. The Slack channel gets a `DaemonGateway{Key: "orch-slack", Servers:
   ["slack"]}` and one `mcpgateway.Session`.
3. Tool names are resolved from `tools/list` by suffix (`…slack_send_message`)
   because they arrive prefixed (`slack_xaa-`).
4. The channel package depends only on a small `ToolCaller` interface, so tests
   use a fake.

**One serialized worker** owns the session. `Send`, `Resolve` and polling all
go through a single queue. Header-script failures (`-32005 … header script
failed`) are retried with backoff (1 s, 2 s, 4 s, max 5 tries); the research
shows they fail before the request leaves the gateway, so retrying a send is
safe.

### 6.3 Outbound

- `Send` posts to the configured `channel` (or `OutboundMessage.ChatID`) with
  `thread_ts = ReplyToID` when set.
- It returns `"<channel_id>:<ts>"` as the message ID, so the conversation
  index key stays collision-free, and uses the **resolved** `channel_id` from
  the response (a user id resolves to a `D…` id).
- Every ts we post goes into a persisted **sent set**
  (`~/.workingman/slack/sent.json`, 30-day TTL, write-then-rename). That set is
  how inbound polling tells our posts from the human's.
- Requests (`Topic == "post-office"` and text starting with `❓`) are added to
  the **watch list** (`~/.workingman/slack/watch.json`): `{ts, opened_at,
  thread_cursor, reply_count}`.

### 6.4 Inbound: polling

No push exists, so `Start` runs a poll loop on the worker:

1. Every `poll_interval` (default 20 s), call `slack_read_channel(channel,
   oldest=<min(oldest watched ts, top-level cursor)>, limit=100,
   response_format=detailed)` and follow `cursor`.
   - A top-level message whose ts is not in the sent set is a fresh human
     message. Emit `InboundMessage{ChatID, MessageID: "<ch>:<ts>", Text}` with
     no `ReplyToID`. The router sends it to the workingman agent, as Signal
     does, or handles `#<short>` and `/inbox` (§5.4). Advance the top-level
     cursor.
   - A watched request whose `Thread: N replies` count rose goes to step 2.
2. `slack_read_thread(channel, message_ts=<request ts>, oldest=<thread
   cursor>)`. Each reply whose ts is not in the sent set is human: emit
   `InboundMessage{MessageID: "<ch>:<reply ts>", ReplyToID: "<ch>:<request
   ts>", Text}`. The conversation index routes it to post-office. Advance the
   thread cursor.
3. Stop watching a request when the Dispatcher calls `Resolve` on it, or after
   7 days.

Cost: one call per interval, plus one per thread with new activity. If the
`read_channel` parse ever finds no `Message TS:` lines in a non-empty
response, the channel logs loudly and falls back to calling `read_thread` per
watched request, which is fine for a handful of open requests.

The **parser** for the detailed text format lives in
`internal/channels/slack/parse.go`, with golden-file tests built from the
samples in slack-mcp.md §3.

**Access policy.** Because every message appears to come from the operator,
`allow_from` (Slack user ids) is checked but in practice admits only the
operator. In a shared channel, messages from anyone else are dropped and
logged, before the router sees them (the router does no authorisation,
`docs/channels.md:91-92`).

### 6.5 "Answered elsewhere" on Slack

`Resolve` (the §5.3 interface), since edits are impossible:

- Post a thread reply with the closing note, and add its ts to the sent set.
  No `reply_broadcast`.
- Add a reaction to the parent: `white_check_mark` for answered,
  `hourglass` for expired, `no_entry_sign` for cancelled. Visible in the
  channel view without opening the thread.
- A later human reply in a closed thread gets one bot reply: `This request is
  already closed (answered via tui).` It is not forwarded anywhere.

### 6.6 Configuration

```yaml
# ~/.workingman/channels.yaml
channels:
  slack:
    type: slack
    options:
      gateway_url: http://localhost:3333   # default
      channel: U0CC1K1MFNX                 # channel id, or your own user id for the DM-to-self
      allow_from: [U0CC1K1MFNX]            # Slack user ids; default deny
      poll_interval: 20s                   # min 5s
      watch_ttl: 168h
routes:
  - {topic: post-office, channel: slack, chat: U0CC1K1MFNX}
```

No `credentials`: the gateway's header script is the authentication.
Unknown option and credential names are rejected, as Signal does
(`internal/channels/signal/config.go:89-180`). `"slack"` is added to
`inboundTypes` (`internal/channels/config.go:316`), and `slack.Factory` to
`cmd/orch/channels.go:63-70`.

`orch slack setup|status|test`, modelled on `orch signal`:

- `setup` lists the user's channels (`slack_list_user_channels`, including
  `im`) and writes the `channels.slack` block and route with the existing
  `yaml.Node` editor;
- `status` checks the gateway knows `slack` and serves it, and shows the
  watch list and cursors;
- `test` posts a test message and waits for a thread reply.

### 6.7 Slack decisions for the human

These come from slack-mcp.md §6. The plan works with the defaults; each one
changes how useful Slack is.

1. **Slack will probably not notify you.** Posts are made as you, and Slack
   generally does not push-notify a user about their own messages. Without a
   fix, Slack is "a place to answer", not "a place that pings you", and you
   still need Signal, WhatsApp or the TUI to be alerted. Options: accept that;
   get a real Slack bot token (not available through this MCP); or try a
   channel set to "notify on every message" (unverified for self-authored
   posts). **Default: accept, and verify on a phone during
   `slack-channel` testing.**
2. **Which Slack conversation:** the DM-to-self (easiest) or a dedicated
   private channel such as `#wm-<user>` (cleaner search and context).
   **Default: DM-to-self.**
3. **Header-script concurrency:** live with serialized calls (the plan), or
   ask for a fix in standalone-codemode that serializes or caches the script's
   output for all `*_xaa` servers.
4. **Text-format stability:** the parsed format belongs to
   `llm.atko.ai/slack_xaa` and is not a contract. Is there an owner to ask for
   a JSON response mode?
5. **Rate limits** of the proxy are unknown. The plan starts at 20 s and backs
   off on errors.
6. **Personal setup:** `mcp-servers-personal.json` has no Slack entry. Is the
   Slack channel needed outside the work setup?

**Decided (§10.3):** accept no push notification for now (Q1); use the
DM-to-self (Q2); keep serialized calls, with gateway fixes left as follow-ups
(Q3, Q4); start polling at 20 s (Q5); Slack is **work setup only**, so
`mcp-servers-personal.json` gets no Slack entry (Q6).

## 7. TUI inbox

### 7.1 Feed

- `internal/tui/inbox.go` adds `WatchInbox(ctx, store, 1s)`, modelled on
  `WatchProjects` (`internal/tui/projects.go:397`): it polls
  `Store.ListOpen()` plus `deliveries/` and emits only on change.
- `model` gains `inbox []InboxItem` and an `inboxCh`, with a re-arming
  `waitForInbox` command, an `Init` hook and an `Update` case.
- The store root is passed through `tui.Run` from both call sites
  (`cmd/orch/main.go:370`, `:484`). The feed is file-based, so standalone
  `orch tui` works with no daemon.
- The existing `acpInputRouter` per-tab "inbox" (`internal/tui/acpwatch.go:147-168`)
  is renamed to `pendingInput` to avoid the clash.

### 7.2 Indicator in all modes

`m.inboxBadge()` renders:

- nothing when the inbox is empty;
- `✉ 3` (faint) when only notifications or agent-addressed rows are open;
- `✉ 2 ?` (bold yellow, the interactive-session colour) when any
  operator request is open.

It is shown:

- at the start of `renderFooter` (`internal/tui/tui.go:2735`), including when
  `statusMsg` is showing, so it is never hidden;
- at the end of the ACP header line (`internal/tui/acptabs.go:933`), which
  adds no rows;
- in the footer of the command picker and archive modals.

When a new operator request arrives, the TUI also rings the terminal bell
once (configurable off), because the badge alone is easy to miss.

### 7.3 Hotkey

**`I` (shift-i)** opens the inbox from every mode that is not typing text:
normal mode, the ACP view, the command picker and the archive confirm. It is
checked in `Update` right after `ctrl+c`, before mode routing.

- `I` is unused everywhere today (orch-integration.md §3.2). Lower-case `i`
  is already "compose" in the ACP view, so `I` keeps the mnemonic without a
  conflict.
- While composing text (ACP compose or an inbox reply), `I` is just a
  character. `esc` first, then `I`.
- `I` inside the inbox closes it (toggle), as does `esc`.

### 7.4 Inbox view

A new `modeInbox` in `uiMode` (`internal/tui/tui.go:35-50`), routed in
`Update` and `View`. It takes the full screen, using the ACP view's layout as
the template:

```
 Inbox  ✉ 2 ?                                            I/esc close
 ─────────────────────────────────────────────────────────────────────
 ? r1t8w  task fix-ci (workingman-messaging)        12m  slack,signal
   Which bucket for the export?
 ? q09ab  step-up                                    3m  slack
   Approve network allow proxy.golang.org for wm-…-fix-ci?
 · 7hd2k  task release (wm)                          1h
   Release v0.9.0 tagged
 ── To agents (read-only) ────────────────────────────────────────────
 → x81mm  task fix-ci → step-up                      14m  received
   Need network access to proxy.golang.org
 ─────────────────────────────────────────────────────────────────────
 [body of the selected message, wrapped, scrollable]
 Options: 1) staging  2) prod
 ─────────────────────────────────────────────────────────────────────
 > _
 j/k select · enter reply · 1-9 pick option · d dismiss · ctrl+f/b scroll
```

Keys:

| Key | Action |
|---|---|
| `j` / `k` | select next / previous |
| `enter` | start typing a reply to the selected request |
| `1`–`9` | answer with that option immediately (requests with options) |
| `d` | dismiss the selected notification (claims `outcome: dismissed`) |
| `ctrl+f` / `ctrl+b` | scroll the body |
| `y` | copy the body (OSC52) |
| `I` / `esc` | close (while typing, `esc` stops typing first) |

The reply line reuses the ACP compose code: `handleACPComposeKey`
(`internal/tui/tui.go:619-688`) and `renderACPInputLine`
(`internal/tui/acptabs.go:1071-1083`) are factored into a shared
`lineInput{draft}`. `enter` submits, `ctrl+u` clears.

### 7.5 Writing answers

On submit, the TUI calls `Store.Claim(id, {outcome: answered, response,
responder: {operator, via: tui}})`, which creates `outbox/<id>.json`. On
success the row stays (greyed, "sending…") until post-office finalises it and
the inbox file disappears. On `ErrAlreadyClosed` the status line shows
`already answered via slack`. If post-office is not running, the row shows
`answered — waiting for post-office` after 10 s so the operator knows the
answer is saved but not yet delivered.

The store is behind a small interface so tests can fake it, as `acpSend` is
faked today.

## 8. Step-Up agent

### 8.1 What it is

An always-on, autonomous agent that **receives** step-up requests from other
agents ("I need network access to `proxy.golang.org`"), decides what to do,
asks the operator for approval when needed, and asks the daemon to apply the
change. It is both a post-office receiver (`next_message`, `reply`) and a
sender (`send_request` to the operator).

How agents use it, from the requesting agent's side:

```
send_request(recipient: "step-up",
             subject: "Need network access to proxy.golang.org",
             body: "go mod download fails: … I need https://proxy.golang.org for this task.")
→ status: pending, request_id: msg_A
wait_for_response(msg_A) … → status: answered, response: "Granted: network allow proxy.golang.org. Retry now."
```

### 8.2 Agent kind and lifecycle

Modelled on the workingman agent (orch-integration.md §4.2, §4.6):

- `agent.StepUpAgent`, appended to the kind list, `String() == "step-up"`,
  not `Interactive()`.
- Persistent, supervised by the daemon with the same backoff as the
  workingman agent (`internal/daemon/workingman_agent.go:274-324`), in a new
  `internal/daemon/step_up.go`.
- Fixed sandbox name `step-up` and session name `step-up-agent`
  (`ACPSandboxNameFor`, `internal/runner/runner.go:1594`).
- Enabled with `--step-up-agent auto|on|off` (`auto` = on when `--acp-kit`
  is set and post-office is reachable).
- **Static, non-dynamic MCPs: `["post-office"]`.** `MCPGatewayDynamic` returns
  false for it, so it cannot `mcp-add` anything else.
- **Mounts:**
  - read-only: `~/.workingman/state/snapshot.json`, the audit log, and the
    sessions root (so it can see which sandboxes and tasks exist);
  - read-write: `~/.workingman/step-up/` (its request directory, §8.3) and a
    scratch dir;
  - **no orch roots**, read-only or read-write. All state changes go through
    the typed request interface, where the daemon can check them.
- Exempt from the reaper and idle timeout; re-adopted by reconcile; special
  cases in `detachable`, `sessions.go` and `snapshot_live.go`, exactly where
  the workingman agent has them.
- Prompt: `internal/prompts/templates/step-up.tmpl` (§8.6).

### 8.3 The workingman interface: the step-up request directory

The daemon has no control API today, and the smallest safe way to give
Step-Up "direct access to workingman" is the existing precedent of an agent
writing request files that the daemon picks up (`catchup-request`,
`internal/daemon/sleepy_head.go:38-63`). Only the Step-Up sandbox mounts this
directory, so **the mount is the capability**.

```
~/.workingman/step-up/
├── requests/<op-id>.yaml     written by Step-Up (write-then-rename)
├── results/<op-id>.yaml      written by the daemon
└── audit.jsonl               append-only, written by the daemon
```

A request:

```yaml
version: 1
id: op_01JD…                # Step-Up chooses; must be unique
op: sandbox_network_allow   # see the table below
for_request: msg_A          # the post-office request from the agent being helped (required)
approval: msg_B             # the operator-approval request (required for every allow)
args:
  resource: proxy.golang.org
reason: "go mod download for task fix-ci"
```

A result:

```yaml
id: op_01JD…
status: applied             # applied | rejected | failed
detail: "sbx policy allow network --sandbox wm-…-fix-ci proxy.golang.org; persisted to tasks/fix-ci.yaml"
at: 2026-11-20T10:45:00Z
```

**v1 operations:**

| `op` | Effect | Approval |
|---|---|---|
| `sandbox_network_allow` | `sbx policy allow network --sandbox <target> <resource>` on the live sandbox, **and** appends the rule to the target task's `policies:` so a recreated sandbox keeps it. | Operator approval, **always** (every new host asks). |
| `sandbox_network_revoke` | `sbx policy rm network --sandbox <target> --resource <resource>` and removes the rule from `policies:`. Only rules previously added through Step-Up. | None (narrows access). |
| `sandbox_filesystem_allow` | `sbx policy allow filesystem --sandbox <target> <path-glob>` on the live sandbox, and appends the rule (`type: filesystem`, see `internal/policy/policy.go`) to the target task's `policies:`. | Operator approval, always. |
| `sandbox_filesystem_revoke` | `sbx policy rm filesystem --sandbox <target> --resource <path-glob>` and removes the rule from `policies:`. Only rules previously added through Step-Up. | None (narrows access). |
| `list_sandbox_policy` | Writes `sbx policy ls --sandbox <target>` output to the result. | None (read-only). |

Task state changes (such as retrying a task), `deny` removal, project-level
state changes and anything touching a sandbox other than the requester's are
**not** in v1. The `step-up-executor` task must first confirm the exact `sbx`
syntax for filesystem rules on a live sandbox and adjust the argv above.
Adding an operation is a code change in the executor, reviewed like any other.

The **target sandbox is never an argument**. The daemon derives it from
`for_request`: it is the `from.sandbox` of that post-office request (looked up
in `inbox/` or `done/`), and that request must be addressed to `step-up`. So
Step-Up can only change the sandbox of an agent that actually asked it for
help, and only while that request is open or closed within the last hour.

The daemon executor (`internal/daemon/step_up_exec.go`) watches
`requests/`, handles one request at a time, writes the result, and appends an
audit line with the request, the approval record, the exact `sbx` argv and its
output. Step-Up polls `results/<op-id>.yaml` (its prompt says how).

Later, once post-office has proven Go MCP hosting, these ops can become typed
tools on a `workingman-admin` MCP granted only to Step-Up
(orch-integration.md §4.5, option c). The request schema becomes the tool
schema; the checks stay in the daemon.

### 8.4 Safety limits

Enforced by the daemon executor, not by the prompt:

1. **Allowlisted operations only** (the table above). Unknown `op`, unknown
   fields or a bad `version` → `rejected`.
2. **Target bound to the requester** via `for_request` (above).
3. **Operator approval is verified, not trusted.** For an op that needs it,
   `approval` must name a post-office record in `done/` that:
   - was sent by Step-Up (`from.agent == step-up`) to the operator;
   - has `in_reply_to == for_request`;
   - was answered by the **operator** (`responder.kind == operator`), with
     `option == "approve"` (Step-Up must send `options: ["approve", "deny"]`);
   - contains the **canonical change line** in its body, for example
     `CHANGE: sandbox_network_allow proxy.golang.org → wm-…-fix-ci`, which the
     daemon recomputes from the op and compares exactly. The operator is
     approving that line, so Step-Up cannot get approval for one change and
     apply another;
   - was answered less than 24 h ago, and has not been used by an earlier op
     (single use, tracked in `audit.jsonl`).
4. **Resource validation** for network rules: a hostname, or `*.` followed by
   a domain with at least two labels; optional `:port`. Rejected: `**`, `*`,
   bare TLDs, IP ranges, `localhost`, `host.docker.internal`, `127.*`,
   `169.254.*` and private ranges, and anything on a configurable denylist.
   For filesystem rules: an absolute, cleaned path glob (no `..`). Rejected:
   `/`, `/**`, `**`, the home directory itself, and anything under `~/.ssh`,
   `~/.aws`, `~/.config/gh`, `~/.workingman` (including the step-up and
   post-office directories), `/etc`, the orch roots, and the configurable
   filesystem denylist.
5. **Only live task sandboxes.** The target must have a live session. Never
   `step-up`, `workingman-agent`, or a daemon gateway.
6. **Rate limit:** at most 10 applied ops per hour and 3 per target sandbox
   per hour. Excess requests are rejected with a reason.
7. **Kill switch:** `~/.workingman/step-up/disabled` (any content) makes the
   executor reject everything, and `--step-up-agent off` stops the agent.
8. **Everything is audited**, including rejections, and the operator gets a
   post-office notification for every applied change: `🔓 Step-Up allowed
   proxy.golang.org for task fix-ci (approved via slack)`.

### 8.5 Configuration

```yaml
# ~/.workingman/step-up/config.yaml (read by the daemon, not mounted into the sandbox)
deny:
  network:
    - "*.internal.example.com"
  filesystem:
    - "/Users/*/Library/Keychains/**"
limits:
  per_hour: 10
  per_sandbox_per_hour: 3
```

There is no auto-approve list in v1: every allow (network or filesystem)
asks the operator. An auto-approve list can be added later if asking proves
too noisy.

### 8.6 Prompt outline (`step-up.tmpl`)

1. Who you are, and that you never do the requesting agent's work for it.
2. The loop: `next_message` → read the request → decide → `reply`. Always
   reply, including refusals with a reason.
3. Deciding: is the ask specific and justified by the task? Prefer the
   narrowest resource. Requests you cannot satisfy with a v1 op get a reply
   explaining that the operator has to do it, plus a notification to the
   operator.
4. Approval: for every allow, `send_request` to the
   operator with `options: ["approve", "deny"]`, the canonical `CHANGE:` line,
   the requesting task and its justification. While waiting, keep serving
   other messages: use `check_response`, not a blocking wait.
5. Applying: write the request file, poll for the result, reply to the
   original requester with what was done and what to retry.
6. The request-file format, the op table and the rule that the daemon will
   reject anything else.

## 9. Implementation task breakdown

Proposed orch tasks, in dependency order. Names are kebab-case and at most 42
characters. Repos are noted where a task is not workingman-only. Tasks with
no dependency between them can run in parallel.

```
verify-gateway-async-passthrough ─┐
codemode-forward-gateway-id ──────┤
postoffice-store ─────────────────┼─► postoffice-mcp-server ─► postoffice-receiver-tools ─► step-up-agent ─► step-up-executor
                                  │        │                                                                     │
                                  │        └─► codemode-post-office-catalog                                      │
                                  ├─► tui-inbox                                                                  │
                                  └─► postoffice-dispatcher ─┐                                                   │
daemon-mcp-gateway-generalize ───────────────────────────────┴─► slack-channel ─► orch-slack-setup-cli          │
                                                                                                                 ▼
                                                                                              messaging-docs-and-smoke-test
```

1. **`verify-gateway-async-passthrough`** — depends on: none. Repos:
   standalone-codemode, workingman. A research spike with a throwaway test
   MCP server behind a sub-gateway. Measure what actually survives the
   standalone-codemode gateway and the claude-code-acp harness:
   `_meta.progressToken` forwarding and `notifications/progress` relay, client
   cancel propagation, the gateway's per-call timeout, the real first-byte
   limit for a call that blocks 50 s, 90 s and 300 s, whether code mode is in
   play on sub-gateways, and the harness's Claude Code version. Write the
   results into `docs/research/mcp-async-requests.md` §7 and recommend a
   `max_wait`. No production code.

2. **`codemode-forward-gateway-id`** — depends on: none. Repo:
   standalone-codemode. Generate the sub-gateway id before `Build` and inject
   `X-Codemode-Gateway-Id: <id>` on every backend request for that
   sub-gateway, on both the static attach path (`Manager.attachSpec`) and the
   dynamic `mcp-add` path. Parent `/mcp` calls send no id. Add an optional
   catalog flag `dynamic_addable: false` that makes `mcp-add` refuse the
   entry. Tests for both, and a note in `docs/catalog.md`.

3. **`postoffice-store`** — depends on: none. Create `internal/postoffice`
   with `doc.go`, the message model (§2), and the `Store` over
   `~/.workingman/post-office/` (§3): atomic writes, the `link(2)` outbox
   claim with `ErrAlreadyClosed`, finalisation into `done/<day>/`, startup
   recovery, retention, option matching, and a change notifier (fsnotify plus
   poll). No MCP and no daemon code. Thorough unit tests, including concurrent
   claims from several goroutines and processes, and crash-recovery cases
   between each finalisation step.

4. **`postoffice-mcp-server`** — depends on: `postoffice-store`,
   `codemode-forward-gateway-id`. Add `cmd/post-office` on go-sdk (new
   dependency, same version as standalone-codemode), with streamable HTTP on
   `127.0.0.1:7077/mcp`, bearer-token auth, `post-office setup` to create the
   token, the identity resolver (§4.3), the capability check (§4.2), and the
   sender tools with the exact schemas and descriptions in §4.4. Implement the
   bounded wait, progress heartbeats, expiry ticker, and the
   `post-office://messages/<id>` resource. Add a Taskfile task and launchd
   plist. Tests use an in-process client against the handler with fake
   gateway records and sessions.

5. **`codemode-post-office-catalog`** — depends on: `postoffice-mcp-server`.
   Repos: standalone-codemode, workingman. Add the `post-office` entry to
   `mcp-servers.json` and `mcp-servers-personal.json` (bearer
   `${post_office_token}`, `dynamic_addable: false`). In workingman, add the
   planning-agent prompt paragraph on when to put `post-office` in a task's
   `static_mcps`, and document the capability in `agents.md`. Verify end to
   end that a task with `static_mcps: [post-office]` can call
   `send_notification` and one without it cannot, including through
   `mcp-add`.

6. **`tui-inbox`** — depends on: `postoffice-store`. Add the inbox feed, the
   badge in the footer, ACP header and modals, the `I` hotkey from every
   non-typing mode, `modeInbox` with the list, body and reply line, option
   keys and dismiss, all per §7. Factor the ACP compose line into a shared
   `lineInput`. Rename the ACP `acpInputRouter` "inbox" to `pendingInput`.
   Pass the store root through `tui.Run` from both call sites. Tests with a
   fake store, in the style of the existing TUI tests.

7. **`postoffice-dispatcher`** — depends on: `postoffice-store`. Add the
   daemon-side Dispatcher (§5.2): inbox watch → `Registry.SendTopic` on topic
   `post-office`, conversation index registration with a new
   `ConversationKindPostOffice`, `deliveries/` records with retry, and the
   `done/` watch that sends closing notes to every other delivery. Add the
   optional `channels.Resolver` interface. Router changes (§5.4): the
   post-office reply-to branch calling `Store.Claim`, "already answered"
   replies, `#<short>` top-level answers and `/inbox`. Wire it in
   `cmd/orch/channels.go`. Update `docs/channels.md`. Tests with channeltest
   fakes.

8. **`daemon-mcp-gateway-generalize`** — depends on: none. Extract the
   mint, record, re-mint-on-loss and delete logic from
   `internal/daemon/review_gateway.go` and the session handling from
   `review_mcp.go` into a reusable `mcpgateway.DaemonGateway{Key, Servers}`
   with a `ToolCaller` interface. Move review onto it with no behaviour
   change; existing review gateway tests must pass unchanged or be moved
   as-is.

9. **`slack-channel`** — depends on: `daemon-mcp-gateway-generalize`,
   `postoffice-dispatcher`. Add `internal/channels/slack` (§6): factory and
   strict options, the serialized worker with header-script retry, `Send`
   with `"<channel>:<ts>"` ids, the persisted sent set and watch list, the
   poll loop over `read_channel`/`read_thread` with the per-thread fallback,
   the detailed-format parser with golden tests from slack-mcp.md, `allow_from`,
   and `Resolver` as thread reply plus reaction. Register it in
   `cmd/orch/channels.go` and `inboundTypes`. Test on the DM-to-self, and
   record whether the phone gets a notification (§6.7 Q1).

10. **`orch-slack-setup-cli`** — depends on: `slack-channel`. Add
    `orch slack setup|status|test` (§6.6), modelled on `orch signal`,
    using the `yaml.Node` editor to write the channel block and the
    `post-office` route. Document it in `docs/channels.md` with a manual smoke
    test.

11. **`postoffice-receiver-tools`** — depends on: `postoffice-mcp-server`.
    Add the recipient registry (`step-up` → sandbox `step-up`) and the
    receiver tools `next_message`, `reply` and `list_inbox` (§4.5), listed
    only for registered recipients. Add the `in_reply_to` field to
    `send_request` for Step-Up's own approval requests (set automatically
    when Step-Up sends while holding a received request, or explicitly via an
    optional argument visible only to recipients). Show agent-addressed
    messages as read-only rows in the TUI inbox if `tui-inbox` has landed.
    Tests for delivery-once, restart recovery and races between `reply`,
    sender cancel and expiry.

12. **`step-up-agent`** — depends on: `postoffice-receiver-tools`. Add the
    `StepUpAgent` kind and everything it touches (§8.2): agent kind,
    `step-up.tmpl` (§8.6), runner names, mounts and static non-dynamic MCPs,
    the daemon supervisor in `internal/daemon/step_up.go`, the reconcile,
    reaper, session and snapshot special cases, and the `--step-up-agent`
    flag. At this stage it can receive, ask the operator and reply, but it
    cannot apply changes; its prompt says so. Add the `agents.md` section.

13. **`step-up-executor`** — depends on: `step-up-agent`,
    `postoffice-store`. Add the daemon executor for
    `~/.workingman/step-up/requests/` (§8.3) with the v1 ops (network
    and filesystem allow/revoke, list policy; no task retry, no auto-approve),
    the
    `config.yaml` loader, and every safety limit in §8.4: target derivation
    from `for_request`, approval verification including the canonical change
    line and single use, resource validation, rate limits, kill switch,
    persisting rules to the task's `policies:`, `audit.jsonl`, and the
    operator notification. Then update `step-up.tmpl` with the request
    format. Also make acp-wrapper re-apply all of a task's `policies:` when a
    sandbox is reused (today only gateway rules are re-applied,
    `internal/acpwrapper/acpwrapper.go:694-706`), so Step-Up's persisted rules
    survive reuse. Unit tests for every rejection path, with a fake `sbx`
    runner.

14. **`messaging-docs-and-smoke-test`** — depends on:
    `codemode-post-office-catalog`, `tui-inbox`, `orch-slack-setup-cli`,
    `step-up-executor`. Write `docs/messaging.md` (user-facing: how to enable
    post-office for a task, the inbox, Slack, Step-Up and its config), update
    `docs/packages.md`, `README.md` and `agents.md`, and add a manual
    end-to-end smoke test: a task asks the operator a question and gets the
    answer from the TUI, then from Slack with the TUI row disappearing and
    Slack showing "answered elsewhere", then asks Step-Up for a network route
    that needs approval.

## 10. Risks and open questions

### 10.1 Risks

| Risk | Impact | Mitigation |
|---|---|---|
| The gateway or harness kills calls well under 50 s, or code mode hides tools. | Waits fail or loop rapidly. | Task 1 measures it before the server is built; `max_wait` is a flag; every call is short and the request is durable. |
| Agents do not loop on `wait_for_response` and just carry on. | Questions get answered after the agent has moved on. | The tool description is explicit; Tasks support later removes the loop. Answers still land in `done/` for the record. |
| Slack does not notify the operator about self-authored posts. | Slack is useless as an alert channel. | Keep Signal, WhatsApp and the TUI bell; verify in task 9; decision Q1. |
| The Slack text format changes. | Inbound polling breaks silently. | Golden tests, loud logging when no `Message TS:` lines are parsed, per-thread fallback, `orch slack status`. |
| Header-script failures under load. | Slack calls fail. | One serialized worker with retry; ask for a gateway fix (§6.7 Q3). |
| Step-Up is prompt-injected by a requesting agent's message body. | Unwanted access is requested. | The daemon, not the prompt, enforces: target bound to the requester, approval bound to an exact change line answered by the operator, resource validation, rate limits, audit. |
| The operator approves by reflex. | Approval becomes a rubber stamp. | The approval text shows exactly one canonical change, the task and its reason; every applied change is notified and audited; revoke exists. |
| Three processes share a directory. | Races or half-written files. | One writer per file, write-then-rename, `link(2)` claim, idempotent finalisation, startup recovery. |
| post-office is down. | Agents cannot send; answers wait in `outbox/`. | launchd keeps it alive; the TUI shows "waiting for post-office"; finalisation catches up on start. |
| Attribution picks the wrong session in a reused sandbox (task agent then commit agent). | A message names the wrong agent. | Use the newest live session; record `session_id`; Q8. |

### 10.2 Open questions for the human

1. **Approve the overall shape**: standalone `post-office` process, a shared
   file store under `~/.workingman/post-office/` rather than inside an orch
   root, and the bounded-wait tool loop now with Tasks later.
2. **Hotkey `I`** for the inbox, from every mode that is not typing. OK, or
   another key?
3. **Slack decisions** in §6.7: accept "no push notification" for now, the
   DM-to-self as the default conversation, whether to ask for the
   header-script fix and a JSON response mode, and whether Slack is needed in
   the personal setup.
4. **Step-Up's power.** Is the v1 op list right (network allow and revoke,
   task retry, list policy)? Should it get read-only orch roots to understand
   tasks better? Should filesystem rules ever be allowed?
5. **The auto-approve list.** Is there a starting list of hosts Step-Up may
   allow without asking (§8.5), or should every allow ask you at first?
6. **Agent-to-agent visibility.** Should requests to Step-Up appear in your
   TUI inbox as read-only rows (proposed), and should you be able to answer
   them yourself in place of Step-Up?
7. **Notifications to channels.** Should operator *notifications* (not just
   requests) go to every channel, or only to the TUI by default, to keep the
   phone quiet? Proposed: every channel, with a `notify: false` option on a
   route to opt out.
8. **Attribution in reused sandboxes**: is "newest live session" the right
   sender when a sandbox has had several sessions?
9. **Re-applying task policies on sandbox reuse.** Step-Up persists rules to
   the task's `policies:`, but a reused sandbox only re-applies gateway rules
   today (`internal/acpwrapper/acpwrapper.go:694-706`). Should acp-wrapper
   re-apply all task policies on reuse? That would be a small extra task.
10. **Default expiry** of 24 h for requests and 7 days for notifications: OK?

### 10.3 Decisions

Recorded 2026-11-20 from the operator's review:

1. Overall shape **approved**: a standalone post-office process, the shared
   store under `~/.workingman/post-office/`, and the bounded-wait loop now
   with Tasks later.
2. Hotkey **`I`** approved.
3. Slack: accept no push for now; the **DM-to-self** is the conversation;
   Slack is **work setup only** (no entry in `mcp-servers-personal.json`).
   Gateway fixes (header script, JSON mode) are follow-ups, not v1.
4. Step-Up v1 ops: **network allow/revoke, filesystem allow/revoke, list
   policy**. `task_retry` is dropped. No read-only orch-root mount.
5. **No auto-approve list**: Step-Up asks the operator for every new host
   (and every filesystem allow).
6. Requests to Step-Up **show in the TUI inbox** as read-only rows.
7. Operator notifications go to **every channel** (with the `notify: false`
   route opt-out).
8. "Newest live session" is fine for attribution for now.
9. Re-applying all task policies on sandbox reuse: accepted, folded into
   `step-up-executor`, because Step-Up's persisted rules depend on it.
10. Default expiry 24 h for requests and 7 days for notifications: accepted.
