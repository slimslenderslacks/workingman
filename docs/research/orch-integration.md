# Research: where messaging plugs into orch

This note maps the workingman (orch) code that the messaging project touches:

- the `post-office` MCP;
- fan-out of operator messages to every channel;
- a Slack channel;
- a TUI inbox;
- the always-on "Step-Up" agent.

It describes what exists today, with `file:line` references, and recommends a
landing spot for each piece. It does not change any code.

It is a companion to [mcp-async-requests.md](mcp-async-requests.md), which
covers:

- the MCP tool surface (`send_request`, `wait_for_response`, …);
- hosting post-office as `cmd/post-office`;
- caller identity via `X-Codemode-Gateway-Id`.

This note does not repeat those, except where orch code has to change to
support them.

Paths are relative to the `workingman` repo unless prefixed
`standalone-codemode/`. Research date: 2026-11-20, at workingman `5a9b8607`
and standalone-codemode `ffab8eaf`.

Contents: [TL;DR](#tldr) · [1. Channels](#1-channels) ·
[2. Fan-out and "answered elsewhere"](#2-fan-out-and-answered-elsewhere) ·
[3. TUI](#3-tui) · [4. Agents, prompts, policies](#4-agents-prompts-and-sandbox-policies) ·
[5. MCP servers and caller identity](#5-how-agents-get-mcp-servers-and-caller-identity) ·
[6. Change list](#6-consolidated-change-list) · [7. Open questions](#7-open-questions)

## TL;DR

- **Channels.** A channel is a small interface: `Name`, `Start`, `Send`
  returning a message ID, and `Close` (`internal/channels/channel.go:15-23`).
  - Factories are keyed by `type` in `cmd/orch/channels.go:63-70`.
  - Reply correlation is purely `(channel, messageID) → ConversationTarget` in
    `FileConversationIndex` (`internal/channels/conversation.go:45-50`). It is
    keyed on the transport's quoted-reply ID.
  - **A `slack` channel** means:
    - a package `internal/channels/slack`;
    - one factory entry;
    - `"slack"` added to `inboundTypes` (`internal/channels/config.go:316`);
    - `ts`/`thread_ts` used as message ID / `ReplyToID`;
    - `Send` and inbound polling implemented as tool calls on a daemon-owned
      sub-gateway over `["slack"]`. This copies the review gateway
      (`internal/daemon/review_gateway.go`, `review_mcp.go`).
- **Fan-out.** `Registry.SendTopic` already sends one text to every route and
  returns per-route IDs (`internal/channels/registry.go:447-468`).
  - **What is missing:**
    - a durable record of "these N sends are one logical message";
    - a cross-process "first answer wins" claim;
    - an "answered elsewhere" broadcast.
  - The closest thing is the router's in-memory permission `sync.Once`
    (`internal/router/permission.go:26-37`). It does not tell the other chats.
  - **Recommendation:** a new `internal/postoffice` package that owns:
    - the on-disk store `~/.workingman/post-office/{inbox,outbox,done}`;
    - the claim, an `O_EXCL` create of `outbox/<id>.json`;
    - a daemon-side `Dispatcher` that watches the inbox, fans out through the
      Registry, registers a new `ConversationKind` `post-office`, and
      broadcasts "answered elsewhere".
  - The router gets one new branch in `process` for that kind.
- **TUI.** The TUI has no header or badge row. Every mode ends in a 1-row
  footer (`renderFooter`, `internal/tui/tui.go:2735`), except the ACP view
  (own header, `internal/tui/acptabs.go:933`) and full-screen modals.
  - Put an inbox badge in both the footer and the ACP header.
  - Feed it from a file poller like `WatchProjects`.
  - Bind **`i`** (normal mode) / **`I`** (everywhere, including ACP) to a new
    inbox mode.
  - Reuse the hand-rolled ACP compose line for replies.
- **Step-Up agent.** Model it on the workingman agent:
  - a new `agent.Kind`;
  - persistent, supervised by the daemon;
  - a fixed sandbox name;
  - a new `step-up.tmpl`.
  - It should get **static** (not dynamic) MCPs `["post-office"]`.
  - Today nothing in orch changes a live sandbox's policy. Task policies are
    applied only at `sbx create`; reused sandboxes re-apply only the gateway
    rules (`internal/acpwrapper/acpwrapper.go:694-706`).
  - "Direct access to the workingman service" therefore needs a new
    host-side executor. Recommended: file requests in a Step-Up-only
    directory, watched by the daemon, which validates, runs
    `sbx policy … --sandbox`, persists the rule into the task's `policies:`,
    and audits.
- **Selective grant and identity.** Tasks opt in with
  `static_mcps: [post-office]` (`internal/task/task.go:206`).
  - Dynamic gateways (Task, Wolf, Workingman by default,
    `internal/runner/runner.go:1280-1288`) can `mcp-add` any catalog server.
    So post-office must check that the caller's gateway was *minted* with
    `post-office`.
  - It does this through `~/.workingman/gateways/<sandbox>.json`
    (`Record.Servers`), keyed by the gateway id that standalone-codemode would
    have to start forwarding.

---

## 1. Channels

### 1.1 Interfaces and types (`internal/channels`)

```go
// channel.go:15-23
type Channel interface {
    Name() string                                   // instance name from channels.yaml
    Start(ctx context.Context, h InboundHandler) error // return quickly; own goroutines
    Send(ctx context.Context, msg OutboundMessage) (messageID string, err error)
    Close() error                                   // idempotent
}
type InboundHandler func(ctx context.Context, msg InboundMessage) // :28
```

- **`InboundMessage`** (`channel.go:31-40`) has `Channel`, `ChatID`,
  `SenderID`, `SenderName`, `MessageID`, `ReplyToID`, `Text` and `Timestamp`.
- **`OutboundMessage`** (`channel.go:43-51`) has `ChatID`, `Text`,
  `ReplyToID` and `Topic`.
  - The comment on `Topic` says threaded transports *may* use it to pick a
    thread.
  - Today every channel just prefixes `[topic]` through `FormatTopic`
    (`:55`).
- **Optional interfaces:**
  - `Chunker` (`channel.go:67-69`) and `Typer` (`:73-75`) are probed by the
    router through `Out.Get`.
  - Signal implements both (`channels/signal/channel.go:76-80`).
  - **There is no `Editor` or `Deleter` interface.** "answered elsewhere" can
    therefore only be a new message today.

### 1.2 Registry and config

- **`Factory`:**
  `func(name string, spec ChannelConfig, creds Credentials) (Channel, error)`
  (`registry.go:153`).
- **`Build`** (`registry.go:189-228`) does, for each enabled channel:
  - looks up `factories[spec.Type]`;
  - resolves credentials;
  - checks that `ch.Name()==name`;
  - calls `Add`;
  - and at the end `SetRoutes(cfg.ActiveRoutes())`.
- **`Start`** (`:295-351`) starts every channel concurrently. A failed
  channel is closed and skipped. `wrapHandler` (`:355-366`) stamps
  `msg.Channel` and recovers panics.
- **Sending:**
  - `Send(ctx, channel, msg)` is at `:403-420`.
  - `RoutesFor(topic)` (`:425-441`) returns the exact-topic routes, or, if
    there are none, the `"*"` routes.
  - `SendTopic` (`:447-468`) sends to every route concurrently and returns
    `[]SendResult{Route, MessageID, Err}` (`:160-164`).
- **`channels.yaml`** lives at `$WORKINGMAN_CHANNELS_CONFIG`, otherwise
  `~/.workingman/channels.yaml` (`config.go:20`, `:146-155`). The
  `orch --channels-config` flag overrides it (`cmd/orch/main.go:117`).
  - It is decoded strictly (`KnownFields(true)`, `config.go:199-210`).
  - **Schema:**
    - `Config{Channels map[string]ChannelConfig; Routes []Route; Notify NotifyConfig; Router RouterConfig}`
      (`:46-51`);
    - `ChannelConfig{Type; Enabled *bool; Credentials map[string]CredentialRef; Options map[string]any}`
      (`:110-116`);
    - `CredentialRef{Env | File+Key}` (`:124-128`, validated `:286-301`;
      secret files must be 0600, `:404`);
    - `Route{Topic, Channel, Chat}` (`:132-136`);
    - `RouterConfig{PermissionTimeout, TurnTimeout, ThinkingAfter, QueueSize}`
      (`:82-87`).
  - `inboundTypes = {"whatsapp","signal"}` (`:316`) is hard-coded.

### 1.3 Notifier and daemon announcements

- **`channels.Notifier`** (`notifier.go:21-63`) adapts the Registry to
  `notify.Sender`/`TopicSender`. It **discards message IDs**, so plain
  notifications cannot be replied to.
- **Wiring** (`cmd/orch/channels.go:105-117`):
  `notify.Multi(Osascript, Async(channels.NewNotifier(reg)))` plus
  `daemon.WithChannels(reg, index, …)`.
  - `notify.Multi` (`internal/notify/multi.go:18-76`) is plain concurrent
    fan-out with no correlation.
- **Correlated sends** are the daemon's own announcements in
  `internal/daemon/channels_notify.go`.
  - `announceWolfStart` (`:127-169`):
    - sends topic `wolf` via `ChannelSender` (`:29-31`, the
      `Registry.SendTopic` signature);
    - then, for every successful route, calls
      `index.Register(route.Channel, messageID, ConversationTarget{Kind:"wolf", …})`
      (`:159-166`).
  - `announceSleepyHead` (`internal/daemon/sleepy_head.go:274-316`) is the
    same pattern with `Kind:"sleepy-head"`.
  - **This is the template for post-office fan-out.**

### 1.4 Conversation index and reply-to correlation

- **Interface:** `ConversationIndex{Register(channel, messageID, target); Lookup(channel, messageID)}`
  (`conversation.go:45-50`).
- **`ConversationTarget`** (`:25-40`) has `Kind`, `WorkStream`,
  `ProjectPath`, `SessionKey` and `SessionID`.
  - Kinds are the string constants `wolf` and `sleepy-head` (`:15-21`).
- **`FileConversationIndex`** (`:62-223`):
  - keys entries on `channel + "\x00" + messageID` (`:121`);
  - rewrites the whole JSON file atomically at 0600 (`:191-223`);
  - has a 30-day TTL and keeps at most 1000 entries (`:53-56`).
  - The file is `~/.workingman/state/channel-conversations.json`
    (`cmd/orch/channels.go:22`, `:83-89`; directory from `stateDirFor`,
    `:223-229`).
- **How replies correlate:** through the transport's quoted-reply ID.

  | Transport | Outbound message ID | Inbound `ReplyToID` |
  |---|---|---|
  | Signal | send timestamp (`signal/channel.go:385-431`) | `Quote.ID` (`signal/inbound.go:248-250`) |
  | WhatsApp Cloud | — | `Context.ID` (`whatsapp/cloud_webhook.go:632`) |
  | WhatsApp bridge | — | `QuotedMessageID` (`whatsapp/bridge_channel.go:369`) |
- **Lookup:**
  - `router.process` (`internal/router/router.go:388-400`) calls
    `Index.Lookup(channel, ReplyToID)`.
  - `resolveReplyTarget` (`:419-449`) then matches `SessionKey`/`SessionID`
    against the live wolves and sleepy-head.
  - The router also registers each chunk of a relayed reply
    (`internal/router/turn.go:354-369`), so a reply to a reply still routes.

### 1.5 Router

- **Interfaces:**
  - `Source` (`router.go:63-74`): `WorkingmanAgent`, `WolfSessions`,
    `SleepyHead`, `Status`.
  - `Outbound` (`:80-83`; implemented by `*Registry`).
  - `Agent` (`:89-93`): `Ask`, `Events`, `Close`.
  - `Attacher` (`:96`, `AttachACP` `:99`), which uses `acpchat.Attach` over
    `agent.sock`.
- **State** is per chat, `(channel, chatID)` (`router.go:177`, `chatFor`
  `:292`).
- **`Handle`** (`:274-290`) is the **single convergence point for inbound
  messages from every channel**. It checks, in order:
  1. empty text → drop;
  2. `/command` → `handleCommand` (`commands.go:32-53`: `/help`, `/status`,
     `/agent`, `/who`, `/wolf`, `/sleepy`);
  3. pending permission answer → `answerPermission`;
  4. otherwise `enqueue`.

  `process` (`:368-415`) then routes by reply-to, then by the chat's binding,
  and otherwise to the workingman agent.
- **Permissions** (`permission.go`):
  - `askPermission` (`:42-96`) puts one ACP permission request to every chat
    with a turn in flight on that session, otherwise to every chat bound to
    the wolf.
  - `answerPermission` (`:122-143`) treats the next message in a chat as the
    answer.
  - `resolve` (`:26-37`) uses `sync.Once`: **the first answer wins**.
  - **What is missing:**
    - the other chats are not told;
    - the prompt message IDs are not kept (`:79`);
    - a late answer elsewhere becomes ordinary agent input.

### 1.6 How Signal and WhatsApp are wired in `cmd/orch`

1. `main.go:83-90` dispatches `orch whatsapp|signal setup|status|test`.
2. `main.go:240-248` computes `stateDir` and calls `setupChannels`
   (`channels.go:44-93`), which:
   - runs `LoadOptional`; with zero channels, channels are disabled;
   - opens `channels.log`;
   - builds the factory map (`:63-70`);
   - calls `channels.Build`;
   - opens the conversation index.
3. `main.go:271` passes `ch.options(a)` (notifier plus `WithChannels`) to
   `daemon.New` (`:301`).
4. `main.go:309` calls `ch.start` (`channels.go:124-140`). That calls
   `router.New` with `Source: daemonSource{d}` (`:144-156`, `:159-202`) and
   `Out: reg`, then `reg.Start(ctx, rt.Handler())`.
5. **Transports:**
   - **Signal** (`signal/channel.go:107-150`): it probes `/api/v1/check`,
     runs an SSE listen loop, and sends with JSON-RPC.
   - **WhatsApp** (`whatsapp/backend.go:162-178`): `mode: bridge` runs a
     supervised Baileys process with long-polling; otherwise it uses the
     Cloud webhook plus the Graph API.

### 1.7 What a `slack` channel has to implement

**Where the Slack MCP is.** `localhost:3333` is the standalone-codemode
**gateway**, not Slack.

- Slack is the catalog entry `slack` → `https://llm.atko.ai/slack_xaa/mcp`
  with an Okta XAA `header-script`
  (`standalone-codemode/mcp-servers.json:40-41`).
- `mcp-servers-personal.json` has **no** slack entry. The personal setup
  needs a different Slack MCP, or none at all.
- The Slack tool set is the subject of the separate `research-slack-mcp`
  task. Everything below assumes only "post a message", "read
  channel/thread history" and something that returns `ts`.

**How the daemon reaches it.** Copy the review gateway:

- `internal/daemon/review_gateway.go`:
  - it mints a daemon-owned sub-gateway with
    `Create(ctx, []string{"github"}, false)` (`:263`);
  - it records it under `~/.workingman/gateways/daemon/` (`:61`), a directory
    acp-wrapper's sweep skips;
  - it re-mints with backoff and deletes the gateway on shutdown.
- `internal/daemon/review_mcp.go` opens a session with
  `mcpgateway.OpenSession(ctx, url, SessionOptions{ClientName:"orch-daemon"})`
  (`:116`) and calls `s.CallTool(ctx, "pull_request_read", args)`, reopening
  on `ErrSessionLost` (`:281-306`).
  - Tool names are the backend's bare names. There is no prefix.
- **Generalise this** into a `daemonGateway{key, servers}`, or move it to
  `internal/mcpgateway`. The Slack channel then gets a store key like
  `orch-slack` and its own `Session`.
  - The channel package should depend only on a small `ToolCaller`
    interface, so it can be tested with `channeltest`-style fakes.

**Pieces:**

1. **Package** `internal/channels/slack` with `const Type = "slack"` and
   `Factory(logger) channels.Factory`.
   - Decode options strictly, as Signal does (`signal/config.go:89-180`).
   - Reject unknown credential names (`:106-110`). Slack needs **no
     credentials**: the gateway's header-script authenticates.
2. **Registration:**
   - add `slack.Type: slack.Factory(logger, gatewayCaller)` to
     `cmd/orch/channels.go:63-70`;
   - add `"slack"` to `inboundTypes` (`config.go:316`).
3. **Config section:**
   ```yaml
   channels:
     slack:
       type: slack
       options:
         channel: C0456ABCD        # the Slack channel the bot posts in
         allow_from: [U0123ABCD]   # operator user ids; default deny
         poll_interval: 5s
   routes:
     - {topic: post-office, channel: slack, chat: C0456ABCD}
     - {topic: wolf,        channel: slack, chat: C0456ABCD}
   ```
4. **`Send`:**
   - call the post tool;
   - map `OutboundMessage.ReplyToID` to `thread_ts`, so replies stay in the
     thread;
   - optionally use `Topic` to choose a thread instead of the `[topic]`
     prefix;
   - return the posted `ts`.
   - Slack `ts` is unique per conversation, not globally, so return
     `"<channelID>:<ts>"` as the ID. That keeps the index key
     `(channel, id)` collision-free.
5. **Inbound (`Start`):**
   - The MCP is request/response, so this is almost certainly a **poll
     loop**, like the WhatsApp bridge's `client.Messages` loop
     (`bridge_channel.go:322`).
   - Read new messages in the configured channel, plus replies in the threads
     the channel opened, since a cursor `ts`.
   - Persist the cursor under `~/.workingman/slack/`.
   - Drop the bot's own posts and dedupe on `ts`.
   - Emit `InboundMessage{ChatID: channelID, MessageID: "<ch>:<ts>", ReplyToID: "<ch>:<thread_ts>"}`.
   - Run the access policy before calling the handler. The router does no
     authorisation (`docs/channels.md:91-92`).
6. **Setup:** `cmd/orch/slack.go` with `orch slack setup|status|test`.
   - Model it on `orch signal`: `main.go:89-90` and the `yaml.Node` editor
     `signal/setup/store.go`.
   - `status` should check that the gateway knows `slack` (`GET /gateways`
     response `servers.slack.state`).
7. **Wording:** "Reply to this message…" (`channels_notify.go:24`) reads as
   "reply in thread" on Slack. That is acceptable, or make it per channel.

**Assumptions to watch for:**

- `chatRef` hashes chat IDs for the audit log (`router.go:304-310`). That is
  harmless for Slack.
- The router assumes **one human per chat**. A shared Slack channel breaks
  that, so `allow_from` matters.
- Whether `ChatID` is the channel or `channel:thread_ts` is a design choice:
  - per-channel means one binding and one pending permission for the whole
    Slack channel;
  - per-thread gives a router chat per thread, but `Send` has to parse the
    ID.

  For post-office, per-thread is natural: one thread per request.

---

## 2. Fan-out and "answered elsewhere"

### 2.1 What exists

| Mechanism | Fan-out | Correlated replies | First-wins | Tells the others |
|---|---|---|---|---|
| `notify.Multi` / `channels.Notifier` | yes | no (IDs dropped) | — | — |
| `announceWolfStart` / `announceSleepyHead` | yes, every route of the topic | yes, every route's ID points at the same target | no; every reply is forwarded | no |
| Router permission prompt | yes, every active or bound chat | via "next message in chat" | yes (`sync.Once`, in memory) | no (the timeout message is the only broadcast, `permission.go:89-96`) |

**Nothing is durable or cross-process.** The TUI is a second consumer. In
integrated `orch --root` mode it runs in the daemon's process
(`cmd/orch/main.go:349-370`), but `orch tui` runs standalone with no daemon
(`main.go:478-486`). post-office is a separate process in the recommended
hosting. So the claim has to be on disk.

### 2.2 Recommendation: an `internal/postoffice` package

`internal/postoffice` is shared by three processes: `cmd/post-office`, the
daemon and the TUI. It holds:

- **`Store`.** On-disk layout under `~/.workingman/post-office/`, which sits
  next to `sessions/`, `gateways/` and `state/`; derive it from the sessions
  root the way `mcpgateway.StateDir` does (`internal/mcpgateway/store.go:48-50`).
  - `inbox/<id>.json` — written by post-office. It holds the message: kind
    (`notification`/`request`), from `{sandbox, project, task, agent}`,
    recipient (empty means the operator), body, `expires_at`, and the
    `deliveries[]` that the dispatcher appends.
  - `outbox/<id>.json` — the answer. It is **created with `O_CREAT|O_EXCL`**
    by whoever answers first (TUI, router or Step-Up agent), holding
    `{answer, answered_by: "tui"|"<channel>"|"<agent>", at}`. The create
    *is* the claim: the second writer gets `EEXIST` and is told "already
    answered". post-office picks it up and completes the waiting
    `wait_for_response`.
  - `done/<id>.json` — post-office moves the inbox entry here once the answer
    has been delivered, or it has expired or been cancelled. "Remove from the
    TUI inbox" is just "the file left `inbox/`".
  - All writes are write-temp-then-rename, as `FileConversationIndex` does
    (`conversation.go:191-223`).
  - Notifications get no outbox. Optionally the TUI writes an "ack" so it can
    clear them.
- **`Dispatcher`.** Runs in the daemon and is wired next to the router in
  `cmd/orch/channels.go:124-140`. It is built only when channels exist; the
  TUI does not need it. It:
  - watches `inbox/` with fsnotify, plus a poll;
  - for each new operator-addressed entry, calls
    `Registry.SendTopic(ctx, "post-office", text)`, and for each successful
    route calls `index.Register(channel, id, ConversationTarget{Kind:"post-office", SessionKey: requestID})`.
    This is the same shape as `announceWolfStart`. It records the deliveries
    in the inbox entry;
  - watches `outbox/`; when an answer appears, sends "✅ answered via
    `<answered_by>`" to every delivery except the answering one, as a reply
    to each delivery's message ID. Channels that later grow an `Editor`
    interface could edit the original message instead;
  - with no channels, does nothing. The TUI is then the only responder, as
    the project brief asks.
- **Router change.** One branch in `process` (`router.go:388-400`): when
  `Lookup` returns `Kind == "post-office"`, call
  `postoffice.Store.Answer(id, text, answeredBy=channel)` instead of
  `resolveReplyTarget`. On `EEXIST`, reply "already answered via X". Add the
  constant next to `conversation.go:15-21`.
  - Optionally add a `/inbox` command (`commands.go:34-52`) that lists open
    requests.

**Why not put it all in the router?** The router is per-chat and in-memory,
and it is only built when channels exist. The TUI-only case (no channels) and
the standalone TUI still need the store, and post-office must not depend on
the daemon. The router stays a thin adapter.

The router's existing permission prompts could later reuse the same claim and
"answered elsewhere" broadcast. That is out of scope here.

### 2.3 Why `~/.workingman` rather than an orch root

- The orch roots are watched by the daemon's dispatch watcher
  (`internal/daemon/watcher.go:113-137`). Unrecognised files are ignored
  today, but writes under a root are what the daemon is built to react to,
  and the workingman agent has every root mounted read-write
  (`internal/daemon/workingman_agent.go:226`).
- Messages are cross-project: the Step-Up agent and the operator inbox.
- `~/.workingman/` already holds the other per-host runtime state:
  - `sessions/` (`internal/session/session.go:170-175`);
  - `state/snapshot.json` (`internal/daemon/snapshot_live.go:86-92`);
  - `state/channel-conversations.json`;
  - `gateways/`.

---

## 3. TUI

**Libraries.** bubbletea v1.3.10 and lipgloss v1.1.0. There is **no
`bubbles`**, so there is no textinput component. Paths in this section are
relative to `internal/tui/`.

### 3.1 Modes and views

- **`model`:** the struct is at `tui.go:81-203`; `newModel` is at `:205`.
- **State axes:**
  - pane focus (`pane`, `tui.go:22-28`): `paneSessions`,
    `paneProjectDetail`, `paneProjects`, `paneTasks`;
  - modal `uiMode` (`tui.go:35-50`): `modeNormal`, `modeCommandPicker`,
    `modeConfirmArchive`;
  - boolean overlays: `showACP` (`:132`), `acpComposing` (`:143`),
    `zoomed` (`:106`), `leftVisible`/`rightVisible` (`:168-169`),
    `showHidden` (`:177`), `mouseCapture` (`:202`), and chord state
    `pendingKey` (`:194`).
- **Key routing** (`Update` `tui.go:273`, routing `:328-346`):
  1. `ctrl+c`;
  2. if `showACP`, `handleACPKey` (`:545`), which hands off to
     `handleACPComposeKey` (`:619`) while composing;
  3. by `mode`: picker (`command_picker.go:68`) or archive confirm
     (`archive.go:36`);
  4. otherwise `handleNormalKey` (`tui.go:353`).
- **`View`** (`tui.go:2040`) checks, in order:
  1. the picker modal (`command_picker.go:240`);
  2. the archive modal (`archive.go:190`);
  3. the ACP view (`acptabs.go:927`);
  4. zoomed: the focused pane plus the footer (`tui.go:2066-2075`);
  5. otherwise the three columns plus the footer (`tui.go:2077-2095`).

### 3.2 Hotkeys already taken

| Key | Where | Action | Ref |
|---|---|---|---|
| `ctrl+c` | everywhere | quit | `tui.go:331` |
| `q` | normal, ACP | quit | `tui.go:378`, `:551` |
| `t` then `l`/`r`/`p` | normal (300 ms chord) | toggle sessions column / detail column / hidden streams | `tui.go:362-374`, `413-422` |
| `:` | normal | command picker | `tui.go:380-394` |
| `a` | normal (if ACP feed), ACP | open / close ACP view | `tui.go:395-401`, `:553` |
| `h` `j` `k` `l` | normal, ACP | select prev/next; ACP: tabs and scrolling | `tui.go:402-409`, `555-564` |
| `alt+h/j/k/l` (`˙ ∆ ˚ ¬`) | normal, ACP, compose | move column / cycle pane / ACP tabs | `tui.go:423-453`, `555-560`, `630-634` |
| `z` | normal, ACP | zoom pane; ACP: expand tool calls | `tui.go:462`, `:574` |
| `m` | normal, ACP | mouse capture | `tui.go:467`, `:576` |
| `y` | normal, ACP | copy (OSC52) | `tui.go:469`, `:582` |
| `enter` | sessions pane, ACP, picker, compose | attach / compose / run / send | `tui.go:471`, `:605`, `command_picker.go:83`, `tui.go:653` |
| `esc` | picker, ACP, compose | close / back / stop typing | `command_picker.go:70`, `tui.go:553`, `:627` |
| `up`/`down` | picker | move | `command_picker.go:73-78` |
| `y`/`Y`, `n`/`N` | archive confirm | yes / no | `archive.go:38`, `:48` |
| `G` | ACP | follow newest output | `tui.go:570` |
| `ctrl+f` / `ctrl+b` | ACP, compose | page | `tui.go:566-568`, `641-644` |
| `x` | ACP (dead tab) | dismiss | `tui.go:588` |
| `i` | ACP (interactive tab) | compose | `tui.go:605` |
| `ctrl+u` | compose | clear draft | `tui.go:638` |
| mouse click | normal with mouse capture | focus / attach | `tui.go:695` |

- Picker entries (`command_picker.go:28-40`, no single-letter shortcuts):
  session, wolf, orch, workspace, review, cleanup, archive, stop, start, hide,
  show.
- **Free in normal mode:** `i I n N M ! ? / g G s w x e o u b c d f v tab esc`,
  and every ctrl combo except `ctrl+c`. Arrows are deliberately unbound
  (`tui.go:405`).
- `r` and `p` are free as first keys but are chord seconds after `t`.

### 3.3 Command picker

- `projectCommand{key, label, desc}` (`command_picker.go:15-19`) and the list
  `projectCommands` (`:28-40`).
- `:` opens it (`tui.go:380`). `dispatchProjectCommand` (`:98`, cases
  `:101-231`) runs the chosen command.
- To add an entry, append to the list and add a `case`.
- The picker is framed as *work-stream* commands, so a global inbox fits
  better as a direct key.

### 3.4 Always-visible indicators today

There is no header or status bar and no global counters. The layout is the
body plus `footerH = 1` (`computeLayout`, `tui.go:1904-1906`).

- **`renderFooter`** (`tui.go:2735-2765`) shows either `statusMsg` (red) or a
  faint key hint plus `focus: <pane>`, truncated to the width.
- **The ACP view** has its own chrome: title `"ACP Sessions"`
  (`acptabs.go:933`), tab bar (`renderACPTabBar` `:788`), status line, body,
  input and hint. `acpChromeRows = 4` (`:771`).
- **Modals** do not show the footer.
- Existing attention cues live inside panes:
  - the yellow interactive session style and the `◆` badge (`tui.go:1107`,
    `:1123`, `:1270`);
  - card borders (`projectCardBorder` `:1652`);
  - detail badges (`project_detail.go:137`).

### 3.5 Data sources

The TUI has no handle on the daemon, except the in-process session feed and
the interactive launcher. It has three feeds:

- `WatchProjects` (`projects.go:397`) polls the roots every 1 s and emits only
  on change.
- `sessCh` comes from the daemon's `WatchSessions` (`cmd/orch/main.go:354`),
  and is `nil` in standalone mode.
- `WatchACPSessions` (`acpwatch.go:205`) polls `session.json` every 500 ms.

Each has a re-arming `waitFor*` command (`tui.go:243-271`). The TUI writes
state only by `project.SaveAs` (for example `wolf.go:39`, `stop.go:40`). It
does not read `snapshot.json`.

### 3.6 Where the inbox fits

1. **Feed.**
   - Add `WatchInbox(ctx, dir, interval)` in a new `inbox.go`, modelled on
     `WatchProjects`, built on `postoffice.Store.List`.
   - Add `inboxCh`/`inbox []InboxItem` to `model`, a `waitForInbox` command,
     an `Init` hook (`tui.go:223`) and an `Update` case.
   - Pass the inbox dir through `tui.Run` (`tui.go:2793`) from both call sites
     (`cmd/orch/main.go:370`, `:484`). This also works in standalone `orch tui`,
     because the feed is file-based.
2. **Indicator in all modes.** Add a helper `m.inboxBadge()` (for example
   `✉ 2`, highlighted when any request is open):
   - prepend it in `renderFooter`, including when `statusMsg` is showing, so
     it is never hidden;
   - append it to the ACP header line (`acptabs.go:933`), which adds no rows
     and leaves `acpChromeRows` alone;
   - optionally add it to the picker and archive modal text.
3. **Hotkey.**
   - `i` in `handleNormalKey`.
   - `i` is compose in ACP, so a key that works everywhere should be `I` or
     `!`. Check it in `Update` before the mode switch, next to `ctrl+c`.
   - Not available: `a`, `t`, `r`/`p` after `t`, `m`, `y`, `z`, `x`, `G`,
     and `h/j/k/l`.
4. **View.**
   - Add `modeInbox` to `uiMode` (`tui.go:37-50`), route it in `Update`
     (`:340-345`) and `View` (`:2050-2062`).
   - Show a list (j/k, `enter` to reply, `esc` back). Use the ACP view's
     layout as the template.
   - **Reply line:** factor `handleACPComposeKey` (`tui.go:619-688`) and
     `renderACPInputLine` (`acptabs.go:1071-1083`) into a shared
     `lineInput{draft}`.
   - **Submit:** `postoffice.Store.Answer(id, text, "tui")`, which is the
     `O_EXCL` claim. On `EEXIST`, show `statusMsg`
     "already answered via <x>".
   - Keep the store behind a small interface so tests can fake it, as
     `acpSend` is faked.
5. **Naming clash.** `acpInputRouter` already calls its per-tab queue an
   "inbox" (`acpwatch.go:147-168`). Rename one of them to avoid confusion.

---

## 4. Agents, prompts and sandbox policies

### 4.1 Agent kinds

The kinds are an iota block in `internal/agent/agent.go:10-52`. New kinds are
appended. `String`/`ParseKind` are at `:54-104` and `Interactive()` at
`:123-125`.

| Kind | Notes |
|---|---|
| `ProjectAgent` | Turns `project.md` into `project.yaml`. |
| `PlanningAgent` | Writes `tasks/*.yaml`. |
| `TaskAgent` | Does one task in a worktree. |
| `WolfAgent` | Interactive; for a blocked project. |
| `CommitAgent` | Commits work. |
| `ArchiveAgent` | Final push and archive. |
| `ReviewAgent` | PR loop through the github MCP. |
| `WorkingmanAgent` | Always-on, persistent, autonomous; has no project. |
| `SleepyHeadAgent` | Interactive; catch-up for missed cron ticks. |

`Spec{Kind, Name, Title, Workspace, Command}` is at `:131-139`.

### 4.2 How the workingman agent is launched (the model for Step-Up)

1. **Flag.** `--workingman-agent` takes auto/on/off (`cmd/orch/main.go:119`;
   `cmd/orch/workingman_agent.go:5-60`). It is on whenever `--acp-kit` is set.
2. **Option.** `daemon.WithWorkingmanAgent(WorkingmanAgentConfig{AuditLog})`
   (`main.go:290-300`).
   - Config fields (`internal/daemon/workingman_agent.go:41-65`): `Dir`,
     `ExtraReadOnlyMounts`, `AuditLog`, `Policies []policy.Rule`, and
     backoff settings.
3. **Scratch dir.** `<sessions-root>/../workingman-agent` (`:153-155`). It is
   rejected if it overlaps a root (`:178-186`).
4. **Start.** `Run` calls `reconcileSessions` (`daemon.go:337`), then
   `beginWorkingmanAgent(ctx)` (`:342`; defined at `workingman_agent.go:384-404`).
5. **Supervision.** `superviseWorkingmanAgent` (`:274-324`) restarts it on
   exit, backing off from 5 s up to 5 min and resetting after 10 min stable
   (`:31-36`, `:246-261`). It starts through `startSessionTracked`
   (`dispatch.go:761-776`).
6. **Plan.** `workingmanPlan` (`workingman_agent.go:195-231`):
   - `Persistent: true`, session name `workingman-agent`;
   - `WritableMounts` = every orch root;
   - `ReadOnlyMounts` = the snapshot, audit-log and sessions directories;
   - `Observe{…}` feeds the prompt.
7. **Runner.**
   - `StartConfigured` (`internal/runner/runner.go:597-615`) renders the
     prompt (`:655-685`), writes `.orch/` (`:687-713`) and calls `startACP`
     (`:971-1041`).
   - **Sandbox name:** fixed by `ACPSandboxNameFor` (`:1594-1598`).
   - **Mounts:** `workingmanWorkspaces` (`:987-989`, `:1693-1727`).
   - **acp-wrapper flags** (`:1139-1253`): `--persistent` with no idle timeout
     (`:1170-1176`), `--require-okta-gateway` (`:1231`), the MCP gateway flags
     (`:1234-1245`) and `--policy` (`:1246`).
   - **MCPs:**
     - `MCPServersFor` (`:1290-1295`) gives the task `static_mcps`, otherwise
       the `--mcp-gateway-servers` default;
     - `MCPGatewayDynamic` (`:1280-1288`) is **true** for workingman unless
       `--workingman-agent-mcp-dynamic=false`.
8. **Lifecycle special cases:**
   - `detachable` returns false (`daemon.go:462-467`);
   - reconcile re-adopts it (`reconcile.go:72-83`);
   - the reaper exempts it (`reaper.go:141-143`);
   - sessions and snapshot have special cases (`sessions.go:86-93`,
     `snapshot_live.go:371-374`).
9. **Talking to it.** `WorkingmanAgentSession()` (`workingman_agent.go:117-148`)
   gives the socket path. The router attaches over `agent.sock`
   (`cmd/orch/channels.go:161-167`, `router/turn.go:147-157`), and so does the
   TUI.

**Sleepy-head** (`internal/daemon/sleepy_head.go`) is the unsupervised,
one-shot variant:

- it is launched only when schedules were missed (`daemon.go:347`,
  `:159-194`);
- it mounts only the behind projects (`:242-270`);
- it asks for state changes by writing a `catchup-request` marker file
  (`:38-63`). The daemon's watcher routes that to `handleCatchup`
  (`watcher.go:118-120`). **This is the existing precedent for "an agent asks
  the daemon to do something".**

### 4.3 Prompts

- Templates are embedded (`internal/prompts/prompts.go:14-15`).
- `Render(kind, data)` picks `kind.String()+".tmpl"` (`:119-131`), so a
  `StepUpAgent` whose `String()` is `"step-up"` needs
  `templates/step-up.tmpl`.
- `Data` (`:19-85`) has per-kind fields. Workingman uses `Roots`,
  `SnapshotFile`, `AuditLog` and `SessionsRoot`. Mirror them into
  `setup.Context` (`internal/setup/setup.go:15-52`) and `runner.go:655-710`.
- `workingman.tmpl:158-250` tells the agent that file edits are the only API
  and that host actions (sbx, wsp) belong to the human. Step-Up's prompt has
  to say the opposite for its own request channel.

### 4.4 Sandbox policies today

- **Types** (`internal/policy/policy.go`):
  `Rule{Action allow|deny; Kind network|filesystem; Resource}` (`:39-43`),
  with `Validate` (`:49`), `CLIArgs(sandbox)` giving
  `policy <action> <kind> --sandbox <name> <resource>` (`:68-76`), and
  `Encode`/`Decode` (`:88-109`).
- **Declared** per task under `policies:` (`internal/task/task.go:208-215`).
  `project.yaml` has no policy field.
- **Built-in rules:**
  - archive allows the git host (`internal/daemon/dispatch_lifecycle.go:565-575`);
  - review allows the GitHub APIs (`review.go:595-602`);
  - the MCP gateway rules (`internal/mcpgateway/mcpgateway.go:235-253`).
- **Applied** by `acp-wrapper` on the host, inside `createSandbox`, after
  `sbx create` (`internal/acpwrapper/acpwrapper.go:565-620`, loop
  `:605-613`).
  - A reused sandbox re-applies **only the gateway rules**
    (`reapplyGatewayPolicies`, `:694-706`).
  - A changed workspace set forces a recreate (`:680-687`).
  - The legacy creator is `runner.go:295-353`.
- **Live changes:** nothing in orch runs `sbx policy` against a running
  sandbox, nor `sbx policy rm` or `ls`. Mechanically,
  `sbx policy allow … --sandbox <name>` works on a live sandbox;
  `reapplyGatewayPolicies` does exactly that.
  - **Caveat:** editing a task's `policies:` alone does nothing until the
    sandbox is recreated.
- Sandboxed agents cannot run `sbx` (`prompts.go:56-64`).

### 4.5 "Direct access to the workingman service": what it can concretely mean

The daemon has **no control API**. Its only listeners are acp-wrapper's
`agent.sock` (which carries ACP, not daemon control) and the WhatsApp webhook
and bridge. External change happens in two ways:

- file edits picked up by fsnotify (`watcher.go:113-137`), with the self-write
  filter `UpdatedBy == WriterDaemon` (`dispatch.go:66`);
- marker files.

| Option | Mechanism | Assessment |
|---|---|---|
| (a) File requests | The Step-Up sandbox gets a read-write mount of `~/.workingman/step-up/requests/`, outside the roots, so it needs its own watcher add. It writes `<id>.yaml` such as `{op: sandbox_policy, sandbox, rules[], reason, approval_id}`. The daemon validates, runs `policy.Rule.CLIArgs` through `sbx`, appends the rule to the target task's `policies:` so a recreate keeps it, writes `<id>.result.yaml`, and audits. | **Smallest**; mirrors `catchup-request`. Only Step-Up has the mount, so the mount is the capability. The result is asynchronous, so the agent polls the result file. |
| (b) Daemon unix or HTTP API | Something like `~/.workingman/state/control.sock` mounted into the sandbox, or `host.docker.internal:<port>` plus an allow rule. | Entirely new listener and authentication. Socket bind-mounting through sbx is unverified. |
| (c) `workingman-admin` MCP | Typed tools (`apply_sandbox_policy`, `set_task_status`, `unblock_project` …) served by the daemon or post-office, listed in the catalog, and granted only through Step-Up's static MCPs. | Best ergonomics; reuses gateway scoping and identity (§5). Requires orch to *host* MCP, which it does not do today (it is only a client, `review_mcp.go`). The same capability and dynamic-gateway caveats as post-office apply. |
| (d) Mounts only | As the workingman agent does: read-write orch roots. | Lets it edit project and task YAML, but **cannot touch a live sandbox**. Useful in addition to the others, not as the answer. |

**Recommendation.**

- Start with (a) for host-side effects, plus (d)-style read-write mounts of
  the roots for project and task state. That is the workingman agent's
  existing power.
- The daemon is the only component that runs `sbx`. It should enforce:
  - an allowlist of operations;
  - "rule must name a sandbox that is a live task sandbox";
  - `approval_id`, which must reference an answered post-office request when
    the change widens access.
- Every change is audited.
- Move to (c) once post-office proves out MCP hosting in Go. The request
  schema becomes the tool schema.

### 4.6 Places a `StepUpAgent` kind touches

1. **`internal/agent/agent.go`:** the constant (appended), `String`,
   `ParseKind`, and an `Interactive()` decision. It is autonomous like
   workingman, not interactive.
2. **`internal/prompts/templates/step-up.tmpl`:** new `Data` fields (requests
   dir, post-office tool usage), mirrored in `setup.Context`.
3. **`internal/runner/runner.go`:**
   - sandbox and session name constants (`:192-203`) and the
     `ACPSandboxNameFor` case (`:1594`). Without them `startACP` fails on an
     empty name (`:973`);
   - the `StartConfigured` scratch dir and persistent handling (`:599-629`);
   - the mount helper (`:987`);
   - the idle-timeout exemption (`:1172`), `--require-okta-gateway`
     (`:1231`), the `acpGateway` fallback (`:1259-1271`), and
     **`MCPGatewayDynamic` must return false for it** (`:1280`).
4. **`internal/daemon`:**
   - a `step_up.go` copying `workingman_agent.go`: config, option and
     supervisor, with `StaticMCPs: ["post-office"]` in its plan;
   - a field on `Daemon` (cf. `daemon.go:137`) and a `Run` hook (cf. `:342`);
   - `detachable`, `reconcile.go:72-94`, `reaper.go:141`,
     `sessions.go:86-93` and `snapshot_live.go:371`;
   - the request-dir watcher and executor.
5. **`cmd/orch/main.go`:** a `--step-up-agent` flag and the option (cf.
   `:119`, `:290-300`).
6. **Router (optional):** a `/stepup` command and a `Source.StepUp()`, so the
   operator can talk to it from a phone, like `/sleepy`.
7. **`agents.md`:** a new section.

Step-Up **receives** agent-to-agent requests through post-office's
`next_message` and `reply` tools (mcp-async-requests.md §5, "Receiver side").
Its `reply` writes the same `outbox/<id>.json` claim, so the requesting
agent's `wait_for_response` completes the same way an operator answer does.

---

## 5. How agents get MCP servers, and caller identity

### 5.1 Today's path

1. **Declaration:** `task.StaticMCPs` (`yaml:"static_mcps"`,
   `internal/task/task.go:198-206`).
   - It is forwarded into `runner.Plan.StaticMCPs`
     (`internal/runner/runner.go:51-59`) at `internal/daemon/dispatch.go:1277`,
     `dispatch_lifecycle.go:374` and `reconcile.go:190-196`.
   - Review hard-codes `reviewStaticMCPs = {"github"}`
     (`internal/daemon/review.go:246`). That is the pattern for a per-kind
     default set.
2. **Resolution:**
   - `MCPServersFor` (`runner.go:1290-1295`) uses the task list, otherwise
     `--mcp-gateway-servers` (`cmd/orch/main.go:124`).
   - Dynamic is on by default for Task, Wolf and Workingman
     (`runner.go:1280-1288`).
3. **Minting:** acp-wrapper builds an `mcpgateway.Manager`
   (`cmd/acp-wrapper/main.go:138-146`, `:174-181`).
   - `Client.Create` (`internal/mcpgateway/mcpgateway.go:109-154`) sends
     `POST /gateways {"servers":[…],"dynamic":bool}` and gets back
     `201 {id,url,servers,dynamic}`. Unknown names are dropped and the call
     retried (`manager.go:115-156`).
   - On the server side (`standalone-codemode/internal/server/gateways.go:79-121`)
     the endpoint is **unauthenticated**. The UUID is the capability
     (`aggregategw/registry.go:57-92`, `:248-257`).
4. **Record:** `~/.workingman/gateways/<sandbox>.json` (`store.go:18-36`,
   `:48-50`) holds `{sandbox, id, url, requested, servers, dynamic, …}`. It
   maps **gateway id → sandbox**.
5. **Reaching it from the sandbox:**
   - URL: `SandboxURL` rewrites loopback to `host.docker.internal`
     (`mcpgateway.go:193-209`);
   - network: allow rules (`:235-254`);
   - environment: `MCP_URL` is set at create, on exec, and in
     `/etc/sandbox-persistent.sh` (`manager.go:370-392`,
     `acpwrapper.go:410`, `:715-728`);
   - Claude config: acp-kit runs `claude mcp add --transport http okta-gateway $MCP_URL`
     (`docs/mcp-gateway.md:136-187`).
6. **Teardown:** `Release` (`manager.go:327-344`) and `Sweep` (`:349-363`).

### 5.2 Granting post-office selectively

- **Opt-in.** Add `static_mcps: [post-office]` on the tasks that may message.
  The planning agent's prompt should say when to add it. The Step-Up plan
  hard-codes it, as review does.
- **Never** add it to `--mcp-gateway-servers`.
- **Close the dynamic gap.** `mcp-add` can attach any catalog server
  (`standalone-codemode/internal/aggregategw/manager.go:372-410`). post-office
  must reject a caller whose `Record.Servers` lacks `post-office`.
  `Record.Servers` reflects only what the gateway was minted with, so the
  check is sound.
- Optionally add a catalog-level "not dynamically addable" flag in
  `standalone-codemode/internal/gatewaycatalog/catalog.go`.

### 5.3 Learning the caller's identity

The gateway forwards nothing that identifies the caller today:

- backend headers are static catalog or placeholder headers plus
  header-script output;
- every gateway uses `staticGatewayID = "static"`
  (`aggregategw/manager.go:31`, `:303`);
- `POST /gateways` takes only `servers` and `dynamic`
  (`gateways.go:39-44`).

The recommended chain (detail in mcp-async-requests.md §6):

1. **standalone-codemode** generates the sub-gateway UUID *before* `Build`
   (today it is assigned after, `registry.go:74-78`). It injects
   `X-Codemode-Gateway-Id` on backend attach (`Manager.attachSpec`,
   `manager.go:762-779`, plus the dynamic options `:399-402`), or resolves a
   reserved `${gateway_id}` header placeholder.
2. **post-office** looks the id up in `mcpgateway.Store.List()` to get
   `{sandbox, servers}`.
3. **Sandbox → agent:**
   - fixed names identify the singleton agents (`workingman-agent`,
     `sleepy-head`, and a new `step-up`);
   - otherwise the sandbox name is derived from project and task by
     `runner.SandboxNameFor` (`runner.go:1544`) and `ACPSandboxNameFor`
     (`:1594`). The reverse lookup is cheapest via `session.json` under
     `~/.workingman/sessions/*`, which already records `sandbox_name`,
     `project_path`, `task_path` and `kind`
     (`internal/session/session.go:52`, `:105`, `:112`, `:117`).
   - The result is stamped into `inbox/<id>.json` `from:{sandbox, project, task, agent}`.
4. **Reject:**
   - calls with no id (the parent `/mcp`);
   - unknown ids;
   - ids whose record lacks `post-office`.

Each sub-gateway attaches its own backend connection
(`aggregategw/manager.go:191-222`). That is what makes per-attach header
injection possible.

The alternative, per-gateway header overrides in the `POST /gateways` body,
lets workingman stamp project and task directly. But because `POST /gateways`
is unauthenticated and reachable from sandboxes on port 3333, a sandbox could
forge them. Use it only together with authenticating that endpoint.

---

## 6. Consolidated change list

**workingman**

| Area | Change |
|---|---|
| `internal/postoffice` (new) | `Store` (inbox/outbox/done, O_EXCL claim, atomic writes), `Dispatcher` (inbox watch → `SendTopic` + index register; outbox watch → "answered elsewhere"), identity resolver (gateway record → sandbox → project/task). `doc.go` with `Role: shared`. |
| `cmd/post-office` (new) | HTTP MCP per mcp-async-requests.md, built on `internal/postoffice`. |
| `internal/channels` | `ConversationKindPostOffice` constant; `"slack"` in `inboundTypes`; optionally an `Editor` interface. |
| `internal/channels/slack` (new) | Channel over a `ToolCaller`; poll loop; access policy; `ts` IDs. |
| `internal/daemon` | Generalise `review_gateway.go` into a reusable daemon gateway (Slack). Add the Step-Up supervisor and the request executor (`sbx policy`, persist to task `policies:`, audit). |
| `internal/router` | `process`: post-office reply-to → `Store.Answer`; "already answered" reply; optional `/inbox`, `/stepup`. |
| `cmd/orch` | Slack factory and `orch slack …`; build the `Dispatcher` in `channels.go`; `--step-up-agent`; pass the inbox dir to `tui.Run`. |
| `internal/tui` | `inbox.go` (watcher, view, keys), `modeInbox`, badge in `renderFooter` and the ACP header, `i`/`I` keys, shared line input. |
| `internal/agent`, `internal/prompts`, `internal/runner`, `internal/setup` | `StepUpAgent` kind, `step-up.tmpl`, names, mounts, static (non-dynamic) MCPs. |
| Docs | `agents.md`, `docs/channels.md` (Slack and post-office topic), `docs/packages.md` (`task docs`). |

**standalone-codemode**

- Forward the sub-gateway id to backends.
- Optionally add a non-addable catalog flag.
- Add a `post-office` catalog entry with a bearer token.
- Add a Slack entry to the personal catalog, if the personal setup needs
  Slack.

## 7. Open questions

1. **Slack MCP tools.** Which tools exist (post, history, replies, reactions,
   update)? Does reading need a user-scoped token? This decides between
   polling and anything push-like, and whether "answered elsewhere" can
   *edit* the Slack message. It is covered by `research-slack-mcp`.
2. **Topic routing.** Should post-office get its own topic (`post-office`)
   with explicit routes, or use `*`? With no routes for the topic,
   `RoutesFor` falls back to `*` (`registry.go:425-441`), which is probably
   what "all active channels" means.
3. **Agent-addressed requests.** Should requests to Step-Up also appear in
   the operator's TUI inbox, as read-only, for visibility?
4. **Approval policy.** Which Step-Up changes must go through a human
   request, and which can it make on its own (an allowlist in its config)?
5. **Live and persisted policy.** Should the daemon re-apply task
   `policies:` on sandbox reuse generally? That would close the gap noted at
   `acpwrapper.go:694-706`.
6. **Several sessions per sandbox.** A reused sandbox can have more than one
   `session.json` over time, for example a task agent followed by its commit
   agent. Decide whether the newest live session is the right attribution.
