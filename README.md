# orch — autonomous claude-code project daemon

`orch` watches one or more directories for `.project.yaml` files and runs
claude-code agents (project, planning, task, commit, wolf, archive) through a
state machine until the project is done — or blocked, in which case you get a
macOS notification and a wolf-agent session to drop into.

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
`cron_max_runs:`) — see [Example `.project.yaml`](#example-projectyaml).
`cron_max_runs` counts cycles, not wake-ups: a firing that starts no cycle isn't
charged against it.

## Prerequisites

- Go 1.22+
- `tmux` — every agent runs in a detached tmux session you can attach to
- `claude` (claude-code CLI) — the actual agent worker
- `wsp` — multi-repo workspace manager. Repos must be registered (`wsp registry add ...`) before the planning step finishes. If you don't have wsp set up, run with `--workspace-manager=stub` instead.
- macOS for `osascript` notifications (optional; wolf still launches without them)

## SSH agent: commit signing and push (private ssh-agent)

`./start-orch.sh` starts a **private `ssh-agent`** for the daemon, loads the
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
  an already-unlocked CLI session); otherwise start-orch fails at startup.
- The agent's lifetime is tied to `start-orch.sh`: it is killed (and its
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
this tree recursively; any subdirectory with a `.project.yaml` becomes a
project.

```sh
mkdir -p ~/orch
./orch --root ~/orch --audit-log ~/orch/audit.log
```

Repeat `--root` for multiple trees. Watch the audit log in another shell:

```sh
tail -f ~/orch/audit.log
```

## Walk through a project

### 1. Create an empty project file

```sh
mkdir -p ~/orch/my-feature
touch ~/orch/my-feature/.project.yaml
```

The daemon logs `project_empty` and launches the **project agent** in a
tmux session. Attach to it:

```sh
tmux list-sessions          # find the session name (orch-project-<hash>)
tmux attach -t orch-project-<hash>
```

The agent reads `.orch/instructions.md` and interviews you about the
project. When it's done it writes the `.project.yaml` itself with
`status: ready`. Detach (`Ctrl-b d`) any time — the session keeps running.

### 2. Planning

The moment the project file is saved with `status: ready`, the daemon
launches the **planning agent** in the same directory. It writes
`tasks/*.yaml` (one file per task, with `depends_on` edges), then flips
`.project.yaml` to `status: working` and exits.

### 3. Tasks

`status: working` triggers the daemon to:

1. Build the task DAG from `tasks/*.yaml`.
2. Pick the first ready task (no uncommitted deps).
3. Provision a wsp workspace at `~/dev/workspaces/<branch>/` with the repos
   from `.project.yaml`.
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

`Ctrl-C` the daemon. In-flight sessions are closed; tmux sessions go away.

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

Daemon-owned writes carry `updated_by: daemon` in `.project.yaml` so the
daemon ignores its own fsnotify events.

## Observability

- **Audit log** (`--audit-log`): one line per event. `tail -f` it.
- **Tmux sessions**: `tmux list-sessions` shows the agents that still run in
  tmux (archive, and the wolf with `--wolf-host` or without `--acp-kit`).
  Attach with `tmux attach -t <name>` to drive one.
- **ACP sessions** (`--acp-kit`): planning/task/commit/review agents and, by
  default, the wolf run as ACP sessions. Press `a` in the TUI for the tab view
  (or enter on a session row); the wolf's tab is interactive — press enter to
  type to it. `--wolf-host` runs the wolf on the host in tmux instead (full
  host access, but only reachable via tmux).
- **Workingman agent** (`--workingman-agent`, needs `--acp-kit`): an always-on,
  mostly read-only assistant in its own sandbox that answers questions about the
  orch state (projects, tasks, the wolf, the audit log) for a human on a
  messaging channel. On automatically when `channels.yaml` has an
  inbound-capable channel; it can read the roots, snapshot, audit log and every
  session's stream, and change nothing — except that, after confirming with the
  human, it may create new `<root>/<project>/project.md` and
  `<root>/<project>/intake/*.md` files (never overwriting or editing). See [agents.md §8](agents.md#8-workingman-agent).
- **State snapshot** (`--state-file`, `orch status`): the daemon's in-memory
  state (live sessions, wolf in flight, failure counters, review polls)
  published as JSON for external readers. See
  [docs/state-snapshot.md](docs/state-snapshot.md).

## Channels / WhatsApp

The daemon can reach you on WhatsApp, modelled on hermes-agent's WhatsApp
gateway. It messages you the moment a wolf agent starts (`🐺 wolf is running for
<project> … Reply to this message to talk to the wolf.`), relays your reply into
that wolf's conversation, and routes any other message to the always-on
**workingman agent**, a mostly read-only assistant that answers questions about
open projects, running tasks and the daemon state, and can create new
`project.md` / intake files on request. Details, configuration reference,
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

## Example `.project.yaml`

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

See `examples/.project.yaml` and `examples/tasks/*.yaml` for the full
schemas.

## Cleanup and archiving

A finished project is retired in two steps — a **cleanup** that leaves every
repo clean, committing and pushing only what needs it, then an **archive**
that moves the project out of
the watched root. Both are driven from the TUI's `:` command menu on the work
streams pane, and both are recorded on `.project.yaml` by two independent
boolean flags (neither one is a `status:`, so a project keeps whatever status
it already had):

| Field      | Meaning                                             | Written by |
|------------|-----------------------------------------------------|------------|
| `cleanup:` | *request*: "run the archive agent on this project"  | the TUI's `:cleanup` (as `updated_by: agent`, so the daemon sees the event); cleared by the daemon when the agent's session ends |
| `archive:` | *result*: the cleanup succeeded, safe to archive     | the archive agent, on success only |

### `:cleanup` — the archive agent

`:cleanup` sets `cleanup: true` and returns immediately; the daemon notices the
flag ahead of its normal status routing and launches the **archive agent** in
the project's wsp workspace. It's an interactive agent, so it may need you to
attach. In every repo of the workspace it:

1. Checks for uncommitted work (`git status --porcelain`).
2. Classifies what's there. Anything that plainly doesn't belong in the repo
   (build artifacts, editor scratch, local caches) is **not** committed and
   **not** deleted — the agent *proposes* a `.gitignore` edit, notifies you, and
   waits for approval before applying it. If approval never comes, it stops and
   reports instead of committing.
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
`.project.yaml` and leaves `status:` untouched. If it can't finish, it leaves
`archive` unset and says what's outstanding.

The daemon clears `cleanup:` when the session ends either way (an unfinished run
is logged as `archive_incomplete`), so a cleanup is never retried behind your
back — re-run `:cleanup` yourself. Exactly one archive agent runs per request,
and a second `:cleanup` while one is in flight is a no-op. A project that is
already cleaned up (`archive: true`) is refused outright — its next step is
`:archive`, not another cleanup.

### `:archive`

`:archive` only archives a project whose `.project.yaml` carries
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
rm ~/orch/my-feature/.project.yaml
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
[Example `.project.yaml`](#example-projectyaml)), so it clears itself on the
gallery's next refresh once a schedule expires — no file edit needed.
