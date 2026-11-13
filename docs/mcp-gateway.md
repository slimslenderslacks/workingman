# How a sandbox reaches MCP servers

Every sandbox workingman creates reaches MCP servers (github, logseq, gmail,
…) through **its own sub-gateway** of the host's
[standalone-codemode](https://github.com/slimslenderslacks/standalone-codemode)
process. The sandbox gets one URL, `MCP_URL`, and Claude Code inside it sees
one MCP server, `okta-gateway`, which exposes only the servers the task asked
for.

> **`--static-mcp` is no longer used.** workingman never passes
> `sbx create --static-mcp` (or any other sbx static-MCP flag) on any path:
> ACP sessions, terminal-tab sessions, the TUI's `:session` tabs, or when a
> sandbox is reused or recreated. A task's `static_mcps` field is still read,
> but it now names the servers of the sandbox's gateway. `acp-wrapper
> --static-mcp` is accepted only for compatibility. It is ignored, with a
> warning.

Code: [`internal/mcpgateway`](../internal/mcpgateway/doc.go) (the
`/gateways` client, the per-sandbox lifecycle and the state store),
[`internal/acpwrapper`](../internal/acpwrapper/doc.go) (ACP sessions) and
[`internal/runner`](../internal/runner/doc.go) (terminal-tab and `:session`
sandboxes).

## Who holds what

```
 host                                                     sandbox
 ┌──────────────────────────────────────────────┐        ┌───────────────────────────┐
 │ standalone-codemode  (--port 3333)           │        │ claude (ACP)              │
 │   catalog: github, logseq, gmail, …          │        │   MCP server okta-gateway │
 │   credentials: 1Password, OAuth tokens,      │◀───────│   = $MCP_URL              │
 │   header scripts                             │  HTTP  │                           │
 │   /gateways/<id>  ← one per sandbox          │        │ MCP_URL=http://host.docker│
 │                                              │        │  .internal:3333/gateways/…│
 │ workingman (orch / acp-wrapper)              │        └───────────────────────────┘
 │   POST /gateways, DELETE /gateways/<id>      │
 │   ~/.workingman/gateways/<sandbox>.json      │
 │   + its own github-only gateway (orch-daemon)│
 └──────────────────────────────────────────────┘
```

- **github, logseq, gmail, etc. are catalog entries** in the `catalog.yaml`
  that standalone-codemode loads at startup (see its
  [catalog reference](https://github.com/slimslenderslacks/standalone-codemode/blob/main/docs/catalog.md)).
  standalone-codemode holds all of their credentials: OAuth tokens (github),
  secret-interpolated headers (`logseq`'s `Bearer ${mcp_http_token}`, read
  from 1Password), and `header-script` helpers that run on the host (`gmail`'s
  `/opt/okta-cpe/Scripts/fetch-mcp-headers.sh`). Nothing secret goes into
  the sandbox. The sandbox only gets an unguessable gateway URL.
- **workingman** only decides which catalog names each sandbox gets, mints
  and deletes the sub-gateways, sets the sandbox's network policy, and passes
  `MCP_URL` in.
- **acp-kit** (`claude-acp-client`, inside the sandbox) registers `MCP_URL`
  with Claude Code as the `okta-gateway` MCP server, and then removes the
  stock sbx `mcp-gateway` server (see
  [okta-gateway only](#okta-gateway-only-mcp-gateway-is-removed)).

## Which servers a sandbox gets

A sandbox's gateway is minted with `POST <gateway>/gateways` and the body
`{"servers": [...]}`. The list is:

1. the task's **`static_mcps`** when it names any. Each entry is a catalog
   server name. The commit agent shares its task's sandbox, so it uses the
   same list.
2. otherwise the **default set**, `--mcp-gateway-servers` /
   `WORKINGMAN_MCP_GATEWAY_SERVERS` (comma-separated). This covers tasks with
   no `static_mcps` and the agents that have no task (project, planning,
   wolf, archive, workingman).
3. The review agent always asks for `[github]`.
4. The TUI's `:session` sandboxes ask for **no servers** and a dynamic
   gateway (`{"servers": [], "dynamic": true}`): the agent finds and attaches
   what it needs with `mcp-find` / `mcp-add`. With `--mcp-gateway-static`
   they get no gateway.

If the list is empty (no `static_mcps` and no default set), the sandbox gets
no gateway and no `MCP_URL`. Its network policy still gets the gateway's
allow rules (see below).

Names the catalog doesn't have make `POST /gateways` return `400
{"error":"unknown_servers","unknown":[...]}`. workingman logs a warning,
drops those names and POSTs again with the rest. A sandbox gets no gateway
only when none of its names are known.

### Example task

```yaml
# tasks/triage-inbox.yaml
name: triage-inbox
description: |
  Read today's unread Gmail threads about the release, cross-reference the
  open GitHub issues they mention, and add a summary page to the Logseq
  journal.
depends_on: []
status: ready
attempts: 0
failure_reason: ""
blocked_reason: ""
# Catalog server names on the host's standalone-codemode. They become
# POST /gateways {"servers": ["github", "logseq", "gmail"]}. They are NOT
# passed as sbx --static-mcp flags.
static_mcps: [github, logseq, gmail]
```

Inside that task's sandbox, Claude Code has one MCP server, `okta-gateway`,
with the github, logseq and gmail tools behind it.

## Order of operations

For a sandbox that does not exist yet, both creation paths
(`acpwrapper.createSandbox` and `runner.DefaultSandboxCreator`) run these
steps in this order:

1. **Create the gateway.** `POST <gateway>/gateways {"servers": [...]}`
   (`mcpgateway.Manager.Prepare`). This happens before the sandbox exists.
   The returned URL's loopback host (`localhost`, `127.0.0.1`, `::1`) is
   rewritten to `host.docker.internal`, keeping the port and path. For
   example, `http://localhost:3333/gateways/3f1c…` becomes
   `http://host.docker.internal:3333/gateways/3f1c…`.
2. **Create the sandbox.** `sbx create claude --name <name> [--kit <kit>]
   <workspaces…>`, with no `--static-mcp` flags.
3. **Apply the network policy.** First the task's own `policies:`, in order,
   then the gateway's allow rules. For a loopback gateway URL those are
   `sbx policy allow network --sandbox <name> host.docker.internal:<port>`
   and `… localhost:<port>`. For any other host it is `<host>:<port>`. This
   happens before anything runs in the sandbox. After these steps the
   gateway is recorded. If `sbx create` or any policy fails, the gateway is
   DELETEd and nothing is recorded.
4. **Launch the agent with `MCP_URL`.** `mcpgateway.Manager.Ensure` gets
   the recorded gateway (still live, same servers) and the agent starts with
   `sbx exec -e MCP_URL=<sandbox url> …`. `sbx create` has no flag for
   environment variables, so `MCP_URL` is passed on every `sbx exec`.
   acp-wrapper also writes `export MCP_URL='…'` into the sandbox's
   `/etc/sandbox-persistent.sh`, so a shell you open in the sandbox sees it
   too.
5. **acp-kit registers it.** Before it starts the ACP bridge,
   `claude-acp-client` runs the equivalent of
   `claude mcp add --transport http --scope user okta-gateway "$MCP_URL"`.
   An entry that already points at `$MCP_URL` is left alone. A stale entry
   from an older gateway is replaced. If `MCP_URL` is empty, nothing is
   registered. User-scope settings load for every ACP session, so the
   gateway is there even when `session/new` sends `"mcpServers": []`. An
   interactive `claude` (`sbx run`) does the same at launch through the
   image's `claude` shim; see
   [Interactive sandboxes](#interactive-sandboxes-sbx-run).

6. **acp-kit removes `mcp-gateway`.** Once `okta-gateway` is registered,
   `claude-acp-client` also removes the stock sbx `mcp-gateway` server.
   See [okta-gateway only](#okta-gateway-only-mcp-gateway-is-removed).

For an ACP session, orch only passes the settings on to acp-wrapper
(`--mcp-gateway-url`, one `--mcp-server` per name, `--mcp-gateway-state`).
The wrapper owns the gateway as it owns the sandbox. For terminal-tab and
`:session` sandboxes, the runner does the same steps itself. A `:session`
sandbox follows `sbx-create.sh`: `sbx create claude --kit <acp-kit> -t
<ACP_IMAGE> -e MCP_URL=… -e <claude env>`, then `sbx run --name <sandbox>` in
the tab, so acp-kit's `claude` shim registers `okta-gateway`.

## okta-gateway only: `mcp-gateway` is removed

Stock sbx `claude` sandboxes come with a default MCP server named
`mcp-gateway` (`http://mcp-gateway.docker.internal/mcp`). sbx adds it at user
scope every time the sandbox starts. acp-kit's `claude-acp-client` removes it
(`claude mcp remove --scope user mcp-gateway`, and `--scope local` if it is
still there) after it has registered `okta-gateway`, on every run. So the
only MCP server the agent sees is `okta-gateway`, and every MCP call goes
through the sandbox's sub-gateway. If `MCP_URL` is empty or `okta-gateway`
could not be registered, `mcp-gateway` is left in place. A project-scope
`mcp-gateway` (from a repo's `.mcp.json`) is not removed; acp-kit only warns
about it. workingman sends `"mcpServers": []` in `session/new`, so it adds no
servers of its own.

### Interactive sandboxes (`sbx run`)

When `MCP_URL` is set, an interactive sandbox sees only `okta-gateway` too.
`sbx run <sandbox>` starts the interactive `claude`, which never goes through
`claude-acp-client`. In acp-kit's image, `claude` on `PATH` is a shim
(`claude-acp-shim`). Every time `claude` starts, the shim runs the same
registration as the ACP path (`CLAUDE_ACP_REGISTER_ONLY=1 claude-acp-client`):
it registers `$MCP_URL` as `okta-gateway` and removes `mcp-gateway`. Then it
starts the real binary (`claude-acp-real`). sbx re-adds `mcp-gateway` on every
sandbox start, but the shim runs after that, so `/mcp` lists only
`okta-gateway`. `sbx run` does not get `-e MCP_URL`, so if `MCP_URL` is not in
the environment, the shim reads it from `/etc/sandbox-persistent.sh`, where
acp-wrapper writes it (step 4 above). With no `MCP_URL` at all, the shim
changes nothing and `mcp-gateway` stays. A failed registration only warns, so
`claude` always starts.

The shim is only in the image (0.90.0 and later), not in the `--kit` mixin.
In a sandbox on the stock template or an older image, register the gateway
by hand before the interactive `claude`:
`sbx exec <sandbox> -- env CLAUDE_ACP_REGISTER_ONLY=1 claude-acp-client`.

### Image

This behavior shipped in acp-kit's image **`slimslenderslacks/claude-code-acp:0.90.0`**,
which is the `ACP_IMAGE` default in `start.sh` (orch `--acp-image`, acp-wrapper
`--template`). The image tag is acp-kit's `IMAGE_VERSION`. It is versioned
separately from the ACP bridge version (still 0.88.0) and bumped whenever
anything in the image changes.

### Loading the image

Sandboxes are created with `sbx create … -t <image> --pull missing`. The
sandbox runtime keeps its own image store, separate from the host docker's,
so build the image and load it there. From the acp-kit checkout:

```sh
make image-build        # or `make image-verify` to also check the baked-in claude-acp-client
docker save slimslenderslacks/claude-code-acp:0.90.0 -o /tmp/acp.tar
sbx template load /tmp/acp.tar
sbx template ls         # confirm the 0.90.0 tag is listed
```

`--pull missing` only pulls a tag the sandbox store doesn't have. Because
the fix has a new tag, the new image is used as soon as it is loaded. (If the
same tag had been rebuilt, the cached old image would keep being used.) If
0.90.0 is neither loaded nor pushed to the registry, `sbx create` fails at
PREPARE IMAGE. To use a different image, set `ACP_IMAGE` before running
`start.sh`. `ACP_IMAGE=""` uses the stock template.

### Sandboxes created from the old image

A new image only affects sandboxes created after the change. An existing
sandbox is never rebuilt:

- **New tasks always get a new sandbox.** The sandbox name comes from the
  project and task name (`runner.SandboxNameFor`), and workingman never
  adopts a sandbox under a different name. So every new task is created from
  the current `ACP_IMAGE`.
- **Reuse is for the same name only.** That covers a retry of the same task,
  the commit agent after its task, a restart after a signal, and sandboxes
  kept because their task failed or blocked or set `save_sandbox: true`.
  These sandboxes stay on the image they were created from. A clean exit
  removes the sandbox, so reuse never outlasts a task.
- **The entrypoint is still updated on create.** `--kit` layers acp-kit on
  top of the image, and its install step writes `claude-acp-client` from the
  kit checkout every time a sandbox is created. A sandbox created from the
  old 0.88.0 image with the current acp-kit checkout already removes
  `mcp-gateway` for ACP sessions. Only sandboxes created before the acp-kit
  change keep the old entrypoint. The interactive `claude` shim is not
  layered on by the kit, so a sandbox created from 0.89.0 or older keeps
  `mcp-gateway` under `sbx run` until it is recreated from 0.90.0.

To move a kept sandbox to the new image, remove it with
`sbx rm --force <sandbox>`. The next launch of that task creates it again
from the current `ACP_IMAGE`. You can also remove `mcp-gateway` from it by
hand: `sbx exec <sandbox> -- claude mcp remove --scope user mcp-gateway`.

## Reuse, recreation, cleanup and restarts

Gateway state is recorded per sandbox in
`<sessions-root>/../gateways/<sandbox>.json` (by default
`~/.workingman/gateways/`). The record holds the gateway id, the base URL it
was minted on, the host URL and sandbox URL, and the requested and served
server lists. `acp-wrapper --mcp-gateway-state` overrides the directory.
The store is deliberately outside the sessions root: a kept sandbox outlives
its session directory.

- **Reuse** (same sandbox name and the same mounts, e.g. a retry, or the
  commit agent after its task): the sandbox is not recreated, and the
  gateway's allow rules are applied again, best effort. `Ensure` then checks
  the recorded gateway. If it is still registered (`GET /gateways/<id>` is
  not `404`), it was minted on the same base URL, and it has the same server
  list, it is reused. Otherwise the stale one is DELETEd and a new one is
  minted. Because `MCP_URL` is passed per `sbx exec`, a reused sandbox always
  gets a valid URL. That matters because standalone-codemode keeps
  sub-gateways in memory only and loses them all when it restarts. A gateway
  that can't be reached at all counts as reusable. Minting a new one would
  fail the same way.
- **Recreation** (same name, different mounts): `sbx rm --force`, then
  DELETE the recorded gateway and drop its record, then run the full create
  sequence above with a new gateway.
- **Removal on exit**: when acp-wrapper removes its sandbox, it DELETEs the
  gateway. A kept sandbox (a failed or blocked task, or `save_sandbox: true`)
  keeps its gateway, so a relaunch reuses both.
- **Restart cleanup**: every acp-wrapper launch sweeps the store. A record
  whose sandbox `sbx ls` no longer lists, and that is older than 10 minutes
  (`mcpgateway.SweepGrace`), has its gateway DELETEd. This catches kept
  sandboxes that someone later removed by hand, and wrappers that died before
  their exit cleanup.

DELETE failures are only logged. A leaked sub-gateway costs memory in
standalone-codemode until it restarts.

## Talking MCP to a gateway from Go

Sandboxed agents reach their gateway through acp-kit. Host-side Go code (the
daemon's review fingerprint check) uses `mcpgateway.OpenSession(ctx,
gw.URL, …)` instead: a small streamable-HTTP MCP client with no third-party
dependencies. It sends `initialize`, keeps the `Mcp-Session-Id` header and
sends `notifications/initialized`; `CallTool` accepts both `application/json`
and `text/event-stream` responses; `Close` DELETEs the session best-effort.

Errors fall into three kinds:

- `errors.Is(err, mcpgateway.ErrSessionLost)` — HTTP 404, 400 on a request
  carrying a session id, or connection refused/reset. The session (and maybe
  the gateway) is gone; open a new one.
- `*mcpgateway.ToolError` — the tool returned `isError: true`. The session
  still works.
- `*mcpgateway.RPCError` — a JSON-RPC error, such as an unknown tool. The
  session still works.

## The daemon's own gateway (PR fingerprint check)

Besides the per-sandbox gateways, the orch daemon mints **one gateway of its
own**, over the `github` server only (static, no dynamics). It uses it for the
cheap PR fingerprint check that decides whether a watched PR changed and the
review agent needs to run. The check runs on the host and goes through
standalone-codemode's github credentials. The daemon doesn't need the `gh`
CLI or any local GitHub auth. Code: `internal/daemon/review_gateway.go` (the
gateway), `internal/daemon/review_mcp.go` (the session and the tool calls)
and `internal/daemon/review.go` (the poll gate).

### Gateway lifecycle

- **Startup.** With `--mcp-gateway-url` set, `Daemon.Run` starts the gateway
  loop off the startup path. It uses the same `mcpgateway.Client` that
  acp-wrapper uses: `POST /gateways {"servers": ["github"]}` (static).
  The record is kept under the fixed key `orch-daemon` in
  `<gateways-dir>/daemon/orch-daemon.json` (by default
  `~/.workingman/gateways/daemon/`). That subdirectory is skipped by
  acp-wrapper's sweep, so the sweep never releases the daemon's gateway.
- **Stale cleanup.** Before minting, the daemon checks for a recorded
  gateway. One is only there if a previous run crashed or couldn't delete
  it. The daemon DELETEs it and drops the record, so gateways don't pile up
  in standalone-codemode. If the stale gateway is on the current endpoint and
  can't be deleted, creation waits and retries, and the record is kept.
- **Failure and retry.** A failed creation never stops the daemon. It is
  retried with exponential backoff, 5s doubling up to 5m. A fingerprint call
  that arrives after the backoff window has passed also tries a creation on
  demand. While no gateway is available, every poll is *unverified* (see
  below).
- **Gateway gone.** If opening a session on the gateway's URL fails with
  `ErrSessionLost`, the gateway is gone, usually because standalone-codemode
  restarted and dropped its in-memory gateways. The daemon invalidates it,
  DELETEs it best-effort, mints a new one and retries the open once.
- **Shutdown.** The MCP session is closed first. Then the gateway is DELETEd
  (with a 10s limit) and its record removed. If the DELETE fails, the record
  is kept, so the next startup deletes the gateway as stale.

### The MCP session

The daemon keeps **one MCP session** (`mcpgateway.OpenSession`, client name
`orch-daemon`) on its gateway and uses it for every fingerprint tool call. The
session is opened lazily on the first call. If a call fails with
`ErrSessionLost`, the daemon drops the session and opens a new one. When that
works right away, the call is retried once on the new session. If the open
fails, the next one waits with exponential backoff (5s doubling up to 5m) and
logs each attempt, instead of failing forever. A `ToolError` or `RPCError`
doesn't drop the session. Each tool call has a 30s limit. Opening a session
(gateway URL plus `initialize`) also has a 30s limit.

### Fingerprint signals

Every read goes through the github server's `pull_request_read` tool. The
`method` argument picks the data:

| method | signal |
|---|---|
| `get` | head SHA, state (`open` / `closed` / `merged`) |
| `get_reviews` | latest review id |
| `get_comments` | latest conversation comment id |
| `get_review_comments` | review-thread count, unresolved-thread count, latest inline comment id |
| `get_check_runs` | check-run states at the head (conclusion once completed, otherwise status) |
| `get_status` | commit-status states at the head |

Lists are paged 100 at a time, up to 10 pages. The results fill the same
`project.PRFingerprint` shape that the review agent records, and are hashed
with `project.PRsFingerprint`. `observed_at` is not part of the hash. If any
watched PR can't be read, the check is treated as unavailable for the whole
project.

### Agent-recorded fallback

At the end of every poll, the review agent records the fingerprint it observed
under each `pull_requests:` entry in `project.yaml`:

```yaml
pull_requests:
  - repo: org/name
    number: 42
    head_sha: 3f2c…
    state: open
    fingerprint:
      latest_comment_id: 1234567
      latest_review_id: 7654321
      review_threads: 3
      unresolved_threads: 1
      check_states:
        - {name: build, state: success}
      observed_at: 2026-11-13T05:00:00Z
```

After each clean reconcile, the daemon records a **baseline**. It uses the MCP
fingerprint (source `mcp`) when it can get one. Otherwise it uses this
agent-recorded fingerprint (source `agent`). The two are directly comparable.
After a daemon restart, the in-memory baseline is seeded from the
agent-recorded fingerprint. That way an unchanged PR isn't re-reviewed just
because orch restarted.

### Poll backoff

The `#review` poll moves along `2m → 4m → 8m → 16m → 32m → 1h`:

- **quiet**: the MCP fingerprint matches the baseline. The review agent is
  not launched, and the poll moves one step slower.
- **changed / no baseline**: the review agent runs, and the poll resets to 2m.
- **unverified**: there is no MCP fingerprint (gateway or session down, a
  tool call failed, or no PR recorded yet). The review agent still runs, so
  no signal is hidden, but the poll moves one step slower. When the new
  baseline taken after that run differs from the previous one, the poll
  resets to 2m.
- `:review` dispatches the review agent at once and resets the poll to 2m.

The snapshot's `review_poll` shows the step (`backoff_index`), whether a
baseline exists (`has_baseline`) and where it came from (`baseline_source`).
See [state-snapshot.md](state-snapshot.md).

### Log and audit events

Each event below goes to the audit log (`--audit-log`). The gateway and
session events also go to `daemon.log` beside it. Those lines start with
`review gateway:` or `review MCP:`.

| Audit event | Fields | Meaning |
|---|---|---|
| `review_gateway_created` | `id`, `url`, `endpoint`, `servers`, `gaps`? | The daemon's github-only gateway was minted. `gaps` lists anything the gateway isn't serving, for example github in `needs_auth`. `daemon.log` also prints a `WARNING` for each gap. |
| `review_gateway_create_error` | `endpoint`, `attempt`, `retry_in`, `err` | Creating the gateway failed (including a stale gateway that couldn't be deleted). It is retried after `retry_in`. |
| `review_gateway_stale_deleted` | `id`, `endpoint`, `created_at` | A gateway left behind by a previous run was deleted at startup. |
| `review_gateway_stale_delete_error` | `id`, `endpoint`, `err` | A gateway left behind by a previous run couldn't be deleted. |
| `review_gateway_record_error` | `id`?, `dir`, `err` | The gateway record couldn't be written or read. An unwritten record means the gateway leaks if orch crashes. An unreadable record is dropped. |
| `review_gateway_invalidated` | `id`, `url` | The gateway was found gone and a new one will be minted. |
| `review_gateway_deleted` | `id`, `endpoint` | The gateway was deleted, at shutdown or after being invalidated. |
| `review_gateway_delete_error` | `id`, `endpoint`, `err` | Deleting the gateway failed. The record is kept, so the next startup deletes the gateway as stale. |
| `review_mcp_session_opened` | `url`, `session` | The first MCP session was opened. |
| `review_mcp_session_error` | `url`, `attempt`, `retry_in`, `err` | Opening or recreating a session failed. The next try is after `retry_in`. |
| `review_mcp_gateway_gone` | `url`, `err` | Opening a session found the gateway gone. The gateway is invalidated. |
| `review_mcp_session_lost` | `url`, `session`, `err` | A tool call found the session gone. It will be recreated. |
| `review_mcp_session_recreate` | `attempt` | A lost session is being recreated. |
| `review_mcp_session_recreated` | `url`, `session`, `attempts` | A lost session was recreated. |
| `review_mcp_tool_error` | `path`, `tool`, `method`, `repo`, `pr`, `err` | A fingerprint tool call (or decoding its result) failed. |
| `review_mcp_session_closed` / `review_mcp_session_close_error` | `url`, `session`, `err`? | The session was closed at shutdown, or closing it failed. |
| `review_fingerprint_error` | `path`, `err` | No MCP fingerprint for the project this time, so the poll is *unverified*. `err` gives the cause: no gateway configured, a gateway or session that isn't available, or the failing tool call. |
| `review_baseline` | `path`, `source`, `changed` | A baseline was recorded after a reconcile, from `mcp` or `agent`. |
| `review_baseline_unavailable` | `path` | Neither source produced a fingerprint. The previous baseline stays. |
| `review_poll_quiet` | `path`, `baseline`, `next` | Nothing changed. The agent was skipped and the poll slowed to `next`. |
| `review_poll_fired` | `path`, `reason`, `next` | The review agent was dispatched. `reason` is `changed`, `no_baseline` or `unverified`. |
| `review_poll_forced` | `path` | `:review` dispatched the agent at once. |

To find out why reviews run on every tick, look for `review_poll_fired
reason=unverified` and read the `review_fingerprint_error` and
`review_gateway_*` / `review_mcp_*` events around it.

## Configuration

| orch flag | Environment variable | Meaning |
|---|---|---|
| `--mcp-gateway-url` | `WORKINGMAN_MCP_GATEWAY_URL` | Base URL of the host's standalone-codemode, e.g. `http://localhost:3333`. Empty turns the feature off: no gateways, no `MCP_URL`, no extra policy rules. Still no `--static-mcp`. |
| `--mcp-gateway-servers` | `WORKINGMAN_MCP_GATEWAY_SERVERS` | Comma-separated default server set for sandboxes whose task names no `static_mcps` (and for agents with no task). Empty means those sandboxes get no gateway. |

The flag wins over the environment variable. orch writes the effective
values to the audit log as `mcp_gateway_config`.

```sh
# host: the gateway, with the full catalog available
./bin/gateway --port 3333 --catalog catalog.yaml

# host: orch
./orch --root ~/orch --acp-kit <kit> \
  --mcp-gateway-url http://localhost:3333 \
  --mcp-gateway-servers github
```

acp-wrapper flags (set by orch, so you normally don't pass them yourself):
`--mcp-gateway-url`, `--mcp-server <name>` (repeatable),
`--mcp-gateway-state <dir>`, `--mcp-gateway-wait <duration>` (default 2m).

## Troubleshooting

Where to look: for ACP sessions, `acp-wrapper.log` next to orch's audit log
(e.g. `~/orch/acp-wrapper.log`; the lines start with
`acp-wrapper: session <id>:` or `mcpgateway:`). For terminal-tab and `:session` sandboxes, the audit log
(`mcp_gateway_failed`).

- **Gateway not answering yet.** standalone-codemode listens only after
  attaching its whole catalog, so right after a restart it refuses
  connections. acp-wrapper (and `:session`) wait for `GET /healthz` for up to
  `--mcp-gateway-wait` (2m), logging `gateway … not ready …; waiting` and
  `ready after …`. If it never answers, the launch **fails** with `MCP gateway
  (okta-gateway) unavailable` instead of starting the agent without
  `MCP_URL`; the daemon retries it like any failed launch. `start.sh` waits
  the same way (`WORKINGMAN_MCP_GATEWAY_WAIT`, default 120s) before starting
  orch.
- **Gateway up, but a server missing.** Each requested server the new
  gateway is not serving is logged as `WARNING gateway <id> does not provide
  <server>: <state>, <n> tools[: why]`: `needs_auth` (run `gateway authorize
  <server>` on the host), `deferred` / `reconnecting` (the remote is
  unreachable; the gateway keeps retrying), `failed` (attach errored), or
  `connected, 0 tools`. A missing dynamic is reported the same way. The agent
  still starts with the gateway it got.
- **Not sbx's MCP support.** MCP servers never reach a sandbox through
  `sbx mcp`; `sbx mcp ls` / `sbx mcp add` / `sbx mcp auth` are not part of
  this setup.

- **Unknown server name → `400`.** The log shows
  `WARNING mcpgateway: unknown catalog server(s): <names>; creating the
  gateway over <rest> only`. Fix the task's `static_mcps` (or
  `--mcp-gateway-servers`), or add the entry to standalone-codemode's catalog
  and restart it. If every name is unknown, the sandbox gets no gateway. To
  check a name by hand:
  `curl -sX POST localhost:3333/gateways -d '{"servers":["gmail"]}'`.
- **Gateway not running.** The log shows `POST http://localhost:3333/gateways:
  … connection refused` and `WARNING no MCP gateway for sandbox …`. Start
  standalone-codemode on the port in `--mcp-gateway-url`. You don't need to
  recreate anything: the next launch of that sandbox mints a gateway and
  passes a new `MCP_URL`. If standalone-codemode restarted, every recorded
  gateway is gone (`GET /gateways/<id>` returns `404`), and each sandbox gets
  a new one on its next launch.
- **Network policy blocks the gateway.** Inside the sandbox, a request to
  `$MCP_URL` returns `403 Blocked by network policy: domain
  host.docker.internal`. Or `claude mcp list` shows `okta-gateway` as
  failing. Check `sbx policy ls` for allow rules on
  `host.docker.internal:<port>` and `localhost:<port>`. The port must match
  the one in `--mcp-gateway-url`. If you changed the port after the sandbox
  was created, the reuse path re-applies the new rules, but a local or org
  **deny** rule overrides any allow. See `sbx policy log`.
- **The agent still sees `mcp-gateway`.** Check whether the sandbox
  predates the acp-kit change (see
  [Sandboxes created from the old image](#sandboxes-created-from-the-old-image)).
  Check that `okta-gateway` registered: `mcp-gateway` is only removed once
  it has. A project-scope entry from a repo's `.mcp.json` is never removed.
- **`okta-gateway` is missing in Claude Code.** Check that `MCP_URL` is set
  (`sbx exec <sandbox> -- printenv MCP_URL`). If it is empty, check the
  causes above. If it is set, check `claude-acp-client`'s stderr for a
  registration warning.
- **The daemon's fingerprint check fails.** Look in `daemon.log` for
  `review gateway:` / `review MCP:` lines, and in the audit log for
  `review_gateway_create_error`, `review_mcp_session_error` and
  `review_mcp_tool_error`. If github shows up in `gaps` on
  `review_gateway_created`, it isn't authorized: run `gateway authorize
  github` on the host. The daemon keeps retrying with backoff, and reviews
  still run in the meantime (the polls are unverified). See
  [the daemon's own gateway](#the-daemons-own-gateway-pr-fingerprint-check).
- **A tool from another server shows up, or one is missing.** The gateway
  serves exactly the record's `servers` (in
  `~/.workingman/gateways/<sandbox>.json`). If you change a task's
  `static_mcps`, the next launch mints a new gateway.
