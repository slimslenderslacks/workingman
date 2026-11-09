# orch — autonomous claude-code project daemon

`orch` watches one or more directories for `project.yaml` files and runs
claude-code agents (project, planning, task, commit, wolf, archive) through a
state machine until the project is done — or blocked, in which case you get a
macOS notification and a wolf-agent session to drop into.

> **Package map:** [docs/packages.md](docs/packages.md) is the generated index of
> every `internal/` package — its role (daemon / tui / shared / wrapper),
> synopsis, exported types and the import graph. Read it first to find your way
> around the code; [docs/package-docs.md](docs/package-docs.md) explains the
> `doc.go` convention that feeds it.

```
status: ready    → planning agent (writes tasks/*.yaml, sets status: working)
status: working  → task agent → commit agent → next task → ... → status: done
status: blocked  → wolf agent + osascript notification
status: done     → terminal

cleanup: true    → archive agent (checked ahead of the status routing;
                   commits + pushes whatever needs it, then sets archive: true)
```

The daemon also reads each project's `cron:` field. A firing on an **idle**
project (`done`, or `ready`) starts a new cycle: the daemon sets `status: ready`
and `replan: true`, and the planning agent runs again — reading the previous
cycle's tasks for context, then deleting, reusing, or replacing them and leaving
what survives in `ready` for the task agents. A firing on a project with work in
flight (`working`) or one that needs attention (`blocked`) just re-evaluates it,
as before. Every schedule needs an explicit stop condition (`cron_until:` or
`cron_max_runs:`) — see [Example `project.yaml`](#example-projectyaml).
`cron_max_runs` counts cycles, not wake-ups: a firing that starts no cycle isn't
charged against it.

## Prerequisites

- Go 1.22+
- A terminal orch can open tabs in — [Ghostty](https://ghostty.org) (macOS via
  AppleScript, Linux via `ghostty +new-tab`), WezTerm or kitty. Sessions that
  need a human at a TTY (the `--wolf-host` wolf, the TUI's `:dir`/`:session`)
  open in a new tab titled after the project. orch detects the terminal it runs
  in; otherwise set `terminal: ghostty` in `~/.config/workingman/settings.yaml`.
  See [docs/terminal-tabs.md](docs/terminal-tabs.md).
- `claude` (claude-code CLI) — the actual agent worker
- `wsp` — multi-repo workspace manager. Repos must be registered (`wsp registry add ...`) before the planning step finishes. If you don't have wsp set up, run with `--workspace-manager=stub` instead.
- macOS for `osascript` notifications (optional; wolf still launches without them)

## SSH agent: commit signing and push (private ssh-agent)

`./start.sh` starts a **private `ssh-agent`** for the daemon, loads the
signing key into it straight from 1Password (never written to disk), and
exports its `SSH_AUTH_SOCK` so sbx forwards it into every sandbox. The same
agent serves both commit signing and `git push` over ssh remotes (wsp repos use
`git@host:org/repo.git`), so the commit agent's push depends on it too. sandboxd
must be (re)started with that `SSH_AUTH_SOCK` in its environment — a sandboxd
restarted without it creates sandboxes whose agent is empty and pushes fail.
See [docs/ssh-agent.md](docs/ssh-agent.md). 1Password's
own agent is not used: it needs a human to approve each signing operation, so
unattended runs would hang.

- The key comes from the 1Password item **"SSH github"** in vault
  **"Personal"** via `op read`. Override the location with `ORCH_SSH_KEY_REF`
  (an `op://` reference to an openssh-format private key).
- `op` must authenticate **non-interactively** (`OP_SERVICE_ACCOUNT_TOKEN`, or
  an already-unlocked CLI session); otherwise start.sh fails at startup.
- The agent's lifetime is tied to `start.sh`: it is killed (and its
  socket dir removed) when the script exits.
- If orch is started without it (`SSH_AUTH_SOCK` unset or the macOS launchd
  agent) the daemon logs an `ssh_auth_sock_unusable` audit event: commit
  signing and ssh push will fail. orch never falls back to 1Password's agent.
- `./check-signing.sh` diagnoses sandboxd forwarding an empty agent and can
  restart sandboxd with the private agent's socket.

## Build

```sh
go build -o orch ./cmd/orch
```

## Start the daemon

Pick a directory tree where you'll keep your project files. `orch` watches
this tree recursively; any subdirectory with a `project.yaml` becomes a
project.

Older versions of `orch` named this file `.project.yaml`. At startup the
daemon renames any legacy `.project.yaml` it finds under a root to
`project.yaml` (logged as `project_file_migrated`); a directory that already
has a `project.yaml` is left untouched.

```sh
mkdir -p ~/orch
./orch --root ~/orch --audit-log ~/orch/audit.log
```

Repeat `--root` for multiple trees.

For day-to-day use, run `./start.sh` instead. It rebuilds `orch` and
`acp-wrapper` from source, starts the private ssh-agent (see above), restarts
sandboxd bound to it, and then runs the daemon and TUI in the foreground of
the terminal you launched it from.
`Ctrl-C` stops orch and tears the private agent down. Watch the audit log in another shell:

```sh
tail -f ~/orch/audit.log
```

## MCP gateway (per-sandbox MCP servers)

Sandboxes reach MCP servers through the host's
[standalone-codemode](https://github.com/slimslenderslacks/standalone-codemode)
gateway, not through sbx static MCPs. Point orch at it:

```sh
./orch --root ~/orch --acp-kit <kit> \
  --mcp-gateway-url http://localhost:3333 \
  --mcp-gateway-servers gmail,github     # default set for tasks without static_mcps
# or: WORKINGMAN_MCP_GATEWAY_URL / WORKINGMAN_MCP_GATEWAY_SERVERS
```

Each time a sandbox is created (or reused):

1. **A sub-gateway is minted**: `POST <url>/gateways {"servers": [...]}`. The
   servers are the task's `static_mcps` when it lists any (each entry is a
   catalog server name), otherwise `--mcp-gateway-servers`. The review agent
   asks for `github`. Names the catalog doesn't have are dropped, with a
   warning. If no servers are left, the sandbox gets no gateway.
2. **The sandbox gets `MCP_URL`**: the returned URL, with a loopback host
   (`localhost`, `127.0.0.1`, `::1`) rewritten to `host.docker.internal`, same
   port and path, e.g. `http://host.docker.internal:3333/gateways/<id>`. `sbx
   create` has no flag for environment variables, so it is passed on every
   `sbx exec` as `-e MCP_URL=…`. The ACP wrapper also writes it into the
   sandbox's `/etc/sandbox-persistent.sh` (the env file every shell in the
   sandbox sources), so a shell you open there sees it too. acp-kit's
   `claude-acp-client` registers it as the `okta-gateway` MCP server.
