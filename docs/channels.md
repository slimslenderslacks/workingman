# Messaging channels (WhatsApp, Signal and Slack)

With a channel configured, the daemon talks to a human on their phone in both
directions:

- **Outbound** — a `🐺 wolf is running for <project>` message the moment a wolf
  agent has actually started, and a `🐺 wolf finished` message when it ends
  (see [wolf-notify.md](wolf-notify.md)).
- **Post-office** — questions and notifications that agents send the operator
  through the post-office MCP (`❓ [req r1t8w] …`, `📣 [note …] …`), fanned out
  to every channel by the daemon's **Dispatcher**; see
  [Post-office messages](#post-office-messages).
- **Inbound** — everything the owner sends back is handed to the **router**
  (`internal/router`): a *reply* to a wolf message goes to that wolf, a reply
  to a post-office request answers it, anything else goes to the always-on
  **workingman agent**, which answers questions about the orch state
  ([agents.md §8](../agents.md#8-workingman-agent)).

This mirrors hermes-agent's WhatsApp gateway (`gateway/platforms/whatsapp_cloud.py`,
`whatsapp_common.py`, `scripts/whatsapp-bridge/`) and Signal adapter
(`gateway/platforms/signal.py`); see the parity tables in the
[README](../README.md#channels--whatsapp) and
[below](#signal). Signal has its own page: [signal.md](signal.md).

Contents: [Architecture](#architecture) · [Quick start](#quick-start) ·
[Configuration reference](#configuration-reference) · [Signal](#signal) ·
[Slack](#slack) ·
[Routes and topics](#routes-and-topics) ·
[Routing rules](#routing-rules) ·
[Post-office messages](#post-office-messages) · [Security model](#security-model) ·
[Tunnel setup](#exposing-the-webhook-tunnel-setup) · [Troubleshooting](#troubleshooting) ·
[Manual smoke test](#manual-smoke-test-real-meta-test-number) ·
[Signal smoke test](signal.md#manual-smoke-test) ·
[Slack smoke test](#slack-smoke-test)

## Architecture

```mermaid
flowchart LR
    phone([Owner's phone])

    subgraph meta["Meta (WhatsApp cloud mode)"]
        graphapi[Graph API]
        hook[Webhook delivery]
    end
    tunnel[[HTTPS tunnel<br/>cloudflared / ngrok / tailscale]]

    signalsvc[(Signal service)]
    sigcli["signal-cli daemon<br/>--http 127.0.0.1:8080"]

    subgraph orch["orch daemon (host)"]
        direction TB
        chan["whatsapp channel<br/>webhook :8090 + Cloud client<br/>(or the Baileys bridge)"]
        sigchan["signal channel<br/>SSE in, JSON-RPC out"]
        policy{"access policy<br/>per channel, allowlist, default deny"}
        router["router<br/>commands, reply-to, binding"]
        index[("conversation index<br/>channel-conversations.json")]
        notify["wolf start/end<br/>notifications"]
        daemon["daemon<br/>state machine + snapshot"]
        acpchat["acpchat<br/>(joins a live ACP session)"]
    end

    subgraph sandboxes["sbx sandboxes (one ACP session each)"]
        wolf["wolf agent<br/>per blocked project"]
        wm["workingman agent<br/>observer<br/>(+ edits project/task files on request)"]
    end

    phone <--> graphapi
    phone <--> signalsvc
    signalsvc <--> sigcli
    hook --> tunnel --> chan
    sigcli -->|"SSE events"| sigchan
    sigchan -->|"JSON-RPC send"| sigcli
    chan -->|"signed POST, verified"| policy
    sigchan --> policy
    policy --> router
    router --> acpchat
    acpchat <-->|"unix socket agent.sock"| wolf
    acpchat <-->|"unix socket agent.sock"| wm
    router -->|"replies"| chan
    router -->|"replies"| sigchan
    daemon -->|"project blocked: launch"| wolf
    daemon --> notify
    notify -->|"send + register message id"| chan
    notify -->|"send + register message id"| sigchan
    notify --> index
    router -->|"reply-to lookup"| index
    daemon -. "snapshot.json, audit log,<br/>sessions (read-only mounts)" .-> wm
    chan -->|"send"| graphapi
```

Two things to keep in mind:

- The daemon never runs the agents' conversations itself. Each agent is a
  persistent ACP session (`acp-wrapper --persistent`) in its own sandbox; the
  router *attaches* to it (`acpchat.Attach`) over the session's unix socket,
  exactly as the TUI does, so the TUI, the daemon and the phone all share one
  conversation.
- Only messages that pass the channel's access policy reach the router. The
  router does no authorisation of its own.
- Channels are independent transports behind one registry: WhatsApp and Signal
  can run at the same time.

## Quick start

```sh
orch whatsapp setup        # credentials, owner allowlist, webhook; prints what to paste into Meta
orch whatsapp status       # masked config, Graph reachability, webhook listener
orch whatsapp test         # send a test message to the owner

# the workingman agent turns itself on whenever --acp-kit is set
orch --root ~/orch --acp-kit <kit> --headless
```

`orch whatsapp setup` writes `~/.workingman/channels.yaml` (no secrets) and a
0600 `~/.workingman/secrets.yaml`. For a personal number via the Baileys bridge
use `orch whatsapp pair` instead ([whatsapp-bridge.md](whatsapp-bridge.md)).
For Signal use `orch signal setup` / `status` / `test` instead (no tunnel, no
console, no secrets file; it needs a running signal-cli daemon): see
[Signal](#signal) and [signal.md](signal.md). For Slack use `orch slack setup` /
`status` / `test` (no token at all; it needs the host's standalone-codemode
gateway with the `slack` server): see [Slack](#slack).

Running with the TUI (no `--headless`) works the same way; the difference is who
starts a conversation in a new agent session: the TUI's watcher normally sends a
session's opening prompt. Headless, the router does it for the workingman agent
(it creates the ACP session and sends the standard "read
`.orch/instructions.md`…" opening prompt before the first question). A *wolf*
under `--headless` is not primed by anyone, so it launches but sits idle until a
TUI attaches; run the TUI (or `orch tui`) when you want wolves to work.

## Configuration reference

`orch --channels-config <file>` — default `~/.workingman/channels.yaml`, or
`$WORKINGMAN_CHANNELS_CONFIG`. A missing file (or one with every channel
`enabled: false`) disables channels and the daemon behaves exactly as before.
Unknown keys are errors, so a typo cannot silently weaken the config.

### Worked example (WhatsApp Cloud API)

```yaml
# ~/.workingman/channels.yaml
channels:
  whatsapp:                              # instance name (lowercase, [a-z0-9_-])
    type: whatsapp
    # enabled: false                     # keep the config but switch it off
    credentials:                         # references only — never inline values
      access_token: {file: ~/.workingman/secrets.yaml, key: whatsapp_access_token}
      app_secret:   {file: ~/.workingman/secrets.yaml, key: whatsapp_app_secret}
      verify_token: {file: ~/.workingman/secrets.yaml, key: whatsapp_verify_token}
      # or from the environment:  {env: WHATSAPP_ACCESS_TOKEN}
    options:
      mode: cloud                        # cloud (default) | bridge
      phone_number_id: "109876543210987" # Meta's id for the sending number (not the phone number)
      # waba_id: "123456789"             # also bind the webhook to your business account
      # api_version: v20.0
      # graph_base_url: https://graph.facebook.com
      webhook_host: 127.0.0.1            # loopback by default; front it with a tunnel
      webhook_port: 8090
      webhook_path: /whatsapp/webhook
      # insecure_skip_signature: true    # DANGEROUS: accept unsigned POSTs (only without an app secret)
      access:                            # who may talk to the daemon; empty = nobody
        allow_from: ["+1 555 123 4567"]  # the owner
        # deny_reply: "Sorry, I can't chat with you."   # off by default: strangers get silence
        # deny_reply_interval: 1h

routes:                                  # where the daemon's own messages go
  - {topic: wolf,       channel: whatsapp, chat: "15551234567"}
  - {topic: workingman, channel: whatsapp, chat: "15551234567"}
  # - {topic: "*",      channel: whatsapp, chat: "15551234567"}   # everything else

notify:
  wolf_start_interval: 10m               # at most one wolf-start message per project per interval (0s = no limit)

router:                                  # inbound router (all optional)
  permission_timeout: 2m                 # unanswered agent permission requests are rejected after this
  turn_timeout: 5m                       # a turn is cancelled after this long
  thinking_after: 10s                    # "…thinking" ack for turns longer than this
  queue_size: 5                          # messages queued per chat behind the running turn
```

```yaml
# ~/.workingman/secrets.yaml   (chmod 600 — the daemon refuses anything looser)
whatsapp_access_token: EAAG...
whatsapp_app_secret: 0123456789abcdef0123456789abcdef
whatsapp_verify_token: a-long-random-string
```

### Keys

| Key | Meaning |
|---|---|
| `channels.<name>.type` | selects the factory: `whatsapp` or `signal` |
| `channels.<name>.enabled` | default `true` |
| `channels.<name>.credentials.<cred>` | `{env: NAME}` or `{file: PATH, key: KEY}`; a file must be mode 0600. WhatsApp cloud credentials: `access_token` (required), `app_secret` (required for the webhook), `verify_token` (the subscription handshake) |
| `…options.mode` | `cloud` (default) or `bridge` |
| `…options.phone_number_id`, `waba_id`, `api_version`, `graph_base_url` | Cloud API identity and endpoint |
| `…options.webhook_host` / `webhook_port` / `webhook_path` | where the webhook listens (default `127.0.0.1:8090/whatsapp/webhook`; `/health` is reserved) |
| `…options.insecure_skip_signature` | accept unsigned POSTs; only honoured when no app secret is set |
| `…options.access.mode` | `bot` (default, a number of its own) or `self-chat` (bridge only) |
| `…options.access.allow_from` | admitted senders (phone numbers or LIDs); `*` is rejected |
| `…options.access.allow_all` | **dangerous** opt-in: admit everyone, logs a loud warning |
| `…options.access.groups`, `group_allow_from`, `require_mention`, `mention_patterns`, `free_response_chats` | group chats (bridge only; the Cloud API serves direct chats) |
| `…options.access.deny_reply`, `deny_reply_interval` | optional reply to a denied sender, rate-limited per sender |
| `…options.access.self_ids`, `reply_prefix`, `forward_owner_messages` | self-chat / owner-message handling (bridge) |
| `…options.bridge.*` | Baileys bridge supervision: see [whatsapp-bridge.md](whatsapp-bridge.md) |
| Signal: `…options.http_url`, `account`, `plain_text`, `access.allow_from` / `note_to_self` / `groups` / `group_allow_from` / `allow_all`; `credentials.account` | see [signal.md](signal.md#configuration) |
| `routes[]` | `{topic, channel, chat}`, plus `notify: false` on a `post-office` route to keep notifications off it; see below |
| `notify.wolf_start_interval` | rate limit for wolf-start messages |
| `router.*` | inbound router timeouts and queue |

Daemon flags that matter here: `--channels-config`, `--workingman-agent[=auto|on|off]`,
`--acp-kit`, `--sessions-root`, `--state-file`, `--wolf-host`,
`--wolf-unblock-grace`, `--wolf-idle-timeout`, `--headless` (`orch --help`).

## Signal

Signal is a second transport behind the same registry, routes and router, so
everything above (wolf-start messages, quote-replies routed through the
conversation index, the allowlist) works the same. It talks to a
[signal-cli](https://github.com/AsamK/signal-cli) daemon (`signal-cli -a
+15551234567 daemon --http 127.0.0.1:8080`): inbound over its SSE stream,
outbound as JSON-RPC. Compared with WhatsApp cloud there is **no Meta console,
no webhook, no tunnel and no 24-hour window**. The full guide, including the
step-by-step "bind the workingman agent and the wolf to Signal", is
[signal.md](signal.md); the short version:

```sh
orch signal setup    # signal-cli URL, account, owner allowlist, optional groups; writes the signal section and routes
orch signal status   # masked config + signal-cli reachable / account registered   (--offline skips the check)
orch signal test [--to <number|uuid|group:ID>] [message]
```

`setup` is interactive, or driven by flags (`--http-url`, `--account`,
`--allow-from`, `--note-to-self`, `--groups`, `--group-allow-from`,
`--route-topics`, `--no-routes`) / `SIGNAL_HTTP_URL`, `SIGNAL_ACCOUNT`,
`SIGNAL_ALLOW_FROM` with `--non-interactive`. It is idempotent, preserves the rest
of `channels.yaml`, adds `wolf` and `workingman` routes to the first owner, and
refuses to save when signal-cli is unreachable or the account is not registered
unless `--skip-validate` is given.

### Worked example (Signal)

```yaml
# ~/.workingman/channels.yaml
channels:
  signal:
    type: signal
    options:
      http_url: http://127.0.0.1:8080      # default
      account: "+15551234567"              # the number signal-cli is registered or linked as
      access:
        allow_from: ["+15557654321"]       # the owner; E.164 numbers or UUIDs; empty = nobody
        # note_to_self: true               # admit the account's own "Note to Self" chat
        # groups: true
        # group_allow_from: ["<group id>"] # groups need an explicit list

routes:
  - {topic: wolf,       channel: signal, chat: "+15557654321"}
  - {topic: workingman, channel: signal, chat: "+15557654321"}
```

No credentials are required; the account number may instead be given as
`credentials: {account: {env: SIGNAL_ACCOUNT}}`.

### Signal alongside WhatsApp

Define both channels (different instance names) in the same file. A topic routed
to both is sent on both; a reply is answered on the channel the question arrived
on; each chat keeps its own `/wolf` / `/agent` binding. Give the same topic one
route per channel:

```yaml
routes:
  - {topic: wolf, channel: whatsapp, chat: "15551234567"}
  - {topic: wolf, channel: signal,   chat: "+15557654321"}
```

### Reply-to routing on Signal

Signal's *Reply* sends a quote whose id is the quoted message's timestamp. The
channel returns that timestamp as the id of every message it sends, so the
conversation index maps a swipe-reply to a `🐺 wolf is running…` message back to
that wolf, exactly as WhatsApp's `context.message_id` does. Outbound, the first
chunk of a reply quotes the message it answers in direct chats (not in groups).

## Slack

Slack is a third transport behind the same registry, routes and router. It
has no Slack app or token of its own: it talks to the **Slack MCP** behind the
host's standalone-codemode gateway (`internal/channels/slack`, design:
[messaging-design.md §6](messaging-design.md#6-slack-channel), research:
[research/slack-mcp.md](research/slack-mcp.md)). The channel mints its own
static sub-gateway over the `slack` catalog server (record
`~/.workingman/gateways/daemon/orch-slack.json`, deleted on shutdown) and the
gateway's host header script is the authentication. Slack is a **work setup
only** channel: `mcp-servers-personal.json` has no Slack entry.

```yaml
# ~/.workingman/channels.yaml
channels:
  slack:
    type: slack
    options:
      gateway_url: http://localhost:3333   # default
      channel: U0CC1K1MFNX                 # a channel id, or your own user id for the DM-to-self
      allow_from: [U0CC1K1MFNX]            # Slack user ids; empty = nobody
      poll_interval: 20s                   # default; min 5s
      watch_ttl: 168h                      # how long a request's thread is watched (default 7 days)
routes:
  - {topic: post-office, channel: slack, chat: U0CC1K1MFNX}
```

Unknown options are rejected, and so is any `credentials:` block.

**What is different about Slack:**

- **Posts are made as you.** The MCP uses your own user token, so every post
  the daemon makes has your name on it, and Slack will most likely **not
  push-notify you** about it. Treat Slack as a place to answer, not a place
  that pings you: keep Signal, WhatsApp or the TUI for alerts. Because the
  author cannot tell our posts from yours, the channel records the ts of
  everything it posts (`~/.workingman/slack/sent.json`, 30 days) and treats
  every other message as yours.
- **No push: replies are polled.** Every `poll_interval` the channel reads the
  conversation (`slack_read_channel`). A new top-level message from you goes
  to the router like any chat message (so `/inbox`, `#r1t8w staging` and
  questions for the workingman agent all work). A sent post-office request is
  *watched* (`~/.workingman/slack/watch.json`); when its thread's reply count
  rises, the thread is read (`slack_read_thread`) and each reply answers the
  request. If the read text format ever stops parsing, `channels.log` says so
  loudly and the channel falls back to reading each watched thread.
- **One call at a time.** The host header script fails under concurrent
  calls, so every Slack call runs on one worker, and a `header script failed`
  error is retried (1 s, 2 s, 4 s, 4 s). A send whose response is lost is
  looked up in Slack rather than posted twice.
- **"Answered elsewhere"** cannot edit the original message. The channel
  implements `channels.Resolver`: a thread reply with the closing note, and a
  reaction on the request — ✅ answered, ⌛ expired, 🚫 cancelled. A later
  reply in a closed thread gets `This request is already closed (answered via
  tui).` and goes nowhere.
- `allow_from` is checked before the router sees a message. In the DM-to-self
  only you can write; in a shared channel everyone else is dropped and logged
  (without their text).

### Setting it up: `orch slack`

```sh
orch slack setup     # gateway URL, conversation (default: your DM-to-self), allow_from; writes the slack section and the post-office route
orch slack status    # config, gateway serves the slack tools, daemon gateway record, watch list and cursors   (--offline skips the gateway)
orch slack test [--to <id>] [--wait 3m] [--no-wait] [message]   # post a test message, wait for your reply in its thread
```

All three reach Slack the way the daemon does, but on a **temporary**
sub-gateway of their own (`POST /gateways` over `slack`, deleted on exit), so
they work whether or not the daemon is running and never touch the daemon's
`orch-slack` gateway. A gateway that comes up serving the slack backend with
"0 tools" is replaced, up to three times.

- **`setup`** mints the gateway, reads your profile
  (`slack_read_user_profile`, which is who every post is made as) and lists
  your conversations (`slack_list_user_channels`, public and private
  channels and DMs). Pick one by number, type an id, or type text to filter
  by name; Enter keeps the current value or, for a new channel, takes the
  **DM-to-self** (your own user id). `allow_from` defaults to you. It then
  writes `channels.slack` and a `{topic: post-office, channel: slack, chat:
  <conversation>}` route with the same `yaml.Node` editor as `orch signal
  setup` (`internal/channels/yamledit`): comments, other channels and routes,
  and the options it does not manage (`watch_ttl`) are kept, a route that
  already exists is not added twice, and nothing is written if the result
  does not validate. Flags: `--gateway-url` (`$WORKINGMAN_MCP_GATEWAY_URL`),
  `--conversation` (`$SLACK_CHANNEL`), `--allow-from` (`$SLACK_ALLOW_FROM`),
  `--poll-interval`, `--route-topics` (default `post-office`), `--no-routes`,
  `--non-interactive`, `--skip-validate` (no gateway; then `--conversation` is
  required), and `--list`, which only prints the conversations and their ids.
  `--channel <name>` manages another instance (its daemon gateway is then
  `orch-slack-<name>`).
- **`status`** prints the config and warns about an empty `allow_from` or a
  missing post-office route (a `*` route counts when there is no post-office
  route at all). Online, it checks the gateway mints with `slack` serving and
  serves every tool the channel calls (`slack_send_message`,
  `slack_add_reaction`, `slack_read_channel`, `slack_read_thread`), shows the
  daemon's own gateway record (`~/.workingman/gateways/daemon/orch-slack.json`)
  and whether that gateway is still registered, and then the channel state in
  `~/.workingman/slack/`: the resolved conversation id, the top-level cursor,
  the size of the sent set, and each watched request with its reply count,
  thread cursor and open/closed state. It exits 1 when something needs
  attention. `--sessions-root` locates `~/.workingman` the way the daemon does.
- **`test`** posts `[orch slack test] …` to the configured conversation (or
  `--to`) and polls its thread until you reply (default 3 minutes), then adds
  ✅ to it and checks your reply's author is in `allow_from`. That is the
  whole round trip the channel depends on: post, read a thread reply, react.
  The daemon's poll ignores top-level Slack MCP posts starting with
  `[orch slack test]`, so a running daemon neither forwards the test message
  to the workingman agent nor watches its thread.

### Slack smoke test

Run once against the real Slack MCP, on the work setup (the gateway's catalog
must have `slack`). Everything goes to your DM-to-self unless you pick
another conversation.

1. [ ] `curl http://localhost:3333/healthz` answers 200.
2. [ ] `orch slack setup --list` prints `U… DM-to-self (you)` and your
   channels with their ids, and writes nothing.
3. [ ] `orch slack setup`, pressing Enter at every prompt: it reports
   `signed in to Slack as <you> (U…)` and saves `channels.slack` with
   `channel` and `allow_from` set to your user id, and a post-office route.
   Run it again: nothing changes and no route is added.
4. [ ] `orch slack status` exits 0: the gateway serves every tool the
   channel uses, no daemon gateway is recorded yet, and the watch list is
   empty.
5. [ ] `orch slack test`. The `[orch slack test]` message appears in Slack;
   reply to it **in its thread**, from the phone if you can (and note whether
   the phone notified you about the test post: §6.7 Q1 of
   messaging-design.md). The command prints your reply and `round trip
   works`, and the post gets a ✅.
6. [ ] Start the daemon (`orch --root <dir> --acp-kit <kit>`). `orch slack
   status` now shows the daemon gateway as still registered, and the
   conversation id resolved to your `D…` DM.
7. [ ] Run `orch slack test --no-wait` with the daemon running: the
   workingman agent does **not** answer the test message.
8. [ ] Write a top-level message such as `/inbox` in the conversation: within
   one poll interval (20 s) the router answers it in Slack.
9. [ ] Have a task with `static_mcps: [post-office]` call `send_request` (or
   use the TUI-inbox steps in [Post-office messages](#post-office-messages)):
   the `❓ [req …]` post appears; `orch slack status` lists it as an open
   watched request. Reply in its thread: the agent gets the answer, the
   thread gets `✅ Sent to <sender>`, and status shows it closed.
10. [ ] Answer a second request from the TUI instead: the Slack post gets
   `✅ Answered via tui: …` in its thread and a ✅ reaction. A later reply in
   that thread gets `This request is already closed (answered via tui).`
11. [ ] Stop the daemon: `~/.workingman/gateways/daemon/orch-slack.json` is
   gone, and `orch slack status` says no daemon gateway is recorded.

## Routes and topics

A *topic* is a free-form tag on an outbound message. A route says "messages with
this topic go to this chat on this channel":

- `wolf` — the daemon's wolf-start / wolf-end messages, and every relayed wolf
  answer (shown as `[wolf <work-stream>]`).
- `workingman` — reserved for the workingman agent's own messages. Replies to a
  question go back to the chat that asked it, so a route is optional.
- `post-office` — operator requests and notifications from agents
  ([Post-office messages](#post-office-messages)). With no `post-office`
  route they go to the `*` routes, which is what "send to every channel"
  means in practice. A `post-office` route may set `notify: false` to receive
  requests but not notifications.
- `*` — wildcard: receives a message only when its topic has **no** exact route.
  Plain notifications (e.g. "Project blocked", sent alongside the macOS pop-up)
  have no topic and so reach only wildcard routes.

A topic may have several routes; each receives the message, concurrently, and one
channel failing never holds up the others. A topic with no route is not an error:
nothing is sent. The topic is shown as a `[topic] ` prefix, since neither
WhatsApp nor Signal has threads. A `chat` is a phone number for WhatsApp, and for
Signal an E.164 number, a UUID or `group:<groupId>`.

## Routing rules

A message is handled by the first rule that applies.

1. **Commands.** A message that starts with `/command` is answered by the
   router itself — no LLM, and never queued behind a running turn:

   | Command | Effect |
   |---|---|
   | `/help` | list the commands |
   | `/status` | one-line summary from the daemon snapshot: projects by status, live sessions by kind, task counts |
   | `/wolf [work-stream]` | bind this chat to a live wolf: the named project's (exact name, else a unique substring), or the only one. `no wolf running` when there is none; several wolves and no name lists them |
   | `/sleepy` | bind this chat to the sleepy-head agent while it is catching up missed schedules (ACP only) |
   | `/agent` | bind the chat back to the workingman agent |
   | `/who` | show what the chat is bound to |
   | `/inbox` | list the open operator requests from agents, oldest first, with their short ids |

   Paths such as `/etc/hosts` are not commands; they are ordinary text.

2. **An answer to a post-office request.** A reply-to of a request's message
   (`❓ [req r1t8w] …`, found in the conversation index), or a top-level
   message starting with `#<short id>` (`#r1t8w staging`), answers that
   request; see [Post-office messages](#post-office-messages). A `#word` that
   is not an open request's id is ordinary text.

3. **A pending permission request.** If an agent asked this chat for
   permission ([observe mode](#observe-mode-and-permissions-chat-bound-to-a-wolf)), the message is the answer.

4. **A reply to a wolf notification.** When the message is a reply-to
   (`InboundMessage.ReplyToID`) of a message in the conversation index — the
   "🐺 wolf is running for …" start message, or an answer the router relayed
   from a wolf — the text goes to **that wolf session** as a prompt and the
   wolf's answer comes back labelled `[wolf <work-stream>]`. A reply does not
   change the chat's binding. If the wolf has ended, or the notification was
   from an earlier session of the same project, the router says so instead of
   guessing. A reply to the "😴 sleepy-head is running" message (or to an
   answer relayed from it) goes to the sleepy-head session the same way,
   labelled `[sleepy-head]`.

5. **The chat's binding.** By default the chat is bound to the **workingman
   agent**: the text is sent to it with `Ask` and its reply is sent back. After
   `/wolf` the chat is bound to a wolf and its messages go there (replies are
   labelled `[wolf <work-stream>]`) until `/agent` — or until the wolf session
   ends, at which point the chat falls back to the workingman agent and is told
   so. The message that discovered the ended wolf is *not* forwarded to a
   different agent; resend it.

## Post-office messages

Agents with the `post-office` MCP send the operator **requests** (which
expect an answer) and **notifications** (which do not). They are files under
`~/.workingman/post-office/` (design: [messaging-design.md](messaging-design.md)
§2–§5; operator guide: [messaging.md](messaging.md)). The orch TUI inbox
reads them directly; when channels are configured, the daemon's
**Dispatcher** (`internal/postoffice/dispatch.go`, started by
`daemonChannels.start`) also sends them to the phone.

**Outbound.** Every operator message in `inbox/` without a
`deliveries/<id>.json` is rendered and sent on topic `post-office`:

```
❓ [req r1t8w] task fix-ci (workingman-messaging)
Which bucket for the export?

…body…

Options: 1) staging  2) prod
Reply to this message, or send #r1t8w <answer>. Expires in 24h.
```

Notifications start with `📣 [note …]` and have no last two lines. Messages
to agents (Step-Up) are never sent to channels. Each route's outcome is
recorded in `deliveries/<id>.json` (which the TUI shows as "sent to signal,
whatsapp"), and each sent request's message ids are registered in the
conversation index as kind `post-office`. A failed send is retried after 30 s,
2 m and 10 m, then recorded as failed; the other routes are not held up. A
daemon restart mid-send can cause at most one duplicate post per channel. The
Dispatcher passes over the whole store on startup, and on every change (with
a 5 s poll as fallback), so nothing sent or closed while it was down is
missed.

**Answering.** Reply to the request's message, or send `#<short id> <answer>`
from anywhere in the chat. The router claims the request (`Store.Claim`,
first answer wins) and replies `✅ Sent to <sender>`. If the request had
options, `1`, `2` … or the option's text select it; any other text is still a
valid answer. If someone answered first (in the TUI, or another channel), or
the request was cancelled or has expired, the reply says so (`Already
answered via tui.`) and nothing is sent to the agent. `/inbox` lists what is
still open.

**Closing notes.** When post-office finalises a request (it appears in
`done/`), every *other* delivery gets a reply to its original message:

- `✅ [req r1t8w] Answered via <tui|signal|…>: <first 200 characters>`
- `⌛ [req r1t8w] No longer needed — <the sender's reason>`
- `⌛ [req r1t8w] Expired — the agent stopped waiting`

The chat that answered gets no note. A channel that implements the optional
`channels.Resolver` interface ([Slack](#slack): a thread reply plus a reaction) is
resolved through it instead of a plain reply. Notifications get no closing
note. Once every note is sent, the record is marked `closed_notified`, and it
is removed 30 days later.

## Observe mode and permissions (chat bound to a wolf)

- Anything the wolf says while someone **else** drives it (the TUI user typing
  into the same ACP session) is relayed to every chat bound to it, labelled
  `[wolf <work-stream>]`. The router's own turns are not repeated.
- The wolf's tool-permission requests are put to the chat:

  ```
  [wolf alpha] 🔐 Permission needed: Run `git push`
  1) Allow once
  2) Always allow
  3) Reject
  Reply yes or no (or an option number). I'll reject it if there's no answer in 2m.
  ```

  The chat's next message answers it: `yes`/`ok`/`allow` (allow once),
  `always`, `no`/`deny`/`stop` (reject), an option number, or an option's own
  name. Anything else is not consumed as an answer; the router asks again. An
  unanswered request is **rejected** after `router.permission_timeout`
  (default 2m), and a request with no chat to ask — nobody is bound to the wolf
  and no turn of the router's is in flight — is rejected at once. A permission
  request raised while the router's own turn is waiting for the agent is asked
  of that chat, and the answer bypasses the queue.

## Robustness

- **One turn at a time per chat**, through a bounded queue
  (`router.queue_size`, default 5). A message that arrives while a turn runs is
  acknowledged ("The agent is busy with your previous message; yours is queued
  (#1)"); past the bound it is dropped with a message. Turns for the same
  agent from different chats are serialised too.
- **Slow turns.** The typing indicator is shown (channels with one, refreshed
  every 20s), and a turn still running after `router.thinking_after` (default
  10s) gets a single "…thinking" message.
- **Turn timeout** — `router.turn_timeout` (default 5m): the agent is asked to
  cancel and the chat gets a friendly message with whatever text had arrived.
- **Agent down.** While the daemon has no live workingman agent (starting, or
  being relaunched after a crash) the reply is immediate: *"The workingman agent
  is starting up, try again in a minute."* The same reply is used when the
  session is listed but cannot be attached to yet. Nothing hangs.
- **Long replies** are cut at 12000 characters, and split by the channel's own
  chunking: WhatsApp splits under 4096 characters inside `Send`; a channel
  implementing `channels.Chunker` is split by the router instead.
- **No secrets echoed.** Every reply, permission prompt and error passes
  through `audit.Redact` (the same helper the state snapshot uses): tokens, API
  keys, passwords, bearer headers, URL credentials and private-key blocks
  become `[REDACTED]`. Only the agent's final text segment is sent, not its
  tool-call narration.

## Router wiring

The `router:` block of `channels.yaml` (see the configuration reference) is
optional.

The router is created when the daemon starts its channels
(`daemonChannels.start` in `cmd/orch/channels.go`), after the wolf-start wiring,
and is closed with them. It reaches the daemon through `router.Source`
(`WorkingmanAgentSession`, `WolfSessions`, `StatusSummary`) and the ACP
sessions through `acpchat.Attach`.

## Audit events

`router_inbound`, `router_command`, `router_route`, `router_queued`,
`router_queue_full`, `router_turn_done`, `router_turn_error`,
`router_agent_down`, `router_attached`, `router_bind`, `router_binding_ended`,
`router_observe_relay`, `router_permission_asked`,
`router_permission_answered`, `router_permission_timeout`,
`router_permission_rejected`, `router_reply_unroutable`, `router_send_error`,
`router_index_error`, `router_attach_error`, `router_primed`,
`router_prime_error`, `router_postoffice_answered`,
`router_postoffice_already_closed`, `router_postoffice_not_found`,
`router_postoffice_ambiguous`, `router_postoffice_unavailable`,
`router_postoffice_error`.

The Dispatcher logs `postoffice_dispatcher_start`, `postoffice_delivered`,
`postoffice_delivery_error`, `postoffice_closed_notified`,
`postoffice_close_error`, `postoffice_index_error` and
`postoffice_dispatch_error` (ids and channel names only, never text).

Each line identifies the chat as `<channel>:<first 8 hex digits of the SHA-256
of the chat id>` and carries message *lengths*, never the message text or the
raw chat id.

## Security model

This channel lets a phone drive an agent that can read your orch state, so every
default is fail-closed.

- **Allowlist, default deny.** A zero-value config, an empty `allow_from`, a
  missing self id or an unknown chat type all deny. `*` is rejected in
  `allow_from`; the only way to admit everyone is the explicit, loudly-logged
  `allow_all: true`. A denied sender gets no reply (unless `deny_reply` is set),
  and denials are logged with the reason and a masked id, never the message
  text. Ids are normalised (`+1 (555) 123-4567`, `15551234567@s.whatsapp.net`,
  LIDs) the way hermes-agent does, so formatting cannot bypass the list.
- **Authenticated webhook.** Every POST must carry a valid `X-Hub-Signature-256`
  (HMAC-SHA256 of the raw body keyed by the app secret, constant-time compare);
  otherwise `401`. The webhook refuses to start without an app secret unless
  `insecure_skip_signature` is set. Bodies are capped at 3 MiB, messages are
  de-duplicated by `wamid`, and the subscription handshake compares the verify
  token in constant time. The listener binds loopback by default and warns when
  bound elsewhere.
- **Scoped agent sandbox.** The workingman agent runs in its own sbx sandbox
  whose writable mounts are a scratch directory and the orch roots; the
  state-snapshot directory, the audit-log directory and the ACP sessions root
  are mounted `:ro` (the mount enforces it — the prompt does not; an audit-log
  directory inside a root is covered by the root's writable mount instead). It
  has no GitHub or other secrets and no extra network policy. It edits
  `project.yaml` / `tasks/*.yaml` and creates `project.md` / `intake/*.md`, but
  only on request and after confirming the change with the human — the prompt is
  the guard there. Anyone who can message it can therefore change project
  state (but nothing on the host: no push, no wsp, no `:archive`), so keep the
  channel allowlist to people you would hand the TUI. The wolf is sandboxed too and can only write its project's control
  directory (+ its worktree). The workingman agent is told its replies go out
  over a phone messenger (short, plain text, no secrets).
- **Tool permissions are asked, not assumed.** When a wolf raises a permission
  request, the router puts it to the bound chat; no answer within
  `router.permission_timeout` (or nobody to ask) is a **rejection**.
- **Secrets handling.** Credentials are references (`env` / `0600` secrets file);
  inline values are a config error. They are held in a `channels.Secret` that
  prints as `[REDACTED]` through `fmt`, JSON and `slog`; the Cloud client scrubs
  the token from errors. Everything the router sends back, and every audit line,
  passes through `audit.Redact` (tokens, API keys, passwords, bearer headers,
  URL credentials, private-key blocks). Router audit lines identify a chat by a
  short hash and carry message *lengths*, not text. Keep the secrets file
  **outside** the directories mounted into the workingman agent (the default
  `~/.workingman/secrets.yaml` is), and note that `channels.log` lives beside the
  audit log, which that agent can read.
- **24-hour window (WhatsApp only).** Meta only lets a business message a user who wrote in the
  last 24 hours (templates excepted; the daemon sends none). A wolf-start message
  to a chat that has been silent for a day fails with a typed error
  (`channel_send_error` in the audit log) rather than being dropped silently.

## Exposing the webhook (tunnel setup)

Meta calls a **public https** URL; the listener is loopback-only. Put a tunnel
in front of it:

```sh
# any one of:
cloudflared tunnel --url http://127.0.0.1:8090
ngrok http 8090
tailscale funnel 8090
```

Then, in the Meta developer console (App Dashboard → WhatsApp → Configuration →
Webhook):

1. Callback URL: `https://<tunnel-host>/whatsapp/webhook` (`orch whatsapp setup
   --public-url https://<tunnel-host>` prints it exactly).
2. Verify token: the value in `secrets.yaml` (`orch whatsapp setup` prints it).
3. Subscribe to the **messages** field.
4. While the app is in development, add the owner's number to the recipient list
   (WhatsApp → API Setup).

Check the loop locally with the daemon running:

```sh
curl -s http://127.0.0.1:8090/health                     # status, counters, which secrets are set
curl -s "https://<tunnel-host>/whatsapp/webhook?hub.mode=subscribe&hub.verify_token=<token>&hub.challenge=42"   # → 42
```

Free tunnels change their URL on restart; re-register the callback when it does.

## Troubleshooting

| Symptom | Likely cause / fix |
|---|---|
| Daemon starts but no channel is mentioned in the audit log | no/`enabled: false` config: look for `channels_disabled`; `--channels-config` path wrong |
| `channels: …` startup error | invalid config (unknown key, inline secret, bad route); the message names the key. `secrets file … must not be accessible by group/others` → `chmod 600` |
| `channel_start_error`: webhook refuses to start | no `app_secret`; set it, or (only for local testing) `insecure_skip_signature: true` |
| Meta's "verify" button fails | tunnel not forwarding to the webhook port; wrong verify token (`403`); path mismatch with `webhook_path`; check `/health` locally |
| Messages sent from the phone get no reply | sender not in `allow_from` (audit/`channels.log`: `sender_not_allowlisted`); webhook not subscribed to `messages`; signature failures (`rejected_signature` in `/health` — wrong app secret); app still in development and the sender isn't a recipient |
| Wolf started but no WhatsApp message | outside the 24-hour window (`channel_send_error`, 131047): message the number once, then retry; no `wolf` (or `*`) route; rate-limited (`wolf_start_suppressed`); the start message is sent only once the session really started |
| Replying to the wolf message says the wolf ended / is from an earlier session | the wolf session ended or was relaunched; reply to the newer message, or `/wolf` |
| "The workingman agent is starting up, try again in a minute." | agent not running yet or being relaunched (it restarts with 5s→5m backoff): check `workingman_agent_*` audit events; it needs `--acp-kit` and `--workingman-agent` (auto-on whenever `--acp-kit` is set) |
| `/wolf` says the wolf cannot be attached | the wolf runs on the host in a terminal tab (`--wolf-host`); only ACP wolves can be tuned into |
| Wolf never starts working under `--headless` | nobody sends a wolf its opening prompt headless; run the TUI |
| Reply says "I cancelled it" | the turn exceeded `router.turn_timeout` |
| Signal: `orch signal status` / `test` fails, or no messages arrive | signal-cli daemon not running, `http_url` mismatch, account not registered, sender not on `allow_from`: see [signal.md troubleshooting](signal.md#troubleshooting) |
| Slack: `orch slack setup` / `status` cannot mint a gateway, or the slack server has 0 tools | standalone-codemode not running at `gateway_url`, the catalog has no `slack` (work setup only), or its backend failed to attach (re-run after a minute) |
| Slack: requests are posted but answers are ignored | the answer was not a **thread** reply, its author is not in `allow_from` (`channels.log`: `dropped message from a sender not in allow_from`), or the request is older than `watch_ttl`; `orch slack status` shows the watch list |
| A bridge-mode channel stopped after "logged out" | WhatsApp unlinked the device (exit 78); run `orch whatsapp pair --reset` |

Signal has no tunnel, webhook or 24-hour window to go wrong; its failures are
signal-cli being down or unregistered and the allowlist.

Logs: the audit log (`router_*`, `wolf_*`, `channel_*` events), `channels.log`
beside it (webhook and send details, ids masked), `whatsapp-bridge.log` for the
bridge backend. `orch whatsapp status` (or `orch signal status`, `orch slack
status`) is the quickest health check.

## Limits

The router attaches to sessions that already exist (`acpchat.Attach` adopts the
ACP session id recorded in the session's `stream.log`); the one exception is the
workingman agent, whose session the router creates (and primes) when nobody else
has. A wolf running on the host in a terminal tab (`--wolf-host`) has no ACP session, so
`/wolf` reports that it cannot be attached. The wire behaviour of the real
`claude-acp-client` when it receives a prompt for a session this connection did
not create is the one thing that needs a live smoke test (see the `acpchat`
package comment, and steps 6 and 12 above).

## Manual smoke test (real Meta test number)

For Signal see [signal.md](signal.md#manual-smoke-test).

The automated end-to-end test (`cmd/orch/channels_e2e_test.go`) runs the whole
pipeline against fakes. What it cannot prove — Meta's real delivery, signatures
and the real ACP agents — needs this checklist, once, with a Meta **test number**
(WhatsApp → API Setup gives a free one) and a throwaway project.

Prerequisites: `acp-wrapper`, the sbx CLI and an `--acp-kit`; a Meta app with a
test number; a tunnel tool; your own phone added as a recipient.

1. [ ] `orch whatsapp setup --public-url https://<tunnel-host>` with the test
   number's phone-number id, access token, app secret and your phone as owner;
   the Graph check passes.
2. [ ] `chmod`/`ls -l ~/.workingman/secrets.yaml` is `-rw-------`; `channels.yaml`
   contains no token.
3. [ ] Start the tunnel, then the daemon *with* the TUI (wolves are primed by
   it): `orch --root <dir> --acp-kit <kit>`. `orch whatsapp status` shows Graph
   reachable and the webhook listening; `curl http://127.0.0.1:8090/health`
   answers.
4. [ ] In the Meta console, register the callback URL + verify token: it turns
   green; subscribe to **messages**.
5. [ ] `orch whatsapp test` → the test message arrives on your phone.
6. [ ] Message the number from your phone: *"hi"*. A workingman-agent answer
   arrives (first one may take a minute while the sandbox boots; before then you
   get *"starting up, try again"*).
7. [ ] Ask *"which projects are open?"* → an answer that matches `orch status`.
   (Optionally restart with `--headless` and repeat 6–7: the router creates and
   primes the workingman agent's session itself.)
8. [ ] `/help`, `/status`, `/who` answer instantly, without the agent.
9. [ ] From a **different** phone (or edit `allow_from` temporarily) send a
   message → no reply, and `channels.log` shows `sender_not_allowlisted`.
10. [ ] POST a forged body to the public URL without a signature
   (`curl -X POST https://<tunnel-host>/whatsapp/webhook -d '{}'`) → `401`;
   `/health` `rejected_signature` goes up.
11. [ ] Create a project and make it blocked (`status: blocked` with a
   `blocked_reason`). Within seconds exactly **one** `[wolf] 🐺 wolf is running
   for <project>` message arrives. Touch the project file again: no second one
   (rate limit).
12. [ ] **Reply** to that message (long-press → Reply): the answer comes back as
   `[wolf <project>] …`, and the wolf's tab in the TUI shows your text.
13. [ ] Send a plain message (not a reply): it goes to the workingman agent,
   not the wolf. `/wolf` binds the chat to the wolf; `/agent` unbinds.
14. [ ] If the wolf asks for a tool permission, the `🔐 Permission needed` prompt
   arrives; `no` rejects it, silence rejects after `permission_timeout`.
15. [ ] Resolve the block (set the project to `ready`): after the unblock grace
   a `🐺 wolf finished for <project>` message arrives.
16. [ ] Stop the daemon (Ctrl-C): it exits promptly, the webhook port is free,
   `/health` no longer answers; the workingman agent's sandbox stops and the next
   start launches a fresh one.
17. [ ] Wait out the 24-hour window (or use a number that hasn't written for a
   day) and trigger a wolf: the audit log shows `channel_send_error` mentioning
   the 24-hour window, and the daemon carries on.
18. [ ] Grep `~/.workingman/**/*.log`, the audit log and your phone's messages
   for the access token and app secret: no hits.
