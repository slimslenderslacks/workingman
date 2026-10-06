# Wolf notifications over channels

`orch --channels-config <file>` (default `~/.workingman/channels.yaml`, or
`$WORKINGMAN_CHANNELS_CONFIG`) wires the messaging channels into the daemon. A
missing file, or one with no enabled channel, disables channels and the daemon
behaves exactly as before (macOS notifications only).

When enabled:

- The notifier becomes `Multi(Osascript, channels)`. The channel half is
  asynchronous, so a slow network never stalls dispatch; failures are audited
  as `channel_send_error`. Existing macOS notifications are unchanged.
- **Wolf start** — once a wolf session has actually started (not on a failed
  launch, not on a dedup no-op) the daemon sends, on topic `wolf`:

  ```
  🐺 wolf is running for <work-stream>
  Blocked: <reason, truncated>
  Failed tasks: a, b
  Reply to this message to talk to the wolf.
  ```

  The send is async with a timeout. The message id the channel returns is
  recorded in the conversation index (`channels.ConversationIndex`:
  `Register(channel, messageID, target)` / `Lookup(channel, messageID)`),
  kept in memory and in `<state dir>/channel-conversations.json` (atomic
  writes), so an inbound reply-to can be mapped back to the wolf session
  (`ConversationTarget.SessionKey` is `<project path>#wolf`).
- **Wolf end** — when that wolf session ends: `🐺 wolf finished for <work-stream>: project now <status>`.
  Only sent if the start message went out.
- **Rate limit** — at most one start message per work stream per
  `notify.wolf_start_interval` (default `10m`, `0s` = no limit) so a wolf
  relaunched in a loop cannot spam the chat. A suppressed start is audited as
  `wolf_start_suppressed`.

```yaml
notify:
  wolf_start_interval: 10m
routes:
  - {topic: wolf, channel: whatsapp, chat: "15551234567"}
```

Audit events: `channels_configured`, `channels_disabled`, `wolf_start_notified`,
`wolf_start_suppressed`, `channel_send_error`, `channel_index_error`,
`channel_start_error`.

Replying to a start message talks to that wolf; see `docs/channels.md` for the
inbound routing rules, commands and permission relay.