3. **The network policy allows the gateway's port**: right after `sbx create`,
   after the task's own `policies:`, and before any `sbx exec`:
   `allow network host.docker.internal:<port>` and `localhost:<port>`. A
   non-loopback gateway gets `<host>:<port>`. The rules are re-applied, best
   effort, when an existing sandbox is reused.
4. **No `--static-mcp`**: `static_mcps` is still parsed from task files but
   is never passed to `sbx create`.
5. **Cleanup**: the gateway is DELETEd (best effort) when its sandbox is
   removed: on exit cleanup, or on a recreate when the mounts changed. A
   kept sandbox (failed task, `save_sandbox`) keeps its gateway. Each gateway
   is recorded as `<sessions-root>/../gateways/<sandbox>.json`
   (`acp-wrapper --mcp-gateway-state` overrides the directory), so after a
   restart it is either reused (it is still live and has the same servers) or
   replaced. Gateways whose sandboxes have since disappeared are swept on the
   next launch. standalone-codemode keeps gateways in memory, so a reused
   sandbox whose gateway is gone gets a new one, and the sandbox itself is
   not recreated.

Without `--mcp-gateway-url` none of this happens: no `MCP_URL`, no extra
policy rules, and still no static MCPs. If the gateway can't be reached when
a sandbox starts, the agent runs without `MCP_URL`; this is logged in the
acp-wrapper log, or as `mcp_gateway_failed` in the audit log for terminal-tab
sandboxes.

