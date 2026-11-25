# Research: the Slack MCP as a workingman channel

What the `slack` server behind the standalone-codemode gateway can actually do,
and whether it is enough to build a Slack **channel**
(`internal/channels`, alongside Signal and WhatsApp; see
[channels.md](../channels.md)). The channel would post notifications and
requests, and pick up human answers.

Contents: [TL;DR](#tldr) ·
[Connecting from the daemon](#1-connecting-from-the-daemon) ·
[What the server exposes](#2-what-the-server-exposes) ·
[Observed behaviour](#3-observed-behaviour) ·
[Answers](#4-answers) ·
[Proposed channel design](#5-proposed-channel-design) ·
[Open questions](#6-open-questions) ·
[Appendix: verbatim tool list and schemas](#appendix-verbatim-tool-list-and-schemas)

Research date: 2026-11-20. Tested from a sandbox through a sub-gateway
(`MCP_URL=http://host.docker.internal:3333/gateways/<id>`, minted with
`static_mcps: [slack]`). Base commits: workingman `f6c2c956`,
standalone-codemode `ffab8eaf`.

## TL;DR

- **Posting works and returns the ts.** `slack_send_message` returns
  `{"message_link": ..., "message_context": {"message_ts": "1795141398.745179", "channel_id": "D0CBT6QV8NT"}}`.
  That is the only tool that returns structured JSON. Everything else returns
  **markdown-ish text** that we would have to parse.
- **Threads work.** `thread_ts` on `slack_send_message` posts a reply.
  `slack_read_thread(channel_id, message_ts, oldest=<last seen ts>)` returns the
  parent plus only the replies **newer than** `oldest` (exclusive). That is the
  polling primitive.
- **Reactions: add only.** `slack_add_reaction` works. There is no remove, and
  reads show reaction **counts** but not who reacted.
- **No edit, no delete.** There is no `chat.update` or `chat.delete`
  equivalent, so "answered elsewhere" has to be a thread reply plus a reaction.
  We cannot edit the original message.
- **No push.** The server advertises only `listChanged` capabilities. There is
  no `resources/subscribe` (it returns `-32601 method not found`), and the only
  resource is an MCP-app UI form. Inbound replies **must be polled**.
- **We post as the user, not a bot.** The token is Jim's user token, obtained
  through Okta XAA. Every post is from `U0CC1K1MFNX` (Jim Clark). The only
  distinguishing mark is a trailer appended to the text:
  `*Sent using* <@U0B1REGGAMC|Slack MCP>`. To tell our posts from human
  replies, record every ts we post. Slack will most likely not push-notify the
  human about messages "they" sent (see [open questions](#6-open-questions)).
- **Concurrent calls are unreliable.** Parallel `tools/call`s regularly failed
  with `header script failed: /opt/okta-cpe/Scripts/fetch-mcp-headers-xaa.sh
  exited with status 1`. Sequential calls always succeeded. The channel must
  **serialize** its Slack calls and retry this error. The failure happens
  before the request leaves the gateway, so a retry is safe even for a send.

## 1. Connecting from the daemon

From standalone-codemode's `README.md` (Servers, `/mcp`, Dynamic
sub-gateways), `docs/as-built.md` and `docs/catalog.md`:

| Endpoint | Auth | Notes |
| --- | --- | --- |
| `GET /servers`, `GET /status` | `RequireBearer` (JWT from the gateway's own `/authorize` + `/token`, signing key in memory only) | discovery |
| `/mcp` | `RequireBearer` | aggregate over the whole catalog; `com.docker/gateway-filter` in `initialize` can scope it to `["slack"]` |
| `POST /gateways` `{"servers":["slack"]}` | **none**. The unguessable id is the only access control. | `201 {"id","url"}`; in memory only, lost on gateway restart |
| `/gateways/{id}` | none | streamable HTTP MCP over just those servers |
| `DELETE /gateways/{id}` | none | teardown |

The catalog entry is
`{"name":"slack","url":"https://llm.atko.ai/slack_xaa/mcp","header-script":"/opt/okta-cpe/Scripts/fetch-mcp-headers-xaa.sh"}`.
The header script runs **on the host, on every backend request**, and mints
the Okta XAA headers. A working script *is* the authorization; there is no
`slack-authorize` tool and no OAuth step for us.

**Recommendation:** the daemon is a host process, so it should do what
`internal/mcpgateway` already does for sandboxes:

1. `mcpgateway.Client{BaseURL: "http://localhost:3333"}.Create(ctx, []string{"slack"}, false)`.
   The response reports per-server state (`ServerState.Serving()`).
2. `mcpgateway.OpenSession(ctx, gw.URL, ...)`, then `ListTools`, then
   `CallTool`. The existing `Session` already handles JSON and SSE responses,
   `Mcp-Session-Id`, and `ErrSessionLost`.
3. On `ErrSessionLost` (gateway restart drops every sub-gateway), mint a new
   gateway and reopen the session. `DELETE` the gateway when the channel
   closes.

This avoids `/mcp`'s bearer token, whose signing key does not survive a
gateway restart. Tool names arrive prefixed `slack_xaa-…` (the backend's own
name, not the catalog's `slack`). Resolve them from `tools/list` by suffix
(`…slack_send_message`) rather than hard-coding the prefix.

The `initialize` result from the sub-gateway:

```json
{"capabilities":{"logging":{},"prompts":{"listChanged":true},"resources":{"listChanged":true},"tools":{"listChanged":true}},
 "protocolVersion":"2025-11-25","serverInfo":{"name":"mcp-gateway-static","version":"0.1.0"}}
```

## 2. What the server exposes

`tools/list` returns 13 Slack tools, plus the gateway's `mcp-find` and
`mcp-add` because the test sub-gateway was dynamic:

| Tool | Read-only | What it is for us |
| --- | --- | --- |
| `slack_send_message` | no | **post** (top level or `thread_ts` reply, optional `reply_broadcast`); returns ts |
| `slack_add_reaction` | no | mark a request answered or timed out |
| `slack_read_channel` | yes | top-level history, newest first, `oldest`/`latest`/`cursor`, limit ≤ 100; per-message thread reply count |
| `slack_read_thread` | yes | parent and replies, `oldest`/`latest`/`cursor`, limit ≤ 1000 |
| `slack_search_public` / `slack_search_public_and_private` | yes | keyword search; the latter "requires user consent" per its description |
| `slack_list_user_channels` | yes | find a channel id (`types=im` for DMs) |
| `slack_search_channels` | yes | find a channel the user is not in |
| `slack_list_channel_members` | yes | — |
| `slack_read_user_profile` / `slack_search_users` | yes | resolve sender names |
| `slack_schedule_message` | no | delayed post (cannot be edited later) |
| `slack_send_message_draft` | no | save a draft. Has `_meta.ui.resourceUri = ui://send-message-input.html` (an MCP-app form). Not useful to us. |

**Missing:** update/edit, delete, remove reaction, reactions.get (who
reacted), any events, socket mode, or webhook. `resources/list` returns one
resource, `ui://send-message-input.html` (`text/html;profile=mcp-app`).
`resources/templates/list` and `prompts/list` are empty.

The full descriptions and input schemas are in the
[appendix](#appendix-verbatim-tool-list-and-schemas).

## 3. Observed behaviour

All writes went to **Jim's own DM-to-self** (`channel_id: "U0CC1K1MFNX"`,
which resolves to `D0CBT6QV8NT`). Two messages and one reaction were
posted there; nothing went to any other channel or person.

**Send** (`channel_id` = own user id):

```json
{"message_link":"https://okta.enterprise.slack.com/archives/D0CBT6QV8NT/p1795141398745179",
 "message_context":{"message_ts":"1795141398.745179","channel_id":"D0CBT6QV8NT"}}
```

The response carries the **resolved** channel id (`D…`), not the user id we
passed. Store that one.

**Thread reply** (`thread_ts: "1795141398.745179"`) returned
`message_ts: "1795141406.120249"`, with a link carrying `thread_ts=` and
`cid=`.

**Reaction:** `{"result":"Successfully added :white_check_mark: reaction to the message."}`

**read_thread** (detailed):

```text
=== THREAD PARENT MESSAGE ===
From: Jim Clark <jim.clark@okta.com> (U0CC1K1MFNX)
Time: 2026-11-19 18:23:18 PST
Message TS: 1795141398.745179
[workingman research-slack-mcp] test post to self-DM — please ignore. Checking send_message return value.
*Sent using* <@U0B1REGGAMC|Slack MCP>
Reactions: white_check_mark (1)

=== THREAD REPLIES (1 total) ===

--- Reply 1 of 1 ---
From: Jim Clark <jim.clark@okta.com> (U0CC1K1MFNX)
Time: 2026-11-19 18:23:26 PST
Message TS: 1795141406.120249
[workingman research] thread reply test — please ignore.
*Sent using* <@U0B1REGGAMC|Slack MCP>
```

followed by `pagination_info: "There are no more messages in this thread."`.

- With `oldest: "1795141400.000000"` the reply is returned. With
  `oldest: "1795141406.120249"` (the reply's own ts) the result is
  `No thread messsages` (sic). So **`oldest` is exclusive**, and the parent is
  always included. A cursor of "last ts seen" works without dedup.
- `response_format: concise` **drops the `Message TS` lines**, so it is
  useless for polling. Always use `detailed`.

**read_channel** (`oldest` set, detailed) lists top-level messages only, with
a thread summary:

```text
=== Message from Jim Clark <jim.clark@okta.com> (U0CC1K1MFNX) at 2026-11-19 18:23:18 PST ===
Message TS: 1795141398.745179
...
Thread: 1 replies (latest: 2026-11-19 18:23:26 PST)
Reactions: white_check_mark (1)
```

The reply **count** is the cheap change signal. The "latest" is a
human-formatted time, not a ts.

**Search** (`"thread reply test" in:<@U0CC1K1MFNX>`) found the thread reply
with `Message_ts` and a permalink carrying `thread_ts`. The search `Text:`
**omits** the `Sent using` trailer, so search cannot tell our posts from human
ones.

**Identity:** `slack_read_user_profile` reports user `U0CC1K1MFNX`
(`jim.clark`), `Bot: No`. Posts carry the user's identity. The "Slack MCP"
app (`U0B1REGGAMC`) appears only as the trailer mention.

**Concurrency:** four parallel `tools/call`s on one session produced 1–2
successes and the rest
`-32005 … rejected by transport: … header script failed: /opt/okta-cpe/Scripts/fetch-mcp-headers-xaa.sh exited with status 1`.
Some calls also returned an empty HTTP body. The same calls run sequentially
always succeeded. A request id reused while in flight gets
`-32600 duplicate in-flight request ID`.

**Push:** a `GET` on the session (the SSE stream) stays open, but the server
would only ever send `list_changed` or logging on it. `resources/subscribe`
returns `-32601`. `logging/setLevel` is accepted.

## 4. Answers

**Can we post and get back a message id / ts?** Yes:
`message_context.message_ts` plus `message_context.channel_id`. Use
`"<channel_id>:<ts>"` (or just ts, since the channel is fixed) as the
`channels.Channel.Send` message ID.

**Can we post into a thread?** Yes, with `thread_ts`. `reply_broadcast: true`
also shows the reply in the channel.

**Can we read thread replies to a message we posted?** Yes, with
`slack_read_thread(channel_id, message_ts=<our ts>, oldest=<cursor>)`,
`detailed`. Paginate with `cursor` (limit up to 1000).

**Reactions?** We can add them. We cannot remove them, and we cannot see who
reacted (counts only). A human reaction is therefore weak as an answer
signal: it is indistinguishable from our own unless we track counts.

**Push / subscription / notification?** None. Only `listChanged`, no resource
subscriptions, no events or streaming of Slack activity. **Inbound must be
polled** (`read_channel` / `read_thread`, or `search`).

**Rate limits / pagination?**
- The MCP exposes no rate-limit information or headers. Behind it are Slack
  Web API limits on the user token: roughly Tier 3 (~50/min) for
  `conversations.history` and `replies`, and roughly 1 msg/s per channel for
  posting. These are Slack's published tiers, not measured here.
- The real constraint observed is the **host header script**. It runs per
  request and fails under concurrency, so polling needs one serialized worker.
- Pagination: `read_channel` limit ≤ 100, `read_thread` ≤ 1000, search ≤ 20
  per page, all with an opaque `cursor`. `pagination_info` is free text
  ("There are no more messages available.", or "Use cursor: \"…\"").

**Identity of the poster?** A user token, so every post is from the human
operator's own account. We cannot filter "messages not from us" by user id.
Options, most robust first:
1. Keep a set of every ts we posted. Any message in a watched thread or
   channel whose ts is not in the set is human.
2. As a fallback, check for the `*Sent using* <@U0B1REGGAMC|Slack MCP>`
   trailer. It also marks posts by any *other* Slack-MCP client the user runs
   (e.g. an agent in another tool), so it is a heuristic only.

## 5. Proposed channel design

`type: slack` in `channels.yaml` (the config test already uses a disabled
`slack` entry), with `options: {gateway_url: http://localhost:3333, channel:
<id or user id>, poll_interval: 20s}`. There are no secrets: the gateway and
the host script hold the credentials.

**Which Slack channel.** Use a dedicated private channel (e.g.
`#wm-<user>`) or the DM-to-self. Either way posts come from the user. A
private channel is cleaner for search and context. The DM-to-self is the
easiest to set up.

**Send (`Channel.Send`).** Call `slack_send_message(channel, text,
thread_ts=ReplyToID)`. Return `message_context.message_ts`, record it in the
`sent` set (persisted, with a TTL like `FileConversationIndex`), and add it
to the watch list when it is a request. Render `Topic` with `FormatTopic`, or
use it to pick a per-topic parent thread. Prefix requests with an id such as
`[req 7f3a]` so humans and search can see them.

**Detecting "a human answered request X".** In `Start`, one goroutine,
serialized calls, retrying the header-script error with backoff:

1. Every `poll_interval`, call `slack_read_channel(channel,
   oldest=<oldest open request ts minus 1µs>, limit=100)` (detailed),
   following `cursor`. Parse the `Message TS:` and `Thread: N replies` blocks.
   - A top-level message whose ts is not in `sent`: a fresh human message.
     Emit an `InboundMessage` with no `ReplyToID`, which the router sends to
     the workingman agent, as with Signal and WhatsApp. Keep a separate
     "last top-level ts" cursor for these.
   - A watched request whose reply count rose: go to step 2.
2. Call `slack_read_thread(channel, message_ts=<request ts>,
   oldest=<last ts seen in that thread>)`. Every reply whose ts is not in
   `sent` is human. Emit an `InboundMessage{MessageID: ts, ReplyToID:
   <request ts>, Text: …}`. The existing `ConversationIndex` reply-to lookup
   then routes it to the requester. Advance the thread cursor.
3. Stop watching a request once it is answered (here or elsewhere), timed out,
   or past a cap (e.g. 7 days).

This costs one call per interval plus one per thread with new activity,
rather than one per open request. If `read_channel` parsing turns out
fragile, the fallback is polling `read_thread` for each open request, which
is fine for a handful of open requests.

**"Answered elsewhere."** Since edits are impossible:
- Post a thread reply on the request: `✅ Answered via <tui|signal|…>:
  <short answer>`. Record its ts in `sent`. Do not use `reply_broadcast`.
- Add the `white_check_mark` reaction to the parent, which is visible in the
  channel view.
- For a timeout or the agent giving up, reply in the thread (`⌛ No longer
  waiting`) and add `hourglass` or `no_entry_sign`.
- If a human later replies in a closed thread, either ignore it or emit it
  as a fresh inbound message with a note. That is a router decision.

This could be an optional interface next to `Chunker` and `Typer`, e.g.
`Resolver { Resolve(ctx, chatID, messageID, note string) error }`, so the
post-office can tell every channel except the one that answered.

**Parsing.** Write a small parser for the `detailed` text format, with
golden-file tests built from the samples above. Fail loudly (log, then fall
back to per-thread polling) when no `Message TS:` lines are found in a
non-empty response.

## 6. Open questions

1. **Will the human be notified at all?** Slack generally does not
   push-notify a user about messages posted as themselves, including in a
   DM-to-self or a channel. This was not verified on a phone. If confirmed,
   the Slack channel is "a place to answer", not "a place that pings you", and
   still needs Signal, WhatsApp or the TUI for alerting. Possible fixes: a
   real Slack bot app or token (not available through this MCP), or posting
   to a channel the user has set to notify on every message (unverified that
   it fires for self-authored posts).
2. **Header-script concurrency:** is the failure in the Okta script (shared
   token cache, file lock?) or in the gateway? Can the gateway serialize or
   cache the script's output for its TTL? That fix would belong in
   standalone-codemode and would also help the other `*_xaa` servers.
3. **Text format stability:** the response format is the remote server's
   presentation layer (`llm.atko.ai/slack_xaa`) and is not a contract. Is
   there a JSON response option, or an owner we can ask?
4. **Real rate limits** of the `llm.atko.ai` proxy, on top of Slack's tiers.
   These are unknown. Start at a 20–30 s poll interval and back off on errors.
5. **`slack_search_public_and_private` says it needs "user consent".** Is that
   only prompt guidance for LLMs, or enforced? We don't need it if we poll
   `read_channel` and `read_thread`.
6. **Tool name prefix `slack_xaa-`** differs from the catalog name `slack`.
   Confirm where the prefix comes from (backend `serverInfo.name`?) before
   relying on suffix matching.
7. **Multiple hosts or users:** the token is per human. A shared team channel
   would show every workingman post as that person.

## Appendix: verbatim tool list and schemas

Captured with `tools/list` on the sub-gateway on 2026-11-20. Metadata is every
field other than `name`, `description` and `inputSchema`. Descriptions are
complete, as the server sent them.

#### `slack_xaa-slack_add_reaction`

Metadata: `{"annotations": {"destructiveHint": false, "idempotentHint": true, "openWorldHint": true, "readOnlyHint": false, "title": "Add a reaction to a message"}, "title": "Add a reaction to a message"}`

Description:

```text
Adds a reaction (emoji) to a Slack message.

Requires the channel_id and message_ts of the target message, plus the emoji name without colons (e.g. "thumbsup", "eyes", "white_check_mark"). Adding a duplicate reaction succeeds silently.

```

Input schema:

```json
{
  "properties": {
    "channel_id": {
      "description": "The ID of the channel containing the message",
      "type": "string"
    },
    "emoji": {
      "description": "Reaction (emoji) name without colons",
      "type": "string"
    },
    "message_ts": {
      "description": "Timestamp of the message to react to",
      "type": "string"
    }
  },
  "required": [
    "channel_id",
    "message_ts",
    "emoji"
  ],
  "type": "object"
}
```

#### `slack_xaa-slack_list_channel_members`

Metadata: `{"annotations": {"destructiveHint": false, "idempotentHint": true, "openWorldHint": true, "readOnlyHint": true, "title": "List channel members"}, "title": "List channel members"}`

Description:

```text
Lists members of a Slack channel, group, or group DM (MPIM). Returns profile details or just user IDs. Your user_id: U0CC1K1MFNX. Does not support DMs.

Formats: 'detailed' (default) = full profile, 'concise' = @username + display name, 'ids_only' = user IDs only (fastest, skip profile fetch).
Filters out deleted users and bots by default (use include_deleted/include_bots to include) for 'detailed' and 'concise' formats. 'ids_only' returns all member IDs without filtering (no profile data is fetched). Returns up to 30 members per page (limit is capped at 30). Use cursor from pagination_info to fetch next page.

Use slack_search_channels to find a channel ID first. Use slack_search_users to find users across the workspace. Use slack_read_user_profile for detailed info on a specific user.

```

Input schema:

```json
{
  "properties": {
    "channel_id": {
      "description": "ID of the channel to list members from",
      "type": "string"
    },
    "cursor": {
      "description": "Pagination cursor from previous response",
      "type": "string"
    },
    "include_bots": {
      "description": "Include bots and apps in the member list (default: false)",
      "type": "boolean"
    },
    "include_deleted": {
      "description": "Include deleted/deactivated users in the member list (default: false)",
      "type": "boolean"
    },
    "limit": {
      "description": "Number of members to return per page (default: 30, max: 30)",
      "type": "integer"
    },
    "response_format": {
      "description": "Level of detail (default: 'detailed'). Options: 'detailed', 'concise', 'ids_only'",
      "enum": [
        "detailed",
        "concise",
        "ids_only"
      ],
      "type": "string"
    }
  },
  "required": [
    "channel_id"
  ],
  "type": "object"
}
```

#### `slack_xaa-slack_list_user_channels`

Metadata: `{"annotations": {"destructiveHint": false, "idempotentHint": true, "openWorldHint": true, "readOnlyHint": true, "title": "List user channels"}, "title": "List user channels"}`

Description:

```text
Lists channels the user is a member of. Supports public channels, private channels, DMs (im = 1-on-1 direct messages), and Group DMs (mpim = multi-party direct messages).

types accepts a comma-separated list: public_channel, private_channel, im (DM), mpim (Group DM). Default: "public_channel,private_channel". To include DMs or Group DMs, add them explicitly (e.g., types="public_channel,private_channel,im,mpim" for all).

shared filters by Slack Connect (external sharing), independent of types: "any" (default), "slack_connect" (only channels shared externally with another org), "internal" (exclude them). Same-org cross-workspace sharing is not Slack Connect and counts as internal.

Archived channels are included by default. Pass exclude_archived=true to hide them.

DMs and Group DMs lack user-set names/topics. DMs show the other participant's display name; Group DMs show members.

name_prefix is case-insensitive. cursor paginates results: pass the next_cursor from a previous response to fetch the next page. It works with every filter, including name_prefix and shared.

team_id restricts the results to a single workspace. On a multi-workspace (Grid) org, channel memberships are per-workspace, so pass team_id (an encoded workspace ID like "T012AB3C4") to list channels in a specific workspace; without it, results come from the user's default workspace only.

Related: slack_search_channels (channels you're not in), slack_read_channel (read messages).

Examples:
	- DMs only: slack_list_user_channels(types="im")
	- Group DMs only: slack_list_user_channels(types="mpim")
	- Private + DMs: slack_list_user_channels(types="private_channel,im")
	- All types: slack_list_user_channels(types="public_channel,private_channel,im,mpim")
	- Slack Connect only: slack_list_user_channels(shared="slack_connect")
	- Exclude Slack Connect: slack_list_user_channels(shared="internal")
	- Prefix filter: slack_list_user_channels(name_prefix="eng-")
	- IDs only: slack_list_user_channels(format="ids_only")

```

Input schema:

```json
{
  "properties": {
    "cursor": {
      "description": "Pagination cursor from a previous response. Pass it to fetch the next page of results.",
      "type": "string"
    },
    "exclude_archived": {
      "description": "Exclude archived channels (default: false)",
      "type": "boolean"
    },
    "format": {
      "description": "Output format: 'full' (default, all details), 'ids_only' (just channel IDs), or 'names_only' (just channel names)",
      "type": "string"
    },
    "limit": {
      "description": "Max channels to return (default: 50, max: 200)",
      "type": "integer"
    },
    "name_prefix": {
      "description": "Filter channels whose name starts with this string (case-insensitive)",
      "type": "string"
    },
    "shared": {
      "description": "Filter by Slack Connect (external sharing) status, independent of `types`. Valid values: \"any\" (default, no filter), \"slack_connect\" (only channels shared externally with another organization), \"internal\" (exclude externally-shared channels). Note: channels shared only across workspaces within the same org (not Slack Connect) count as internal.",
      "type": "string"
    },
    "team_id": {
      "description": "Encoded workspace ID (e.g. \"T012AB3C4\") to list channels from. On a multi-workspace org, channel memberships are per-workspace; without this, results come from the user's default workspace only.",
      "type": "string"
    },
    "types": {
      "description": "Comma-separated list of channel types to include. Valid values: public_channel, private_channel, mpim, im. Default: \"public_channel,private_channel\" (DMs and Group DMs are excluded unless explicitly listed).",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

#### `slack_xaa-slack_read_channel`

Metadata: `{"annotations": {"destructiveHint": false, "idempotentHint": true, "openWorldHint": true, "readOnlyHint": true, "title": "Read channel messages"}, "title": "Read channel messages"}`

Description:

```text
Reads messages from a Slack channel in reverse chronological order (newest first). To read DM history, use a user_id as channel_id. Read-only.

Use slack_read_thread with message_ts to read thread replies. Use slack_search_channels to find a channel ID by name. Use slack_search_public to search across channels. If 'channel_not_found', try slack_search_channels first.

```

Input schema:

```json
{
  "properties": {
    "channel_id": {
      "description": "ID of the Channel, private group, or IM channel to fetch history for. Can also be a user_id to read DM history.",
      "type": "string"
    },
    "cursor": {
      "description": "Paginate through collections of data by setting the cursor parameter to a next_cursor attribute returned by a previous request",
      "type": "string"
    },
    "latest": {
      "description": "End of time range of messages to include in results (timestamp)",
      "type": "string"
    },
    "limit": {
      "description": "Number of messages to return, between 1 and 100. Default value is 100.",
      "type": "integer"
    },
    "oldest": {
      "description": "Start of time range of messages to include in results (timestamp)",
      "type": "string"
    },
    "response_format": {
      "description": "Level of detail: 'detailed' (default, includes reactions + thread info) or 'concise'.",
      "type": "string"
    }
  },
  "required": [
    "channel_id"
  ],
  "type": "object"
}
```

#### `slack_xaa-slack_read_thread`

Metadata: `{"annotations": {"destructiveHint": false, "idempotentHint": true, "openWorldHint": true, "readOnlyHint": true, "title": "Read thread messages"}, "title": "Read thread messages"}`

Description:

```text
Reads messages from a specific Slack thread (parent message + all replies). Read-only.

Requires channel_id and message_ts of the parent message. Use slack_search_public or slack_read_channel to find these values. Use slack_search_public with "is:thread" to find threads by content. Use slack_send_message with thread_ts to reply to a thread.

```

Input schema:

```json
{
  "properties": {
    "channel_id": {
      "description": "Channel, private group, or IM channel to fetch thread replies for",
      "type": "string"
    },
    "cursor": {
      "description": "Paginate through collections of data by setting the cursor parameter to a next_cursor attribute returned by a previous request",
      "type": "string"
    },
    "latest": {
      "description": "End of time range of messages to include in results. Slack ts format string (e.g. \"1234567890.123456\").",
      "type": "string"
    },
    "limit": {
      "description": "Number of messages to return, between 1 and 1000. Default value is 100.",
      "type": "integer"
    },
    "message_ts": {
      "description": "Timestamp of the parent message (e.g. \"1234567890.123456\"). Must be a string in Slack ts format with a decimal point.",
      "type": "string"
    },
    "oldest": {
      "description": "Start of time range of messages to include in results. Slack ts format string (e.g. \"1234567890.123456\").",
      "type": "string"
    },
    "response_format": {
      "description": "Level of detail: 'detailed' (default, includes reactions + thread info) or 'concise'.",
      "type": "string"
    }
  },
  "required": [
    "channel_id",
    "message_ts"
  ],
  "type": "object"
}
```

#### `slack_xaa-slack_read_user_profile`

Metadata: `{"annotations": {"destructiveHint": false, "idempotentHint": true, "openWorldHint": true, "readOnlyHint": true, "title": "Read user profile"}, "title": "Read user profile"}`

Description:

```text
Retrieves detailed profile information for a Slack user: contact info, status, timezone, organization, and role. Read-only. Defaults to current user if user_id not provided.

Use slack_search_users to find a user ID by name or email.

```

Input schema:

```json
{
  "properties": {
    "include_locale": {
      "description": "Include user's locale information. Default: false",
      "type": "boolean"
    },
    "response_format": {
      "description": "Level of detail in response. 'detailed' includes all fields, 'concise' shows essential info. Default: detailed'",
      "enum": [
        "detailed",
        "concise"
      ],
      "type": "string"
    },
    "user_id": {
      "description": "Slack user ID to look up (e.g., 'U0ABC12345'). Defaults to current user if not provided",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

#### `slack_xaa-slack_schedule_message`

Metadata: `{"annotations": {"destructiveHint": false, "idempotentHint": false, "openWorldHint": true, "readOnlyHint": false, "title": "Schedule message"}, "title": "Schedule message"}`

Description:

```text
Schedules a message for future delivery to a Slack channel. Does NOT send immediately — use slack_send_message for that.

post_at must be a Unix timestamp at least 2 minutes in the future, max 120 days out. Message is markdown formatted. Once scheduled, cannot be edited via API — user should use "Drafts and sent" in Slack UI.

Thread replies: provide thread_ts and optionally reply_broadcast=true. Cannot schedule in externally shared (Slack Connect) channels.

Use slack_search_channels to find channel IDs, slack_search_users to find user IDs (usable as channel_id for DMs).

```

Input schema:

```json
{
  "properties": {
    "channel_id": {
      "description": "Channel where message will be scheduled",
      "type": "string"
    },
    "message": {
      "description": "Message content to schedule",
      "type": "string"
    },
    "post_at": {
      "description": "Unix timestamp when message should be sent (2 min future minimum, 120 days max)",
      "type": "integer"
    },
    "reply_broadcast": {
      "description": "Broadcast thread reply to channel",
      "type": "boolean"
    },
    "thread_ts": {
      "description": "Message timestamp to reply to (for thread replies)",
      "type": "string"
    }
  },
  "required": [
    "channel_id",
    "message",
    "post_at"
  ],
  "type": "object"
}
```

#### `slack_xaa-slack_search_channels`

Metadata: `{"annotations": {"destructiveHint": false, "idempotentHint": true, "openWorldHint": true, "readOnlyHint": true, "title": "Search channels"}, "title": "Search channels"}`

Description:

```text
Search for Slack channels by name or description. Returns channel names, IDs, topics, purposes, and archive status.

Query tips: use terms matching channel names/descriptions (e.g., "engineering", "project alpha"). Names are typically lowercase with hyphens.

Use slack_read_channel to read messages from a known channel. Use slack_search_public to search message content across channels.

```

Input schema:

```json
{
  "properties": {
    "channel_types": {
      "description": "Comma-separated list of channel types to include in the search. Defaults to public_channel. Mix and match channel types by providing a comma-separated list of any combination of public_channel, private_channel. Example: public_channel,private_channel; Second Example: public_channel",
      "type": "string"
    },
    "cursor": {
      "description": "The cursor returned by the API. Omit this parameter for the first request, and use this to get the next page of results",
      "type": "string"
    },
    "include_archived": {
      "description": "Include archived channels in the search results",
      "type": "boolean"
    },
    "limit": {
      "description": "Number of results to return, up to a max of 20. Defaults to 20.",
      "type": "integer"
    },
    "query": {
      "description": "Search query for finding channels",
      "type": "string"
    },
    "response_format": {
      "description": "Level of detail (default: 'detailed'). Options: 'detailed', 'concise'",
      "enum": [
        "detailed",
        "concise"
      ],
      "type": "string"
    }
  },
  "required": [
    "query"
  ],
  "type": "object"
}
```

#### `slack_xaa-slack_search_public`

Metadata: `{"annotations": {"destructiveHint": false, "idempotentHint": true, "openWorldHint": true, "readOnlyHint": true, "title": "Search public messages and files"}, "title": "Search public messages and files"}`

Description:

```text
Searches for messages, files in public Slack channels ONLY. Current logged in user's user_id is U0CC1K1MFNX.

`slack_search_public` does NOT generally require user consent for use, whereas you should request and wait for user consent to use `slack_search_public_and_private`.

---
`query` should include keywords or natural language question with search modifiers.

Search modifiers:
  in:channel-name / in:<#C123456> / -in:channel   Channel filter
  in:<@U123456> / in:@username                     DM filter
  from:<@U123456> / from:username                  Author filter (angle brackets are literal for IDs)
  to:<@U123456> / to:me                            Recipient filter
  creator:@user                                    Canvas creator filter
  is:thread / is:saved / has:pin / has:link / has:file  Content filters
  has::emoji: / hasmy::emoji:                      Reaction filters
  before:YYYY-MM-DD / after:YYYY-MM-DD / on:YYYY-MM-DD / during:month  Date filters
  "exact phrase" / -word / * (wildcard, min 3 chars)  Text matching

File search: use `content_types="files"` with `type:` filter (images, documents, pdfs, spreadsheets, presentations, canvases, lists, emails, audio, videos). All standard modifiers work with file searches.

Search types:
1. Natural language —  ❌ Semantic search is not available for this user.
2. Keyword — exact matches, space-separated = AND, no boolean operators (AND/OR/NOT).

Use slack_read_thread for thread details, slack_read_canvas for canvas content, slack_read_channel for surrounding messages.

Strategy: break into multiple small searches, use modifiers to narrow, try keyword then semantic (or vice versa), broaden if 0 results.

---

Examples:
  ✅ Use
    query="What's our holiday schedule? in:#general"
    query="bug report after:2024-01-08" sort="timestamp"
    query="from:<@U123456> in:dev bug report"

Additional parameters:
  include_context (Optional[bool])  Include surrounding context messages for each result (default: true). Ignored when response_format is 'concise', which never shows context. Set to false to reduce response size.
  max_context_length (Optional[int])  Max character length for each context message. Longer messages are truncated.
```

Input schema:

```json
{
  "properties": {
    "after": {
      "description": "Only messages after this Unix timestamp (inclusive)",
      "type": "string"
    },
    "before": {
      "description": "Only messages before this Unix timestamp (inclusive)",
      "type": "string"
    },
    "content_types": {
      "description": "Content types to include, a comma-separated list of any combination of messages, files. Here's more info about the content types: messages: Slack messages from public channels accessible to the acting user\nfiles: Files of all types accessible to the acting user\n",
      "type": "string"
    },
    "context_channel_id": {
      "description": "Context channel ID to support boosting the search results for a channel when applicable",
      "type": "string"
    },
    "cursor": {
      "description": "The cursor returned by the API. Omit this parameter for the first request, and use this to get the next page of results",
      "type": "string"
    },
    "include_bots": {
      "description": "Include bot messages (default: false)",
      "type": "boolean"
    },
    "include_context": {
      "description": "Include surrounding context messages for each result (default: true). Ignored when response_format is 'concise', which never shows context. Set to false to reduce response size.",
      "type": "boolean"
    },
    "limit": {
      "description": "Number of results to return, up to a max of 20. Defaults to 20.",
      "type": "integer"
    },
    "max_context_length": {
      "description": "Max character length for each context message. Longer messages are truncated.",
      "type": "integer"
    },
    "only_my_channels": {
      "description": "Limit results to public channels the user is a member of. Set to true when the user asks to search only their own or joined channels. Default: false.",
      "type": "boolean"
    },
    "query": {
      "description": "Search query (e.g., 'bug report', 'from:<@U0123456ABC> in:dev')",
      "type": "string"
    },
    "response_format": {
      "description": "Level of detail (default: 'detailed'). Options: 'detailed', 'concise'",
      "enum": [
        "detailed",
        "concise"
      ],
      "type": "string"
    },
    "sort": {
      "description": "Sort order: 'score' (relevance, default) or 'timestamp' (newest first unless sort_dir is 'asc').",
      "enum": [
        "score",
        "timestamp"
      ],
      "type": "string"
    },
    "sort_dir": {
      "description": "Sort direction (default: 'desc'). Options: 'asc', 'desc'",
      "enum": [
        "asc",
        "desc"
      ],
      "type": "string"
    }
  },
  "required": [
    "query"
  ],
  "type": "object"
}
```

#### `slack_xaa-slack_search_public_and_private`

Metadata: `{"annotations": {"destructiveHint": false, "idempotentHint": true, "openWorldHint": true, "readOnlyHint": true, "title": "Search messages and files"}, "title": "Search messages and files"}`

Description:

```text
Searches for messages, files in ALL Slack channels, including public channels, private channels, DMs, and group DMs. Current logged in user's user_id is U0CC1K1MFNX.

---
`query` should include keywords or natural language question with search modifiers.

Search modifiers:
  in:channel-name / in:<#C123456> / -in:channel   Channel filter
  in:<@U123456> / in:@username                     DM filter
  from:<@U123456> / from:username                  Author filter (angle brackets are literal for IDs)
  to:<@U123456> / to:me                            Recipient filter
  creator:@user                                    Canvas creator filter
  is:thread / is:saved / has:pin / has:link / has:file  Content filters
  has::emoji: / hasmy::emoji:                      Reaction filters
  before:YYYY-MM-DD / after:YYYY-MM-DD / on:YYYY-MM-DD / during:month  Date filters
  "exact phrase" / -word / * (wildcard, min 3 chars)  Text matching

File search: use `content_types="files"` with `type:` filter (images, documents, pdfs, spreadsheets, presentations, canvases, lists, emails, audio, videos). All standard modifiers work with file searches.

Search types:
1. Natural language —  ❌ Semantic search is not available for this user.
2. Keyword — exact matches, space-separated = AND, no boolean operators (AND/OR/NOT).

Use slack_read_thread for thread details, slack_read_canvas for canvas content, slack_read_channel for surrounding messages.

Strategy: break into multiple small searches, use modifiers to narrow, try keyword then semantic (or vice versa), broaden if 0 results.

---

Examples:
  ✅ Use (with user consent)
    query="What's our holiday schedule? in:#general"
    query="bug report after:2024-01-08" sort="timestamp"
    query="from:<@U123456> in:dev bug report"

Additional parameters:
  include_context (Optional[bool])  Include surrounding context messages for each result (default: true). Ignored when response_format is 'concise', which never shows context. Set to false to reduce response size.
  max_context_length (Optional[int])  Max character length for each context message. Longer messages are truncated.
```

Input schema:

```json
{
  "properties": {
    "after": {
      "description": "Only messages after this Unix timestamp (inclusive)",
      "type": "string"
    },
    "before": {
      "description": "Only messages before this Unix timestamp (inclusive)",
      "type": "string"
    },
    "channel_types": {
      "description": "Comma-separated list of channel types to include in the search. Defaults to 'public_channel,private_channel,mpim,im' (all channel types including private channels, group DMs, and DMs). Mix and match channel types by providing a comma-separated list of any combination of `public_channel`, `private_channel`, `mpim`, `im`. To search only DMs and group DMs, pass `im,mpim` here instead of adding an `is:dm` filter; you don't need both.",
      "type": "string"
    },
    "content_types": {
      "description": "Content types to include, a comma-separated list of any combination of messages, files. Here's more info about the content types: messages: Slack messages from channels accessible to the acting user\nfiles: Files of all types accessible to the acting user\n",
      "type": "string"
    },
    "context_channel_id": {
      "description": "Context channel ID to support boosting the search results for a channel when applicable",
      "type": "string"
    },
    "cursor": {
      "description": "The cursor returned by the API. Omit this parameter for the first request, and use this to get the next page of results",
      "type": "string"
    },
    "include_bots": {
      "description": "Include bot messages (default: false)",
      "type": "boolean"
    },
    "include_context": {
      "description": "Include surrounding context messages for each result (default: true). Ignored when response_format is 'concise', which never shows context. Set to false to reduce response size.",
      "type": "boolean"
    },
    "limit": {
      "description": "Number of results to return, up to a max of 20. Defaults to 20.",
      "type": "integer"
    },
    "max_context_length": {
      "description": "Max character length for each context message. Longer messages are truncated.",
      "type": "integer"
    },
    "only_my_channels": {
      "description": "Limit results to channels the user is a member of. Set to true when the user asks to search only their own or joined channels. Default: false.",
      "type": "boolean"
    },
    "query": {
      "description": "Search query using Slack's search syntax (e.g., 'in:#general from:@user important')",
      "type": "string"
    },
    "response_format": {
      "description": "Level of detail (default: 'detailed'). Options: 'detailed', 'concise'",
      "enum": [
        "detailed",
        "concise"
      ],
      "type": "string"
    },
    "sort": {
      "description": "Sort order: 'score' (relevance, default) or 'timestamp' (newest first unless sort_dir is 'asc').",
      "enum": [
        "score",
        "timestamp"
      ],
      "type": "string"
    },
    "sort_dir": {
      "description": "Sort direction (default: 'desc'). Options: 'asc', 'desc'",
      "enum": [
        "asc",
        "desc"
      ],
      "type": "string"
    }
  },
  "required": [
    "query"
  ],
  "type": "object"
}
```

#### `slack_xaa-slack_search_users`

Metadata: `{"annotations": {"destructiveHint": false, "idempotentHint": true, "openWorldHint": true, "readOnlyHint": true, "title": "Search users"}, "title": "Search users"}`

Description:

```text
Search for Slack users by name, email, or profile attributes (department, role, title).
Current logged in user's Slack user_id is U0CC1K1MFNX.

Query syntax: full names ("John Smith"), partial names ("John"), emails ("john@company.com"), departments/roles ("engineering"), combinations ("John engineering"), exclusions ("engineering -intern"). Space-separated terms = AND.

Use slack_read_user_profile for detailed info on a known user ID. Use slack_search_public with from: filter to find messages by a user.

```

Input schema:

```json
{
  "properties": {
    "cursor": {
      "description": "The cursor returned by the API. Omit this parameter for the first request, and use this to get the next page of results",
      "type": "string"
    },
    "limit": {
      "description": "Number of results to return, up to a max of 20. Defaults to 20.",
      "type": "integer"
    },
    "query": {
      "description": "Search query for finding users. Accepts names, email address, and other attributes in profile\n\nExamples:\n  - \"John Smith\" - exact name match\n  - john@company - find users with john@company in email\n  - engineering -intern - users with \"engineering\" but not \"intern\" in profile",
      "type": "string"
    },
    "response_format": {
      "description": "Level of detail (default: 'detailed'). Options: 'detailed', 'concise'",
      "enum": [
        "detailed",
        "concise"
      ],
      "type": "string"
    }
  },
  "required": [
    "query"
  ],
  "type": "object"
}
```

#### `slack_xaa-slack_send_message`

Metadata: `{"annotations": {"destructiveHint": false, "idempotentHint": false, "openWorldHint": true, "readOnlyHint": false, "title": "Send message"}, "title": "Send message"}`

Description:

```text
Sends a message to a Slack channel or user. To DM a user, use their user_id as channel_id. If the user wants to send a message to themselves, the current logged in user's user_id is U0CC1K1MFNX. Return the message link to the user.

Message uses standard markdown (**bold**, _italic_, `code`, ~~strikethrough~~, >blockquotes, lists, links, code blocks, tables, headers). Limited to 5000 chars per text element. Tables use standard Markdown syntax with `|` as column delimiters. Do NOT escape the structural `|` characters that form the table borders and column separators. Only escape `|` as `\|` when a literal pipe character appears inside a cell value. Code blocks can include a language specifier (e.g. ```python, ```js, ```bash) for syntax highlighting and copy functionality. Do not include sensitive info in link query params. Cannot post to externally shared (Slack Connect) channels.

Thread replies: set thread_ts to parent message timestamp, reply_broadcast=true to also post to channel.

Link previews: when your message contains links from apps installed in the workspace (e.g. GitHub, Jira, Figma), set unfurl_app_links=true to enable rich link previews for those URLs.

Use slack_search_channels to find channel IDs, slack_search_users to find user IDs.
If user has not reviewed the message, use slack_send_message_draft instead.

```

Input schema:

```json
{
  "properties": {
    "channel_id": {
      "description": "Search all channels",
      "type": "string"
    },
    "draft_id": {
      "description": "ID of the draft to delete after sending",
      "type": "string"
    },
    "message": {
      "description": "Add a message",
      "type": "string"
    },
    "reply_broadcast": {
      "description": "Also send to conversation",
      "type": "boolean"
    },
    "thread_ts": {
      "description": "Provide another message's ts value to make this message a reply",
      "type": "string"
    },
    "unfurl_app_links": {
      "description": "Set to true to enable link previews from installed apps (e.g. GitHub, Jira) in the message",
      "type": "boolean"
    }
  },
  "required": [
    "channel_id",
    "message"
  ],
  "type": "object"
}
```

#### `slack_xaa-slack_send_message_draft`

Metadata: `{"_meta": {"ui": {"resourceUri": "ui://send-message-input.html"}}, "annotations": {"destructiveHint": false, "idempotentHint": false, "openWorldHint": true, "readOnlyHint": false, "title": "Create a draft message"}, "title": "Create a draft message"}`

Description:

```text
Creates a draft message in a Slack channel. The draft is saved to the user's "Drafts & Sent" in Slack without sending it.

## When to Use
- User wants to prepare a message without sending it immediately
- User needs to compose a message for later review or sending
- User wants to draft a message to a specific channel

## When NOT to Use
- User wants to send a message immediately (use `slack_send_message` instead)
- User wants to schedule a message (use `slack_send_message` with scheduling)
- User wants to create drafts in multiple channels (call this tool multiple times)

## Input Parameters:
- `channel_id`: Single channel ID where the draft should be created
- `message`: The draft message content using standard markdown. Supports **bold**, _italic_, `code`, ~strikethrough~, >blockquotes, lists, links, and code blocks.
- `thread_ts` (optional): Timestamp of the parent message to create a draft reply in a thread (e.g., "1234567890.123456")

## Output:
Returns `channel_link` - a Slack web client URL (e.g., https://app.slack.com/client/T123/C456) that opens the channel in the web app where the draft was created.

## Finding value for `channel_id` input:
- Use `slack_search_users` tool to find user ID for DMs, then use their user_id as the channel_id

## Error Codes:
- `channel_not_found`: Invalid channel ID or user does not have access to the channel
- `not_in_channel`: The user is not a member of the channel, so they could not send the draft
- `attached_draft_exists`: A draft already exists for this channel (user should edit or delete the existing draft first)
- `failed_to_create_draft`: Draft creation failed for an unknown reason

## Notes:
- Drafts are created as attached drafts (linked to the specific channel)
- User must have write access to the channel and be a member of it
- Only one attached draft is allowed per channel - if a draft already exists, you'll get an error

```

Input schema:

```json
{
  "properties": {
    "channel_id": {
      "description": "Channel to create draft in",
      "type": "string"
    },
    "message": {
      "description": "The message content in standard markdown",
      "type": "string"
    },
    "thread_ts": {
      "description": "Timestamp of the parent message to create a draft reply in a thread",
      "type": "string"
    }
  },
  "required": [
    "channel_id",
    "message"
  ],
  "type": "object"
}
```

