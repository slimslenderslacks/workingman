# Setup

Three scripts drive the local setup:

* [start.sh](#startsh) — check prerequisites (binaries, ssh agent, sbx daemon, signing) and start the orch daemon.
* [sbx-create.sh](#sbx-createsh) — create an MCP gateway, configure secrets and network policy, then create and run a sandbox.
* [sbx-rm.sh](#sbx-rmsh) — delete the sandbox's gateway and remove the sandbox.

## start.sh

| Variable | Description / default |
| --- | --- |
| CHECK_AGENT | disable the ssh agent probe check |
| AGENT_DIR | ssh agent.sock location |
| AGENT_PID | ssh agent pid |
| ORCH_PID | orch daemon pid |
| ORCH_SSH_KEY_REF | where do we get our signing key |
| SSH_AUTH_SOCK | |
| ACP_KIT | $HOME/dev/repos/slimslenderslacks/acp-kit |
| ACP_IMAGE | slimslenderslacks/claude-code-acp:0.90.0 |
| WORKINGMAN_MCP_GATEWAY_URL | http://localhost:3333 |
| WORKINGMAN_MCP_GATEWAY_SERVERS | github |

### checks

* orch and acp-wrapper binaries are built and up to date
* make sure we have an ssh agent.sock on the host for commit signing
* make sure we have an sbx binary on the path and a running daemon
* the check-signing.sh script probes that we can sign something from within a sandbox (and that github will accept it), and also that we can reach and auth github.com

### setup deps

* what signing key should be added to an ssh agent?
    * we use /tmp/orch-ssh.XXXXXX/agent.sock
    * how do we load the private key into this agent? 1password?

### orch binary

| Flag | Value |
| --- | --- |
| --root | ~/orch |
| --audit-log | ~/orch/audit.log |
| --acp-kit | ACP_KIT |
| --acp-image | ACP_IMAGE |
| --acp-wrapper | $HERE/acp-wrapper |

### acp-wrapper binary

acp-wrapper runs one sandboxed Claude Code agent and exposes it over ACP. It creates the sbx sandbox (with acp-kit layered on), mints a per-sandbox MCP gateway, runs acp-kit's `claude-acp-client` inside the sandbox, and serves `<sessions-root>/<session-id>/agent.sock`, fanning the agent's ACP stdio out to every connected client. When the agent exits it removes the sandbox and deletes the gateway.

The orch daemon never talks to a sandbox directly: for each agent session (planning, task, commit, the wolf, the workingman agent) it writes a `starting` session.json and execs a fresh acp-wrapper with the args below (`Runner.acpWrapperCommand`). One-shot stages get `--exit-when-empty` so the wrapper exits when the TUI disconnects and the next stage dispatches; conversational sessions get `--persistent`. The TUI and the daemon's channel router then attach to `agent.sock`.

| Flag | Description |
| --- | --- |
| --session-id | unique session id; names the session dir and (by default) the sandbox (required) |
| --sessions-root | root dir for per-session dirs (default ~/.workingman/sessions) |
| --sandbox | sbx sandbox name (default acp-&lt;session-id&gt;) |
| --kit | acp-kit dir or published ref layered onto the sandbox (required) |
| --template | prebuilt image for `sbx create -t` (orch passes ACP_IMAGE) |
| --sbx | path to the sbx binary (default: sbx on PATH) |
| --workspace | host path to mount; first is the agent cwd and must be writable; `:ro` suffix for read-only (repeatable) |
| --exit-when-empty | exit once the last client disconnects (one-shot stages) |
| --persistent | keep running across client disconnects (wolf, workingman agent) |
| --unblock-grace | with --persistent: end once the project has been unblocked this long |
| --idle-timeout | with --persistent: end after this long with no ACP traffic |
| --task-path | task YAML; final status decides whether to keep the sandbox |
| --project-path | project.yaml, recorded in session.json for daemon reconciliation |
| --kind | agent kind, recorded in session.json |
| --push | preflight that the forwarded ssh agent can push to the git host |
| --save-sandbox | keep the sandbox when the agent exits |
| --model | model alias or id, set as ANTHROPIC_MODEL in the sandbox |
| --mcp-gateway-url | host MCP gateway base URL; with --mcp-server, mints a per-sandbox gateway and sets MCP_URL |
| --mcp-server | catalog server to request from the gateway (repeatable) |
| --mcp-dynamic | dynamic gateway that also serves mcp-find / mcp-add |
| --mcp-gateway-state | dir recording each sandbox's gateway (default &lt;sessions-root&gt;/../gateways) |
| --policy | sbx policy rule `allow:network:<resource>` style, applied after create (repeatable) |
| --static-mcp | deprecated, ignored |

## sbx-create.sh

This is an interactive version of what acp-wrapper does per session: same gateway, policy and env setup, but `sbx run` attaches the sandbox to your pty instead of serving it over an ACP socket.

The TUI's `:session` command follows the same protocol (`runner.ensureSessionSandbox`) for the `session-<work-stream>` sandbox rooted at the work stream's workspace, except that its gateway is dynamic with no servers preselected (`{"servers": [], "dynamic": true}`).

Orchestrate three things:

* curl the $GATEWAY_URL to create a gateway
   * trap all failures with a gateway delete
* `sbx secret ls ...` and `sbx policy allow network ...`
* `sbx create ...`
* `sbx run ...`

sbx `create` and `run` start with this environment

| Variable | Description |
| --- | --- |
| MCP_URL | URL of this sandbox's MCP gateway (localhost rewritten to host.docker.internal) |
| ANTHROPIC_DEFAULT_OPUS_MODEL | model Claude Code uses for the opus alias (`claude-opus-5-5[1m]`) |
| ANTHROPIC_DEFAULT_SONNET_MODEL | model Claude Code uses for the sonnet alias (`claude-sonnet-5-5[1m]`) |
| ANTHROPIC_DEFAULT_HAIKU_MODEL | model Claude Code uses for the haiku alias (`claude-haiku-5-5`) |
| CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC | turn off non-essential network traffic (telemetry, error reporting, auto-updates) |
| CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS | stop sending experimental beta headers to the API |

### checks

* do we have a custom_secret set for the llm gateway?
* explicitly allow host.docker.interal:3333 and localhost:3333 for this sandbox

## sbx-rm.sh

delete the gateway created for this sandbox and then `sbx rm`