## Walk through a project

### 1. Create an empty project file

```sh
mkdir -p ~/orch/my-feature
touch ~/orch/my-feature/project.yaml
```

The daemon logs `project_empty` and launches the **project agent**. It is
autonomous (an ACP session — press `a` in the TUI to watch it): it reads the
seed `description:`, fills in the rest of `project.yaml` and sets
`status: ready`, or blocks the project (summoning the wolf) when the
description isn't enough to go on.

### 2. Planning

The moment the project file is saved with `status: ready`, the daemon
launches the **planning agent** in the same directory. It writes
`tasks/*.yaml` (one file per task, with `depends_on` edges), then flips
`project.yaml` to `status: working` and exits.

### 3. Tasks

`status: working` triggers the daemon to:

1. Build the task DAG from `tasks/*.yaml`.
2. Pick the first ready task (no uncommitted deps).
3. Provision a wsp workspace at `~/dev/workspaces/<branch>/` with the repos
   from `project.yaml`.
4. Launch a **task agent** in that workspace.
5. When the task agent exits with `status: success`, launch a **commit agent**
   in the same workspace.
6. When the commit agent exits with `status: committed`, pick the next ready
   task. Repeat until everything is committed, then mark the project
   `status: done`.

Task agents retry up to 3 times on `status: failed`. After the 3rd failure
the project goes `status: blocked`, you get an osascript notification, and
a wolf agent launches.

### 4. Stop

`Ctrl-C` the daemon. ACP sessions keep running for the next orch to reconnect
to; sessions in terminal tabs (the `--wolf-host` wolf) are closed, which closes
their tabs.

## Stub mode (no wsp / no repos)

If you want to exercise the orchestration without wsp registered or repos
to clone:

```sh
./orch --root ~/orch --workspace-manager=stub
```

