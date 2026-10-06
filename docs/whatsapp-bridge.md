# WhatsApp bridge backend

The `whatsapp` channel has two backends, chosen with `options.mode` in
`~/.workingman/channels.yaml`:

| `mode`   | Transport                                   | Needs                                   |
|----------|---------------------------------------------|-----------------------------------------|
| `cloud`  | Meta WhatsApp Cloud API (webhook + Graph)   | Meta app, public webhook URL. **Default** |
| `bridge` | Baileys linked device, via a local Node process | Node >= 18, a phone to scan a QR code   |

The bridge backend is `whatsapp.BridgeChannel`: the same `channels.Channel`
as the cloud one, so routing, topics and the `AccessPolicy` are identical.

```yaml
channels:
  whatsapp:
    type: whatsapp
    options:
      mode: bridge
      access:
        mode: self-chat              # message yourself; or `bot` for a dedicated number
        self_ids: ["15551234567"]
        reply_prefix: "[wm] "        # self-chat only: marks our own replies
      bridge:                        # every key optional
        # node: /usr/local/bin/node
        # port: 0                    # 0 = a free 127.0.0.1 port, chosen at start
        # session_dir: ~/.workingman/whatsapp/session
        # install_dir: ~/.workingman/whatsapp/bridge
        # send_read_receipts: false
routes:
  - {topic: wolf, channel: whatsapp, chat: "15551234567"}
```

## Pairing

```sh
orch whatsapp pair            # install the bridge's npm deps, show a QR code
orch whatsapp pair --reset    # discard the session and link again
```

Scan the QR code in WhatsApp under *Settings > Linked devices > Link a device*.
The session lands in `~/.workingman/whatsapp/session`; treat it like a
password. `pair` is a no-op when a session already exists.

The first run needs network access for `npm ci`. Node is an optional
dependency: if it is missing or older than 18, `pair` and the daemon fail with a
message saying so (the cloud backend never needs it).

## Bot vs self-chat

Same as hermes-agent (`website/docs/user-guide/messaging/whatsapp.md`):

- **self-chat**: a personal number; you message yourself and only your own
  messages in your own chat are admitted. Replies come from the same number, so
  set `reply_prefix` to mark them.
- **bot**: a dedicated number; admitted senders come from `access.allow_from`.

## Ban risk

Baileys speaks the unofficial WhatsApp Web protocol. WhatsApp can disconnect or
ban numbers that use it, so prefer a number you can afford to lose and keep
traffic low and human-like. The Cloud API backend is the supported option.

## Runtime behaviour

- The daemon supervises `node bridge.js`: restart with exponential backoff
  (1s doubling to 1m, reset after 30s of healthy running). Output goes to
  `whatsapp-bridge.log` beside the audit log, never the TUI; the log rotates
  at 5 MiB.
- The bridge binds `127.0.0.1` only, rejects non-loopback `Host` headers, and
  requires a per-run bearer token the daemon generates.
- If WhatsApp logs the session out, the bridge exits with code 78, the daemon
  stops restarting it and `Send` returns an error saying to run
  `orch whatsapp pair`.
- A send right after startup waits up to 30s for the connection to come up.
- Echo suppression: the bridge assigns message ids before sending and the
  `AccessPolicy` remembers them, so our own messages are never read back as
  the owner typing.
- Text only: captions are kept, but media is not downloaded or sent.

The vendored bridge lives in `internal/channels/whatsapp/bridge/assets/`, derived
from hermes-agent (MIT); see `NOTICE.md` there. Its pure helpers have Node
tests: `cd internal/channels/whatsapp/bridge/assets && node --test`.
