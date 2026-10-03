# Talking to the daemon over a messaging channel

With a channel configured (`orch --channels-config <file>`, default
`~/.workingman/channels.yaml`; see `docs/wolf-notify.md` and
`docs/whatsapp-bridge.md`) the daemon does not only *send* messages — it also
listens. Every inbound message is handed to the **router**
(`internal/router`), which decides who should answer it and sends the answer
back to the same chat.

Only senders the channel's access policy admits (the WhatsApp allowlist, see
`orch whatsapp setup`) ever reach the router; the router does no
authorisation of its own.

## Routing rules

A message is handled by the first rule that applies.

1. **Commands.** A message that starts with `/command` is answered by the
   router itself — no LLM, and never queued behind a running turn:

   | Command | Effect |
   |---|---|
   | `/help` | list the commands |
   | `/status` | one-line summary from the daemon snapshot: projects by status, live sessions by kind, task counts |
   | `/wolf [work-stream]` | bind this chat to a live wolf: the named project's (exact name, else a unique substring), or the only one. `no wolf running` when there is none; several wolves and no name lists them |
   | `/agent` | bind the chat back to the workingman agent |
   | `/who` | show what the chat is bound to |

   Paths such as `/etc/hosts` are not commands; they are ordinary text.

2. **A pending permission request.** If an agent asked this chat for
   permission (rule 5), the message is the answer.

3. **A reply to a wolf notification.** When the message is a reply-to
   (`InboundMessage.ReplyToID`) of a message in the conversation index — the
   "🐺 wolf is running for …" start message, or an answer the router relayed
   from a wolf — the text goes to **that wolf session** as a prompt and the
   wolf's answer comes back labelled `[wolf <work-stream>]`. A reply does not
   change the chat's binding. If the wolf has ended, or the notification was
   from an earlier session of the same project, the router says so instead of
   guessing.

4. **The chat's binding.** By default the chat is bound to the **workingman
   agent**: the text is sent to it with `Ask` and its reply is sent back. After
   `/wolf` the chat is bound to a wolf and its messages go there (replies are
   labelled `[wolf <work-stream>]`) until `/agent` — or until the wolf session
   ends, at which point the chat falls back to the workingman agent and is told
   so. The message that discovered the ended wolf is *not* forwarded to a
   different agent; resend it.

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

## Configuration

All optional, in `channels.yaml`:

```yaml
router:
  permission_timeout: 2m   # unanswered permission requests are rejected
  turn_timeout: 5m         # a turn is cancelled after this long
  thinking_after: 10s      # "…thinking" ack for turns longer than this
  queue_size: 5            # messages queued per chat behind the running turn
```

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
`router_index_error`, `router_attach_error`.

Each line identifies the chat as `<channel>:<first 8 hex digits of the SHA-256
of the chat id>` and carries message *lengths*, never the message text or the
raw chat id.

## Limits

The router attaches to sessions that already exist (`acpchat.Attach` adopts the
ACP session id recorded in the session's `stream.log`); it never creates a
conversation. A wolf running on the host under tmux (`--wolf-host`) has no ACP
session, so `/wolf` reports that it cannot be attached. The wire behaviour of
the real `claude-acp-client` when it receives a prompt for a session this
connection did not create is the one thing that needs a live smoke test (see the
`acpchat` package comment).