Task and commit agents run in `$TMPDIR/orch-workspaces/<branch>/` — empty
dirs the daemon creates on demand. The project and planning agents are
unaffected (they don't need a wsp workspace).

## Files the daemon writes

Inside each agent's working directory:

```
.orch/
  context.yaml       # paths + branch + task name (whatever applies to this Kind)
  instructions.md    # rendered prompt the agent reads at startup
```

In the project root:

```
tasks/*.yaml         # written by the planning agent; observed in audit
```

Daemon-owned writes carry `updated_by: daemon` in `project.yaml` so the
daemon ignores its own fsnotify events.

## Observability

- **Audit log** (`--audit-log`, default `logs/audit.log` relative to where
  `orch` runs; `~/orch/audit.log` with `./start.sh`): one line per event.
  The TUI doesn't display it — `tail -f` it in a separate terminal.
  `daemon.log`, `channels.log` and `acp-wrapper.log` are written beside it.
- **Terminal tabs**: agents that run on the host rather than under ACP (the wolf
  with `--wolf-host`, every agent without `--acp-kit`) each get a terminal tab
  titled after their project. Enter on the session row in the TUI switches to
  it. See [docs/terminal-tabs.md](docs/terminal-tabs.md).
- **ACP sessions** (`--acp-kit`): planning/task/commit/review/archive agents
  and, by default, the wolf run as ACP sessions. Press `a` in the TUI for the tab view
  (or enter on a session row); the wolf's tab is interactive — press enter to
  type to it. `--wolf-host` runs the wolf on the host in a terminal tab instead
  (full host access, but only reachable from that tab). Each tab keeps a
  scrollback of its session's output (the most recent ~5000 lines; older output
  is dropped, with a marker saying how much). See [ACP view keys](#acp-view-keys).
- **Workingman agent** (`--workingman-agent`, needs `--acp-kit`): an always-on
  assistant in its own sandbox that answers questions about the orch state
  (projects, tasks, the wolf, the logs) for a human on a messaging channel, and,
  when asked (after confirming), changes it by editing the orch roots: stop/start
  a work stream, summon the wolf, retry a task, unblock a project, queue intake
  work, start a new project. On automatically when `channels.yaml` has an
  inbound-capable channel; the snapshot, logs and every session's stream are
  mounted read-only. See [agents.md §8](agents.md#8-workingman-agent).
- **State snapshot** (`--state-file`, `orch status`): the daemon's in-memory
  state (live sessions, wolf in flight, failure counters, review polls)
  published as JSON for external readers. See
  [docs/state-snapshot.md](docs/state-snapshot.md).

## Channels / WhatsApp

The daemon can reach you on WhatsApp, modelled on hermes-agent's WhatsApp
gateway. It messages you the moment a wolf agent starts (`🐺 wolf is running for
<project> … Reply to this message to talk to the wolf.`), relays your reply into
that wolf's conversation, and routes any other message to the always-on
**workingman agent**, an assistant that answers questions about open projects,
running tasks and the daemon state, and makes changes to projects and tasks on
request. Details, configuration reference,
security model, troubleshooting and a manual smoke-test checklist:
[docs/channels.md](docs/channels.md).

```sh
orch whatsapp setup     # Cloud API: credentials, owner allowlist, webhook (prints what to paste into Meta)
orch whatsapp status    # masked config, Graph reachability, webhook listener
orch whatsapp test      # send a test message to the owner
orch whatsapp pair      # bridge mode: link a personal number by QR code
orch --root ~/orch --acp-kit <kit>   # channels.yaml present ⇒ channels on, workingman agent on
```

Two backends, chosen per channel with `options.mode`:

| | `cloud` (default, supported) | `bridge` (unofficial) |
|---|---|---|
| Transport | Meta WhatsApp Cloud API: signed webhook in, Graph API out | Baileys linked device, run as a supervised local Node process |
| Needs | Meta app + phone-number id, public https URL (tunnel) | Node ≥ 18, a phone to scan a QR code; no public URL |
| Number | a Meta (test or business) number | your personal number (`self-chat`) or a spare one (`bot`) |
| Ban risk | none — official API | **real**: Baileys speaks the unofficial WhatsApp Web protocol; WhatsApp can disconnect or ban numbers that use it. Use a number you can afford to lose and keep traffic low |
| Setup | `orch whatsapp setup` | `orch whatsapp pair` |

Parity with hermes-agent's WhatsApp channel (✅ supported · ➖ partly · ❌ not supported):

| Feature | hermes-agent | workingman cloud | workingman bridge |
|---|---|---|---|
| Cloud API backend (webhook + Graph) | ✅ | ✅ | n/a |
| Baileys bridge backend | ✅ | n/a | ✅ (bridge vendored from hermes) |
| Setup wizard / pairing | ✅ | ✅ `orch whatsapp setup` | ✅ `orch whatsapp pair` |
| Bot mode (dedicated number) | ✅ | ✅ | ✅ |
| Self-chat mode (personal number) | ✅ | ❌ (the API never delivers it) | ✅ |
| Allowlist, default deny | ✅ | ✅ (no wildcard; `allow_all` is an explicit opt-in) | ✅ |
| Reply to unauthorised senders | pairing code, or ignore | silent by default; optional `deny_reply` | same |
| Webhook signature + verify handshake | ✅ | ✅ | n/a |
| Group chats | ✅ | ❌ (direct chats only) | ✅ (`groups`, `group_allow_from`, mention gating) |
| Text in / out | ✅ | ✅ | ✅ |
| Markdown → WhatsApp formatting, 4096-char chunking | ✅ | ✅ | ✅ |
| Quoted replies (reply-to) | ✅ | ✅ — also how a wolf message is answered | ✅ |
| Typing indicator | ✅ | ✅ | ✅ |
| Read receipts | ✅ | ❌ not sent | ➖ opt-in (`send_read_receipts`) |
| Images, voice, documents | ✅ | ➖ captions kept, media not downloaded or sent | ➖ same |
| Voice transcription | ✅ | ❌ | ❌ |
| Polls, locations, interactive buttons | ✅ | ❌ (permission prompts are plain text: reply `yes`/`no`) | ❌ |
| Message batching (debounce) | ✅ | ❌ one turn at a time per chat, bounded queue | ❌ |
| Tool-progress messages | ✅ | ❌ only the agent's final text segment is sent | ❌ |
| Template messages outside the 24 h window | n/a | ❌ typed error (`ErrOutsideServiceWindow`), audit-logged | n/a |
| Session survives restarts | ✅ | n/a (stateless) | ✅ (`~/.workingman/whatsapp/session`) |

What is specific to workingman: the **wolf channel** (topic `wolf`: start/end
messages, reply-to routing, `/wolf`, relayed tool-permission requests), the
**workingman agent** (an ACP session in its own sandbox; the orch roots, daemon
snapshot, audit log and agent sessions are mounted read-only, except the roots, which are writable only for creating new `project.md` / intake files), the commands
`/help /status /wolf /agent /who`, and the redaction of every outgoing reply.
The WhatsApp numbers allowed to talk to the daemon are an allowlist — nothing
else reaches an agent.

## Channels / Signal

Signal is the second channel type, with the same wolf messages, reply-to routing,
router commands, allowlist and workingman agent as WhatsApp. It is modelled on
hermes-agent's Signal adapter (`gateway/platforms/signal.py`) and needs one thing
WhatsApp does not: a running [signal-cli](https://github.com/AsamK/signal-cli)
daemon (`signal-cli -a +15551234567 daemon --http 127.0.0.1:8080`) that is
registered or linked to a Signal account. Step-by-step binding of the workingman
agent and the wolf, the configuration reference, security notes,
troubleshooting and a smoke-test checklist: [docs/signal.md](docs/signal.md)
(shared concepts: [docs/channels.md](docs/channels.md)).

```sh
orch signal setup       # signal-cli URL, account, owner allowlist; writes the channel and the wolf/workingman routes
orch signal status      # masked config; signal-cli reachable, account registered (--offline skips the check)
orch signal test        # send a test message to the owner
orch --root ~/orch --acp-kit <kit>   # channels.yaml present ⇒ channels on, workingman agent on
```

Differences from WhatsApp (cloud):

| | WhatsApp (cloud) | Signal |
|---|---|---|
| Needs | Meta app, phone-number id, public https URL (tunnel) | a signal-cli daemon on the host; nothing public |
| Meta console / webhook / tunnel | yes | **none** |
| 24-hour service window | yes (typed error outside it) | **none** |
| Secrets | access token, app secret, verify token | none (the account number may be an env credential) |
| Message to yourself | not delivered | **Note to Self** (`note_to_self: true`) |
| Groups | no | opt-in per group |
| Message length / formatting | 4096 chars, WhatsApp markup | 8000 chars, Markdown → native Signal styles |

Parity with hermes-agent's Signal adapter (✅ supported · ➖ partly · ❌ not supported):

| Feature | hermes-agent | workingman |
|---|---|---|
| signal-cli HTTP daemon: SSE in, JSON-RPC out | ✅ | ✅ |
| Setup wizard | ✅ `hermes gateway setup` | ✅ `orch signal setup` (validates signal-cli and the account) |
| Allowlist, default deny | ✅ | ✅ (no wildcard; `allow_all` is an explicit opt-in) |
| Unknown sender → pairing code | ✅ | ❌ silent denial |
| Group chats | ✅ (`*` allowed) | ➖ only listed groups, and the sender must be allowlisted |
| Note to Self, echo protection | ✅ | ✅ (opt-in) |
| Quoted replies | ✅ | ✅ in direct chats — also how a wolf message is answered |
| Native formatting, 8000-char chunking | ✅ | ✅ |
| Typing indicator | ✅ | ✅ |
| Reconnect with backoff | ✅ | ✅ (2s → 60s) |
| Phone-number redaction in logs | ✅ | ✅ (`***4321`) |
| Attachments, voice, reactions | ✅ | ❌ text only |
| Tool-progress messages | suppressed | ❌ only the agent's final text segment is sent |

The security model matches WhatsApp: only allowlisted numbers reach an agent, and
the signal-cli data directory (account keys, `~/.local/share/signal-cli/` by
default) must be kept private. Both channels can be enabled at once.

## Example `project.yaml`

```yaml
description: |
  Add a /healthz endpoint to the gateway and a matching probe in the
  deploy manifests.

repos:
  - org: docker
    name: gateway
  - org: docker
    name: deploy-manifests

branch: feat/healthz-probe
status: ready
cron: "*/15 * * * *"   # optional; wake the project every 15 minutes
cron_max_runs: 96      # required with cron (or cron_until): stop after 96 cycles
cron_runs: 12          # daemon-owned cycle counter
replan: true           # optional; daemon-owned "re-plan the existing tasks" request
cleanup: true          # optional; "please run the archive agent" (set by :cleanup)
archive: true          # optional; "the cleanup finished" (set by the archive agent)
hide: true             # optional; display only — keep this project out of the TUI gallery
updated_by: agent
```

A `cron:` schedule must come with a stop condition — either
`cron_until: <RFC3339 timestamp>` (an absolute deadline) or
`cron_max_runs: <int>` (a cycle limit). They are two expressions of the same
idea, either one is enough, and if both are set whichever trips first wins. That
stop condition is the *only* thing that ends a schedule: `status: done` does
**not**, because done is the resting state between cycles rather than the end of
a recurring work stream. A project with `cron:` and *no* stop condition is not
scheduled at all: the daemon blocks it and summons the wolf agent, since a
schedule that can never end would wake the project up forever.

`cron_max_runs` counts **work, not wake-ups**. `cron_max_runs: 30` means 30
firings that actually started a cycle, and the 30th does its work before the
schedule is retired — the daemon increments `cron_runs` after the cycle starts
and unregisters (logging `cron_unscheduled`) once the count meets the limit. A
firing that starts no cycle — one landing on a project still `working` through
the previous cycle, or `blocked`, or being archived — is *not* charged; it logs
`cron_run_not_counted` alongside `cron_replan_skipped` and still pokes the
project as a recovery poll.

The tradeoff is deliberate: since skipped firings don't spend the budget, a
project wedged in a non-idle status can keep waking up past `cron_max_runs`.
`cron_until` is the absolute wall-clock bound for that case, and there is no
second, hidden cap on firings — a schedule that has produced no cycles has, by
this definition, not used any of its runs.

### Recurring projects

`cron:` plus a project description written as a standing instruction ("triage new
issues", "refresh the dependency report") gives a work stream that re-plans
itself on every firing:

```
done ──(cron fires)──> ready + replan: true ──> planning agent ──> working ──> done
```

The planning agent gets a different prompt when `replan` is set. Instead of the
default "preserve everything already here" rule — which exists for the
incremental case, where a human adds one task through the UI and expects the rest
untouched — it is told to read the previous cycle's tasks for context and then
decide per task: delete it, reset it to `ready` to run again (dropping the last
run's `summary:`, `commits:`, and `completed_at:`), or leave it `committed` so it
stays settled and still satisfies anything that depends on it.

`replan` is daemon-owned: the daemon sets it on the firing, forwards it to the
agent through `.orch/context.yaml`, and clears it once a planning session has
moved the project off `ready`. Agents should not write it.

See `examples/project.yaml` and `examples/tasks/*.yaml` for the full
schemas.

## ACP view keys

The ACP view (`a` in the TUI) has two scroll modes per tab, shown at the end of
the tab's status line:

- **live** (the default) — the view stays pinned to the newest output as it
  streams in.
- **inspection** — entered as soon as you scroll up off the bottom. The view
  stays on the history you're reading while new output keeps buffering below
  it; the status line reads `inspection — press G to follow`. Scrolling back to
  the bottom with `j`/`ctrl-f`, or pressing `G`, returns to live.

| Key | Effect |
|-----|--------|
| `j` / `k` | scroll down / up one line |
| `ctrl-f` / `ctrl-b` | scroll down / up a page |
| `G` | jump to the newest output and return to live |
| `⌥j` / `⌥k` (or `l` / `h`) | next / previous tab |
| `z` | expand / collapse tool-call output |
| `x` | dismiss a dead session's tab |
| `enter` (or `i`) | type a message to a conversational tab (the wolf) |
| `esc` (or `a`) | back to the main view |
| `q` | quit |

`j`/`k` here are scroll keys, not tab switches — tabs move with `⌥j`/`⌥k`
(as on the main screen) or `h`/`l`. While typing a message, letters are text,
so only `ctrl-f`/`ctrl-b` scroll; `esc` stops typing and gives `j`/`k`/`G` back.
Each tab keeps its own scroll position.

## Hiding work streams

The gallery is only so many cards wide, and a project that is parked — but not
finished enough to clean up and archive — crowds out the ones you're actually
watching. `hide: true` takes a work stream out of the gallery and nothing else:

```yaml
hide: true   # defaults to false
```

It is **display only**. The daemon still dispatches a hidden work stream, its
agents still run, its `cron:` still fires, and it still shows up in the state
snapshot (flagged `hidden`). Nothing about the project's lifecycle changes.

| Command / key | Effect |
|---------------|--------|
| `:hide` | sets `hide: true` on the selected work stream — its card disappears on the gallery's next refresh |
| `:show` | clears `hide:` — the work stream is back in the gallery for good |
| `tp` | toggles hidden work streams on and off **in this TUI session** |

`tp` is how you get at a hidden work stream again: with the toggle on, hidden
cards come back into the gallery marked with a `⊘`, so you can select one and
run `:show` on it. The toggle isn't persisted — it's "let me see everything for
a moment", not a saved preference — and the footer hint tracks which way it's
currently set. A gallery that is empty only because everything in it is hidden
says so (`(none visible — 3 hidden; tp to show)`) rather than looking like the
work streams vanished.

Unlike `:cleanup` and `:review`, `:hide`/`:show` write `project.yaml` as
`updated_by: daemon`. The request flags are written as the agent *so that* the
daemon's fsnotify handler acts on them; hiding must do the opposite — written
as the agent it would be routed like any other project change, which on a
`blocked` work stream would re-summon the wolf just for hiding a card.

`:show` is one of the three commands (with `:stop` and `:start`) that has no
first-letter shortcut in the `:` menu — `session` owns the `s` — so pick it
with `j`/`k` and `enter`.

## Cleanup and archiving

A finished project is retired in two steps — a **cleanup** that leaves every
repo clean, committing and pushing only what needs it, then an **archive**
that moves the project out of
the watched root. Both are driven from the TUI's `:` command menu on the work
streams pane, and both are recorded on `project.yaml` by two independent
boolean flags (neither one is a `status:`, so a project keeps whatever status
it already had):

| Field      | Meaning                                             | Written by |
|------------|-----------------------------------------------------|------------|
| `cleanup:` | *request*: "run the archive agent on this project"  | the TUI's `:cleanup` (as `updated_by: agent`, so the daemon sees the event); cleared by the daemon when the agent's session ends |
| `archive:` | *result*: the cleanup succeeded, safe to archive     | the archive agent, on success only |

### `:cleanup` — the archive agent

`:cleanup` sets `cleanup: true` and returns immediately; the daemon notices the
flag ahead of its normal status routing and launches the **archive agent** in
the project's wsp workspace. It runs autonomously, as a one-shot ACP session in
its own `<work-stream>-archive` sandbox — nothing to attach to; watch it in the
TUI's tab view like any other ACP agent. In every repo of the workspace it:

1. Checks for uncommitted work (`git status --porcelain`).
2. Classifies what's there. Anything that plainly doesn't belong in the repo
   (build artifacts, editor scratch, local caches) is **not** committed and
   **not** deleted, and it never edits `.gitignore` itself. Since nobody is
   there to answer, it doesn't ask: it leaves that repo alone and reports the
   exact `.gitignore` edit it would have made as outstanding work for you.
3. Makes one final commit — only if there is something to commit.
4. Pushes to the branch **actually checked out in that workspace**
   (`git rev-parse --abbrev-ref HEAD`) — not a hardcoded branch name, and never
   a force-push — and only if there is something to push. It first asks whether
   an upstream exists at all (`git rev-parse --abbrev-ref
   --symbolic-full-name @{upstream}`), then counts the distance
   (`git rev-list --count @{upstream}..HEAD`). It pushes when that count is
   above zero, or when there genuinely is no upstream configured. A repo
   already level with its upstream is reported as *"already clean, nothing to
   push"* — no no-op push, and no empty commit or force/`--set-upstream`
   workaround to invent one.

There are no other pre-commit steps: no tests, no lint, no formatting, no
scratch-file cleanup. A repo that needed neither a commit nor a push is a
success. On success the agent sets `archive: true` on
`project.yaml` and leaves `status:` untouched. If it can't finish, it leaves
`archive` unset and says what's outstanding — its final message is the whole
report, so read the session tab when a cleanup doesn't flip the flag.

Because it pushes, the daemon grants its sandbox egress to `github.com` on
dispatch (sbx denies network by default, and the archive agent has no task file
to declare the rule itself), and the acp-wrapper runs the same push preflight it
runs for the commit agent — a forwarded SSH agent holding no usable key fails
the launch loudly rather than mid-push. See `./check-signing.sh`.

The daemon clears `cleanup:` when the session ends either way (an unfinished run
is logged as `archive_incomplete`), so a cleanup is never retried behind your
back — re-run `:cleanup` yourself. Exactly one archive agent runs per request,
and a second `:cleanup` while one is in flight is a no-op. A project that is
already cleaned up (`archive: true`) is refused outright — its next step is
`:archive`, not another cleanup.

### `:archive`

`:archive` only archives a project whose `project.yaml` carries
`archive: true`. Anything else is refused on the status line with "has not been
cleaned up — run :cleanup first", and nothing moves. When the guard passes and
you confirm, the archive:

1. removes the project's **wsp workspace** (`wsp rm <branch>`), then
2. moves the project directory to the sibling backup root, keeping its name
   (`~/orch/my-feature` → `~/orch.backup/my-feature`).

The workspace goes first on purpose: if `wsp rm` fails, the archive aborts with
the project still in place and the failure on the status line. The daemon drops
the project on its next scan — the move is the only cleanup needed.

In the TUI's work-stream gallery, a project with `archive: true` is drawn with a
**blue border** — it's cleaned up and waiting for `:archive`. Selection still
wins over the blue, so the cursor never disappears onto a blue card. See
[Gallery border colours](#gallery-border-colours) for the full precedence.

Doing it by hand instead:

```sh
rm ~/orch/my-feature/project.yaml
rm -rf ~/orch/my-feature/{tasks,.orch}
wsp rm feat/healthz-probe
```

## Gallery border colours

Each card in the TUI's work-streams gallery carries a border colour, so a scan
of the pane tells you what state its project is in:

| Border | Meaning |
|--------|---------|
| **blue** | `archive: true` — cleaned up, waiting for `:archive` |
| **green** | a live `cron:` schedule — the stop condition hasn't tripped, so the project still wakes itself up |
| grey | nothing special |

A project's own border shows one of those at a time, and they win in that order:
**blue archive > green cron > grey**. Blue beats green because the blue border is
what tells you `:archive` will be accepted, and a cleaned-up project is
effectively finished even if a schedule is still registered against it — so a
project that is both loses its green border until it's archived.

The **selected** card — where the cursor is — gets a second pink border drawn
*around* its own, rather than recolouring it:

```
╭────────────────────────────╮   ← pink: this is the selected card
│╭──────────────────────────╮│   ← blue/green/grey: what the project is
││ weather-tui              ││
│╰──────────────────────────╯│
╰────────────────────────────╯
```

That way the durable state colour stays readable while the cursor sits on the
card. Every card reserves the ring's two rows and columns whether or not it is
selected, so moving the cursor never reflows the gallery.

The green border tracks the same stop condition the daemon unschedules on
(`cron_until` / `cron_max_runs` vs. `cron_runs`, see
[Example `project.yaml`](#example-projectyaml)), so it clears itself on the
gallery's next refresh once a schedule expires — no file edit needed.
