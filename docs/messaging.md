# Agent messaging: post-office, the inbox, Slack and Step-Up

Agents can message you while they work. A **notification** needs no reply
("tagged v0.9.0"). A **request** needs an answer before the agent can go on
("which bucket for the export?"). The agent may wait for it, carry on and
check back later, or give up. Requests can also go to another agent: v1 has
one, **Step-Up**, which handles requests for more sandbox access, such as a
network route.

This page covers how to set it up and use it. The design, with the reasons
for each choice, is [messaging-design.md](messaging-design.md). The channel
details are in [channels.md](channels.md), and the agents in
[agents.md](../agents.md).

Contents: [How it fits together](#how-it-fits-together) ·
[Set up post-office](#set-up-post-office) ·
[Give a task the messaging capability](#give-a-task-the-messaging-capability) ·
[What the agent sees](#what-the-agent-sees) · [The TUI inbox](#the-tui-inbox) ·
[Slack and other channels](#slack-and-other-channels) · [Step-Up](#step-up) ·
[Files](#files) · [Troubleshooting](#troubleshooting) ·
[End-to-end smoke test](#end-to-end-smoke-test)

## How it fits together

```
 task agent ──tools/call──► its sandbox's sub-gateway ──► post-office (127.0.0.1:7077/mcp)
                                                              │
                                    ~/.workingman/post-office/{inbox,outbox,done,deliveries}
                                         ▲                  ▲                    ▲
                                    orch TUI           orch daemon          Step-Up agent
                                    (inbox view)       (Dispatcher → Slack,  (next_message,
                                                        Signal, WhatsApp)     reply)
```

- **post-office** is a small MCP server (`cmd/post-office`) that runs on its
  own, not inside orch, so it keeps serving while orch restarts. Agents reach
  it only through their sandbox's standalone-codemode sub-gateway.
- Every message is a **file** under `~/.workingman/post-office/`. A dropped
  connection or a restarted agent never loses a request.
- The **TUI** reads those files directly, so the inbox works with or without
  the daemon and with or without channels.
- When channels are configured, the **daemon** sends each message to every
  channel. **The first answer wins**, from anywhere. The TUI row then goes
  away, and every other channel is told "✅ Answered via …".

## Set up post-office

Once per machine:

```sh
task build                    # builds post-office with orch, acp-wrapper and op-read
post-office setup             # writes the bearer token to ~/.workingman/post-office/token
post-office setup --print     # prints only the token
task post-office:install      # macOS: ~/.local/bin/post-office plus launchd agent com.workingman.post-office
# or in the foreground:
post-office serve             # http://127.0.0.1:7077/mcp
```

Store the token as the standalone-codemode secret
`mcp/post-office/post_office_token`. The gateway's `post-office` catalog entry
(in `mcp-servers.json` and `mcp-servers-personal.json`) sends it as
`Authorization: Bearer ${post_office_token}`. If you rotate the token
(`post-office setup --rotate`), update the secret and restart post-office.
Until both are done, every call gets 401.

`post-office serve` flags: `--listen` (default `127.0.0.1:7077`), `--dir`
(the store, default `$WORKINGMAN_POST_OFFICE_DIR` or
`~/.workingman/post-office`), `--sessions-root`, `--gateways-dir` and
`--max-wait` (the longest one tool call blocks, default 50 s). The launchd
agent logs to `~/Library/Logs/workingman/post-office.log`. Only one copy runs
at a time (`post-office.lock`).

orch itself needs nothing extra. It needs the MCP gateway
(`--mcp-gateway-url`, which `start.sh` sets) so that sandboxes can reach
post-office at all.

## Give a task the messaging capability

An agent can message only if its sandbox's gateway was **created** with
`post-office`. Put it in the task's `static_mcps`:

```yaml
# tasks/fix-ci.yaml
name: fix-ci
description: |
  … If you need a decision from the operator, ask with send_request rather
  than guessing.
static_mcps: [post-office, github]   # replaces the default set, so list everything the task needs
```

- The **planning agent** adds `post-office` on its own when a task
  description says the agent may need to ask the operator or another agent
  something, or keep the operator informed.
- `post-office` is **never** in `--mcp-gateway-servers`, so no agent gets it
  by default.
- An agent on a dynamic gateway cannot give itself the capability with
  `mcp-add`. The catalog entry is `dynamic_addable: false`, and post-office
  checks every call against the servers the gateway was created with. An
  agent that tries gets `post-office is not enabled for this sandbox (add
  static_mcps: [post-office] to the task)`.
- The **sender is never a tool argument**. post-office works it out from the
  calling gateway: the sandbox, then that sandbox's newest live session (kind,
  project and task). Messages show up as `task fix-ci (workingman-messaging)`,
  `step-up` or `workingman agent`.

The Step-Up agent always has `post-office` (and nothing else). No other
daemon-owned agent has it.

## What the agent sees

| Tool | What it does |
|---|---|
| `send_notification(subject, body, recipient?)` | One-way. Returns at once with `status: sent`. |
| `send_request(body, subject?, recipient?, options?, wait_seconds?, expires_in_minutes?)` | Asks a question. Waits up to `wait_seconds` (at most 50) and returns `status: answered` with the `response`, or `status: pending` with a `request_id`. |
| `wait_for_response(request_id, timeout_seconds?)` | Waits up to 50 s more. The agent loops on it for as long as it is willing to wait. |
| `check_response(request_id)` | Checks without waiting, between other work. |
| `cancel_request(request_id, reason?)` | Withdraws the question. It leaves your inbox and the channels say "⌛ No longer needed". |
| `list_requests(status?)` | Recovers a lost `request_id`. |

`recipient` is empty (or `operator`) for you, or `step-up`. Any other name is
rejected, so a typo never creates a message nobody can read. `options` are
suggested answers. You can still answer freely, and an answer matching an
option (by text or by number) is reported as that option. A request expires
after 24 h by default (between 1 minute and 7 days). A notification expires
after 7 days.

An interrupted or timed-out `wait_for_response` never cancels the request.
Only `cancel_request` and expiry do. Every result starts with
`status: <state>`, so the agent can act on it even when code mode flattens
the result.

## The TUI inbox

The inbox is visible in every mode:

- **The badge.** Nothing when the inbox is empty. `✉ 3` (faint) when only
  notifications, or messages to agents, are open. `✉ 2 ?` (bold yellow) when
  requests are waiting for you. It sits at the start of the footer, at the end
  of the ACP header line, and in the command-picker and archive footers.
- **The bell.** The terminal bell rings once for each new request (not for
  the ones already open when the TUI starts). `WORKINGMAN_TUI_BELL=0` turns it
  off.
- **`I`** (shift-i) opens the inbox from any mode where you are not typing
  text. While composing in the ACP view, `I` is just a letter: press `esc`
  first.

```
 Inbox  ✉ 2 ?                                            I/esc close
 ? r1t8w  task fix-ci (workingman-messaging)        12m  slack
   Which bucket for the export?
 · 7hd2k  task release (wm)                          1h
   Release v0.9.0 tagged
 ── To agents (read-only) ───────────────────────────────────────────
 → x81mm  task fix-ci → step-up                      14m  received
   Need network access to proxy.golang.org
 [body of the selected message]
 Options: 1) staging  2) prod
 > _
```

| Key | Action |
|---|---|
| `j` / `k` | next / previous message |
| `enter` | type an answer to the selected request; `enter` sends it, `esc` stops typing (the draft is kept), `ctrl+u` clears it |
| `1`–`9` | answer with that option at once |
| `d` | dismiss the selected notification |
| `ctrl+f` / `ctrl+b` | scroll the body |
| `y` | copy the body to the clipboard |
| `I` / `esc` | close the inbox |

Operator requests come first, then notifications, then the read-only
**To agents** rows (requests to Step-Up, so you can see agent-to-agent
traffic). You cannot answer or dismiss those rows. The right-hand column
shows where a message was sent (`slack, signal`), or `received` once Step-Up
has taken it.

After you answer, the row greys out (`sending…`) until post-office delivers
the answer, and then disappears. A message answered on a channel greys out as
`answered via slack` and disappears the same way. If you were typing a reply
when that happened, typing stops and the reply line says why. If post-office
is not running, the row shows `answered — waiting for post-office` after
10 s. The answer is saved, and is delivered once post-office starts.

Standalone `orch tui` finds the store the way the daemon does
(`--sessions-root`, default `~/.workingman/sessions`, or
`$WORKINGMAN_POST_OFFICE_DIR`). If it cannot open the store, the TUI runs
without the inbox and says why.

## Slack and other channels

With no channels configured, the TUI is the only place to answer. With
channels configured (`~/.workingman/channels.yaml`), the daemon's
**Dispatcher** also sends every operator request and notification on topic
`post-office`: to the `post-office` routes, or to the `*` routes when there
is none. A `post-office` route with `notify: false` gets requests but not
notifications. Messages to Step-Up are never sent to channels.

```
❓ [req r1t8w] task fix-ci (workingman-messaging)
Which bucket for the export?

…body…

Options: 1) staging  2) prod
Reply to this message, or send #r1t8w <answer>. Expires in 24h.
```

**Answering from a channel:** reply to the message (on Slack, in its
thread), or send `#r1t8w staging` from anywhere in the chat. The router
answers `✅ Sent to <sender>`, or `Already answered via tui.` if someone was
first. `/inbox` lists the open requests with their short ids.

**Answered elsewhere:** every channel that did not answer gets a closing
note on its copy: `✅ Answered via <tui|slack|…>: <start of the answer>`,
`⌛ No longer needed — <reason>`, or `⌛ Expired — the agent stopped
waiting`. On Slack this is a thread reply plus a reaction on the request
(✅, ⌛ or 🚫), because the Slack MCP cannot edit messages. A later reply in a
closed thread gets `This request is already closed (answered via tui).`

**Slack** runs through the Slack MCP behind the standalone-codemode gateway
(work setup only), in your **DM-to-self** by default:

```sh
orch slack setup     # pick the conversation (Enter = DM-to-self); writes channels.slack and the post-office route
orch slack status    # config, gateway, watch list
orch slack test      # round trip: post, your thread reply, ✅
```

Then restart the daemon so it picks up the channel. Two things to know:

- **Posts are made as you.** Slack probably does not push-notify you about
  them, so treat Slack as a place to answer, not an alert. The TUI bell,
  Signal or WhatsApp still alert you.
- **Replies are polled**, every 20 s by default (`poll_interval`). An answer
  in Slack can take that long to reach the agent.

Everything else (Signal, WhatsApp, routes, the router's commands, security)
is in [channels.md](channels.md): [Slack](channels.md#slack) and
[Post-office messages](channels.md#post-office-messages).

## Step-Up

Step-Up is an always-on agent that other agents ask for more than their
sandbox allows. In v1 that means network routes and filesystem paths. It
judges each request, asks **you** to approve every allow, and asks the
daemon to apply the change. It never changes a sandbox itself.

### Turning it on

```sh
orch --root ~/orch --acp-kit <kit> --mcp-gateway-url http://localhost:3333   # --step-up-agent=auto is the default
```

`--step-up-agent=auto` (the default) starts it when `--acp-kit` is set and
post-office answers on `127.0.0.1:7077` **when orch starts**. So start
post-office first. If you start it later, restart orch, or pass
`--step-up-agent` (`=on`) to force Step-Up on. `--step-up-agent=off`
disables it. It also needs `--mcp-gateway-url`. Without it, the daemon logs
`step_up_agent_unavailable` and skips Step-Up. It runs in the sandbox
`step-up` and shows up in the TUI as the `step-up` session. You can watch it
there, but nobody needs to type into it: the daemon prompts it whenever a
message arrives for it.

### What a request looks like

From the requesting agent's side (its task needs `static_mcps: [post-office]`):

```
send_request(recipient: "step-up",
             subject: "Need network access to proxy.golang.org",
             body: "go mod download fails with 403 … I need https://proxy.golang.org for this task.")
→ status: pending, request_id: msg_A
wait_for_response(msg_A) … → status: answered
                             response: Granted by the operator: network allow proxy.golang.org for your sandbox. Retry now.
```

### Approving

For an allow it thinks is reasonable, Step-Up sends you a request with the
options **approve** and **deny**. It appears in your inbox and on your
channels. Step-Up writes the wording; it looks something like this:

```
❓ [req q09ab] step-up
Approve network access to proxy.golang.org for task fix-ci?

Task fix-ci (workingman-messaging) asks for network access to
proxy.golang.org: "go mod download fails without it".
CHANGE: sandbox_network_allow proxy.golang.org → wm-workingman-messaging-fix-ci
Answer "approve" to allow it, or "deny".

Options: 1) approve  2) deny
```

The `CHANGE:` line is what you approve. The daemon recomputes it from the
change Step-Up asks for and refuses the change if the approval does not
contain that exact line. So Step-Up cannot get one change approved and apply
another. Only the **approve** option counts. "ok" or "yes" is passed to
Step-Up as free text, and it asks again. One approval covers one change,
once, within 24 h. `deny` (or any refusal) goes back to the agent with your
words.

When the change is applied:

- the live sandbox gets the rule (`sbx policy allow network --sandbox <s>
  <host>`);
- the rule is added to the task's `policies:` in its task file, so a new or
  reused sandbox keeps it;
- you get a notification: `🔓 Step-Up allowed proxy.golang.org for task
  fix-ci (workingman-messaging) (approved via slack)`. Its body gives the
  `sbx` command that undoes the change;
- Step-Up replies to the agent, which retries.

### What Step-Up can and cannot do

| Op | Needs your approval |
|---|---|
| `sandbox_network_allow` — a host, `*.domain.tld`, optional `:port` | yes, always |
| `sandbox_filesystem_allow` — an absolute path glob | yes, always |
| `sandbox_network_revoke`, `sandbox_filesystem_revoke` — only rules Step-Up added | no (they narrow access) |
| `list_sandbox_policy` | no (read-only) |

Nothing else: no task retries, no deny rules, no project changes. Whatever
the prompt says, the daemon enforces these limits:

- **Only the sandbox of the agent that asked.** The daemon takes the target
  from the original request's sender. That sender must be a live task or
  commit session still running the task that asked, and the request must be
  open, or closed less than an hour ago. Step-Up, the workingman agent,
  sleepy-head and daemon gateways are never targets.
- **Unsafe resources are refused:** bare `*` or `**`, top-level domains, IP
  addresses, `localhost`, `*.docker.internal`, local TLDs; `/`, your home
  directory, `~/.ssh`, `~/.aws`, `~/.config/gh`, `~/.gnupg`, `~/.docker`,
  `~/.kube`, `~/.netrc`, `~/.workingman`, `/etc`, the orch roots, and
  anything on your deny list.
- **Rate limits:** at most 10 applied changes an hour, and 3 per sandbox.
- **Kill switch:** while `~/.workingman/step-up/disabled` exists (any
  content), every change is refused. `--step-up-agent=off` stops the agent.
- **Everything is audited**, including refusals, in
  `~/.workingman/step-up/audit.jsonl` (the request, the target, the approval,
  the exact `sbx` command and its output) and as `step_up_op` in the daemon
  audit log.

### Configuration

`~/.workingman/step-up/config.yaml` is optional, and only the daemon reads
it (it is not mounted into Step-Up's sandbox):

```yaml
deny:
  network:
    - "*.internal.example.com"
  filesystem:
    - "/Users/*/Library/Keychains/**"
limits:
  per_hour: 10               # applied allows and revokes, all sandboxes
  per_sandbox_per_hour: 3
```

The file is strict. An unknown key, a negative limit or an empty pattern
makes it invalid, and **while it is invalid every change is refused** (the
result says why). There is no auto-approve list: every allow asks you.

### Undoing a change

Ask Step-Up to revoke it (from any agent with post-office), or run the `sbx
policy rm …` command from the notification and delete the rule from the
task's `policies:` yourself.

## Files

| Path | What |
|---|---|
| `~/.workingman/post-office/inbox/` | open messages, one JSON file each (`msg_<ULID>.json`) |
| `~/.workingman/post-office/outbox/` | answers that post-office has not delivered yet |
| `~/.workingman/post-office/done/<day>/` | closed messages with their answers, kept 30 days |
| `~/.workingman/post-office/deliveries/` | what the Dispatcher sent where |
| `~/.workingman/post-office/token` | post-office's bearer token |
| `~/.workingman/slack/{sent,watch}.json` | the Slack channel's state |
| `~/.workingman/step-up/requests/`, `results/` | Step-Up's change requests and the daemon's results |
| `~/.workingman/step-up/audit.jsonl`, `config.yaml`, `disabled` | audit, configuration and kill switch |

`$WORKINGMAN_POST_OFFICE_DIR` moves the post-office store (for tests). Set
it the same way for post-office, orch and `orch tui`.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| The agent has no `send_request` tool | Its task has no `static_mcps: [post-office]`, or the gateway was created before you added it. A sandbox's gateway is created with the sandbox, so recreate the sandbox. |
| `post-office is not enabled for this sandbox` | Same: the gateway was created without `post-office`. `mcp-add` cannot fix it. |
| Every call fails with 401 | The gateway's `mcp/post-office/post_office_token` secret does not match `~/.workingman/post-office/token`. |
| `no live session for sandbox …` | post-office could not match the sandbox to a running session (for example `~/.workingman/sessions` is not where orch keeps it; see `--sessions-root`). |
| The inbox shows `answered — waiting for post-office` | post-office is not running. Start it; the answer is delivered then. |
| Nothing arrives on Slack | `orch slack status`. Is there a `post-office` (or `*`) route? Did the daemon restart after `orch slack setup`? Is the gateway's `slack` server serving tools? |
| Slack answers take a while | Replies are polled every `poll_interval` (20 s). |
| Step-Up never starts | post-office was not reachable when orch started (`--step-up-agent=auto`), or orch has no `--mcp-gateway-url`. See `step_up_agent_flag` and `step_up_agent_unavailable` in the audit log. |
| Step-Up says a change was rejected | `~/.workingman/step-up/results/` and `audit.jsonl` give the reason: approval missing or mismatched, rate limit, deny list, invalid `config.yaml`, or the `disabled` file. |

## End-to-end smoke test

A manual test of the whole path, on the work setup with the real Slack MCP.
One task asks you two questions and then asks Step-Up for a network route.
You answer the first in the TUI, the second in Slack, and approve the route.
It takes about 15 minutes. Run it after changing any part of messaging, and
record the date and result at the end of this section.

**The first live run must also confirm the `sbx` syntax** the executor uses
(`policy allow network --sandbox <s> <host>`, `policy rm …`, `policy ls
--sandbox <s>`, design §8.3). It was not checked against a real `sbx` when the
executor was built. If it is wrong, the fix is confined to
`policy.Rule.CLIArgs`, `RemoveCLIArgs` and `ListCLIArgs`.

### Before you start

1. [ ] post-office is set up and running: `post-office setup`, the token is
   stored in the gateway, and `post-office serve` (or the launchd agent) is
   up.
2. [ ] `curl http://localhost:3333/healthz` returns 200. The gateway catalog
   has `post-office` and `slack`.
3. [ ] Slack is configured: `orch slack setup`, then `orch slack test`
   completes the round trip ([channels.md § Slack smoke
   test](channels.md#slack-smoke-test) covers the channel on its own).
4. [ ] Pick a host that task sandboxes **cannot** reach by default. This
   test uses `httpbin.org`. The task records the status code it gets before
   and after Step-Up (`before:` should be `403`, blocked by the sandbox
   proxy). If your default policy already allows it, use another host
   everywhere below.
5. [ ] Start orch with the gateway and the kit, after post-office:
   `./start.sh`, or `orch --root ~/orch --acp-kit <kit> --mcp-gateway-url
   http://localhost:3333 --workspace-manager=stub`. The audit log shows
   `step_up_agent_flag` with Step-Up on, and the TUI shows the `step-up`
   session.

### The project

Create `~/orch/messaging-smoke/project.yaml`:

```yaml
description: Manual smoke test of agent messaging (docs/messaging.md).
branch: messaging-smoke
status: working
updated_by: agent
```

and `~/orch/messaging-smoke/tasks/ask.yaml`:

```yaml
name: ask
description: |
  This is a manual test of the post-office messaging tools. Do exactly this,
  in order, and write each result to answers.txt in your working directory:

  1. send_request to the operator: subject "Smoke test: question 1",
     body "Answer from the TUI.", options ["red", "blue"]. Loop on
     wait_for_response until it is answered. Write "q1: <option> <response>".
  2. send_request to the operator: subject "Smoke test: question 2",
     body "Answer from Slack, in the thread." Loop on wait_for_response
     until it is answered. Write "q2: <via> <response>".
  3. Run: curl -sS -o /dev/null -w '%{http_code}' https://httpbin.org/get
     and write "before: <code>".
  4. send_request with recipient "step-up": subject "Need network access to
     httpbin.org", body "This smoke-test task must reach https://httpbin.org
     (the curl in step 3 was blocked)." Loop on wait_for_response until it
     is answered. Write "step-up: <response>".
  5. Run the same curl again and write "after: <code>".
  6. send_notification to the operator: subject "Smoke test done", body the
     contents of answers.txt.
  Then set this task to status: success.
depends_on: []
static_mcps: [post-office]
status: ready
attempts: 0
model: default
```

(With a real workspace manager, give the project `repos:` as usual. Stub mode
needs none.)

### Question 1: answered in the TUI

6. [ ] Within a minute the TUI badge shows `✉ 1 ?` and the bell rings.
7. [ ] Slack's DM-to-self gets `❓ [req …] task ask (messaging-smoke)` /
   `Smoke test: question 1` with `Options: 1) red  2) blue`.
   `orch slack status` lists it as an open watched request.
8. [ ] Press `I`. The request is the first row, with `slack` in its right
   column. The body and options show below it.
9. [ ] Press `2`. The row greys out (`sending…`), then disappears within a
   few seconds. The badge goes away.
10. [ ] In Slack, the request's thread gets `✅ [req …] Answered via tui:
    blue`, and the request gets a ✅ reaction (**answered elsewhere**).
11. [ ] Reply `red` in that thread. Within a poll interval you get `This
    request is already closed (answered via tui).`, and the agent does not
    get it.

### Question 2: answered in Slack

12. [ ] The second request appears in the TUI (`✉ 1 ?`) and in Slack.
13. [ ] Leave the TUI on the inbox (`I`) and reply `from slack` **in the
    request's Slack thread**.
14. [ ] Within about 20 s the thread gets `✅ Sent to task ask
    (messaging-smoke)`. In the TUI, the row greys out as `answered via
    slack`, then **disappears**, and the badge goes away.
15. [ ] No `Answered via` note is posted in that Slack thread: the channel
    that answered gets none. Any other channel you have configured (Signal,
    WhatsApp) gets `✅ [req …] Answered via slack: from slack`.

### Step-Up: a network route that needs approval

16. [ ] The TUI inbox shows a **To agents** row: `task ask → step-up`, `Need
    network access to httpbin.org`, turning to `received` once Step-Up takes
    it. Pressing `enter` or `d` on it says it is read-only. Nothing about it
    is posted to Slack.
17. [ ] Step-Up asks you: a request from `step-up` with options `approve` /
    `deny`, in the TUI and in Slack, whose body has the line
    `CHANGE: sandbox_network_allow httpbin.org → <the task's sandbox>`.
18. [ ] Answer `1` (approve) in the TUI, or `#<short id> approve` / a thread
    reply `approve` in Slack. The other place gets the answered-elsewhere
    note.
19. [ ] Within a minute or so:
    - [ ] you get the notification `🔓 Step-Up allowed httpbin.org for task
      ask (messaging-smoke) (approved via tui)` (or `via slack`);
    - [ ] `sbx policy ls --sandbox <the task's sandbox>` on the host lists
      the allow rule for `httpbin.org` (**this confirms the `sbx` syntax**);
    - [ ] `tasks/ask.yaml` has gained `policies: [{action: allow, type:
      network, resource: httpbin.org}]`, and nothing else in it changed;
    - [ ] `~/.workingman/step-up/results/` has the op's result with
      `status: applied`, and `~/.workingman/step-up/audit.jsonl` has a line
      for it with the `sbx` command and output.
20. [ ] The task's `answers.txt` (or the notification `Smoke test done`)
    reads `q1: blue blue`, `q2: slack from slack`, `before: 403`, a
    `step-up:` line saying access was granted, and `after: 200`. The task
    ends `status: success`.

### Optional checks

21. [ ] **Refusal path.** Ask again, answer `deny`. The agent's
    `wait_for_response` returns Step-Up's reply that the operator declined,
    and no op is written.
22. [ ] **Kill switch.** `touch ~/.workingman/step-up/disabled`, then
    approve another allow. The result is `rejected` (`the Step-Up executor is
    disabled`). `rm` the file afterwards.
23. [ ] **Revoke.** Ask Step-Up to revoke `httpbin.org`. No approval is
    needed. The rule leaves `sbx policy ls` and `policies:`, and you get a
    `🔒` notification.
24. [ ] **Cancel.** Have an agent `cancel_request` an open request. The TUI
    row disappears, and Slack gets `⌛ No longer needed — <reason>` with a 🚫
    reaction.
25. [ ] **No channels.** Move `~/.workingman/channels.yaml` aside and restart
    orch. A request appears only in the TUI and is answered there as in steps
    6–9.

### Cleanup

Set the project to `status: done` (or `:cleanup` it), and remove the
`httpbin.org` rule if you did not revoke it: `sbx policy rm network --sandbox
<sandbox> --resource httpbin.org`.

### Runs

| Date | Result | Notes |
|---|---|---|
| — | not yet run live | Written 2026-11-20 in a sandbox with no `sbx` and no access to the host's TUI or Slack. The steps follow the code as built. The first live run must confirm the `sbx` syntax (step 19). |
