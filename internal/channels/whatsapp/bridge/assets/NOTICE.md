# Attribution

`bridge.js`, `helpers.js`, `outbound_ids.js`, `package.json` and
`package-lock.json` are derived from the WhatsApp bridge in
[NousResearch/hermes-agent](https://github.com/NousResearch/hermes-agent)
(`scripts/whatsapp-bridge/`), MIT licensed, Copyright (c) 2025 Nous Research.
The license text is in `LICENSE-hermes-agent`.

`outbound_ids.js` is vendored unchanged. The rest was trimmed to what the
workingman daemon needs:

- Endpoints kept: `GET /messages` (now a real long poll), `POST /send`,
  `POST /typing`, `POST /read`, `GET /health`, plus the QR pairing flow.
- Dropped: media download/send, polls, locations, message editing, chat
  info, and the in-bridge allowlist (`allowlist.js`) and owner-message gate
  (`owner_message_gate.js`). Access control lives in the Go `AccessPolicy`
  (`internal/channels/whatsapp/policy.go`), which ports those modules; the
  bridge forwards every text event and the daemon decides.
- Added: a per-run bearer token on every HTTP request, pre-assigned outbound
  message ids (so a send's echo is recognised even if it arrives before the
  send call returns), and a distinct exit code (78) when WhatsApp logs the
  session out.

Baileys (`@whiskeysockets/baileys`) is MIT licensed by its authors.
