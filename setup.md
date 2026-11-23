# Setup

Three scripts drive the local setup:

* [start.sh](#startsh) — check prerequisites (binaries, ssh agent, sbx daemon, signing) and start the orch daemon.
* [sbx-create.sh](#sbx-createsh) — create an MCP gateway, configure secrets and network policy, then create and run a sandbox.
* [sbx-rm.sh](#sbx-rmsh) — delete the sandbox's gateway and remove the sandbox.

On a laptop whose VPN inspects TLS, also see [Custom CA (VPN / TLS inspection)](#custom-ca-vpn--tls-inspection) and `check-ca.sh`.

## start.sh

| Variable | Description / default |
| --- | --- |
| CHECK_AGENT | disable the ssh agent probe check |
| AGENT_DIR | ssh agent.sock location |
| AGENT_PID | ssh agent pid |
| ORCH_PID | orch daemon pid |
| ORCH_SSH_KEY_REF | op:// reference to the openssh-format signing key; default `op://workingman-okta/SSH github/private key?ssh-format=openssh` (must be in a vault the service account can read — never Personal) |
| ORCH_OP_READ | op-read binary that resolves ORCH_SSH_KEY_REF; default `$HERE/op-read` |
| OP_SERVICE_ACCOUNT_TOKEN | 1Password service account token; when unset, read from the macOS keychain |
| OP_SA_KEYCHAIN_SERVICE / OP_SA_KEYCHAIN_ACCOUNT / OP_SA_KEYCHAIN | keychain item holding the token; default service `workingman:service_account`, account `jim.clark`, default keychain search list |
| SSH_AUTH_SOCK | |
| ACP_KIT | $HOME/dev/repos/slimslenderslacks/acp-kit |
| ACP_IMAGE | slimslenderslacks/claude-code-acp:0.90.0 |
| WORKINGMAN_MCP_GATEWAY_URL | http://localhost:3333 |
| WORKINGMAN_MCP_GATEWAY_SERVERS | github |
| WORKINGMAN_EXTRA_CA_CERTS | unset (off). PEM bundle of extra CAs every sandbox must trust, for a laptop whose VPN does TLS inspection; read by orch itself (`--extra-ca-certs`), so start.sh passes no flag and only echoes it. See [docs/sandbox-custom-ca.md](docs/sandbox-custom-ca.md) |

### checks

* orch, acp-wrapper and op-read binaries are built and up to date
* make sure we have an ssh agent.sock on the host for commit signing
* make sure we have an sbx binary on the path and a running daemon
* the check-signing.sh script probes that we can sign something from within a sandbox (and that github will accept it), and also that we can reach and auth github.com

### setup deps

* what signing key should be added to an ssh agent?
    * we use /tmp/orch-ssh.XXXXXX/agent.sock
    * the private key is loaded by `op-read "$ORCH_SSH_KEY_REF" | ssh-add -`: op-read resolves the op:// reference with the 1Password Go SDK as a service account (no `op` CLI, no signin)
    * the service account token is `OP_SERVICE_ACCOUNT_TOKEN`, else the macOS keychain; seed it with `security add-generic-password -U -a jim.clark -s workingman:service_account -w`
    * the key item must be in a shared vault granted to the service account (service accounts cannot read Personal)

### orch binary

| Flag | Value |
| --- | --- |
| --root | ~/orch |
| --audit-log | ~/orch/audit.log |
| --acp-kit | ACP_KIT |
| --acp-image | ACP_IMAGE |
| --acp-wrapper | $HERE/acp-wrapper |
| --extra-ca-certs | not passed; defaults to `$WORKINGMAN_EXTRA_CA_CERTS` (empty = off). Validated at startup: orch exits if the file is unreadable or holds no PEM certificate, and warns (subject + notAfter) about expired ones. Under launchd, pass it as a daemon flag: `orch service install --root ~/orch -- --extra-ca-certs ~/.workingman/extra-ca.pem` |

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
| --extra-ca-certs | PEM bundle of extra CAs the sandbox must trust; re-validated on every launch (orch passes it only when `--extra-ca-certs` / `WORKINGMAN_EXTRA_CA_CERTS` is set) |
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
* with `--extra-ca-certs PEM` (default `$WORKINGMAN_EXTRA_CA_CERTS`): `sbx exec <name> -- sh -c <script> sh <base64 PEM>`, the daemon's idempotent CA install (the heredoc is byte-identical to `sbx.ExtraCAScript`, checked by a Go test)
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

* does the sandbox already exist with these mounts? (`sbx ls --json`)
* do we have a custom_secret set for the llm gateway? (`sbx secret ls --json`; acp-wrapper fails the launch if it never answers, `:session` carries on without the gateway)
* both JSON calls go through `sbx.Client`: stdout only, `WARN:`/JSON log lines before or after the document skipped, retried up to 5 times with jittered backoff under the host-wide `~/.workingman/locks/sbx.lock` (held only while sbx runs); exhaustion names the command (`sbx ls --json failed after 5 attempts: ...`)
* explicitly allow host.docker.interal:3333 and localhost:3333 for this sandbox

## sbx-rm.sh

delete the gateway created for this sandbox and then `sbx rm`

## Custom CA (VPN / TLS inspection)

Design and measurements: [docs/sandbox-custom-ca.md](docs/sandbox-custom-ca.md).

### When you need it

When the laptop's VPN re-signs HTTPS with a corporate CA, sandboxes do not
trust it, and inside them:

* `git` over https to github.com fails with `SSL certificate verification failed: certificate signer not trusted` (or `SSL certificate problem: ...`). acp-wrapper's **git TLS preflight** (`git ls-remote https://github.com/slimslenderslacks/workingman` from inside the sandbox) fails the launch with `git TLS preflight FAILED` in `acp-wrapper.log` and the session's error.
* the agent, when no global sbx secret targets the LLM gateway, answers only `Unable to connect to API: Self-signed certificate detected`; the **LLM TLS preflight** fails the launch with `LLM TLS preflight FAILED`.

Other failures of either probe (no git/curl in the sandbox, network policy, DNS, timeouts) only warn.
Laptops not on such a VPN leave the setting unset and nothing changes.

### Export the current corporate root (macOS keychain)

Use the root's exact name: `-c "Okta"` also matches the expired `Okta Inc Root CA`.

```bash
# What is there (label + SHA-256):
security find-certificate -a -c "Okta" -Z | grep -E '"labl"|SHA-256'
mkdir -p ~/.workingman
security find-certificate -a -c "Okta Inc Decryption Root CA" -p > ~/.workingman/extra-ca.pem
openssl x509 -in ~/.workingman/extra-ca.pem -noout -subject -enddate -fingerprint -sha256
```

Generic form: `security find-certificate -a -c "<name>" -p > ~/.workingman/extra-ca.pem`.
The file must be PEM; a DER `.cer` converts with
`openssl x509 -inform DER -in corporate-ca.cer -out ~/.workingman/extra-ca.pem`.

As measured on 2026-11-23, the VPN chains to `CN=Okta Inc Decryption Root CA, O=Okta`
(valid 2025-11-17 .. 2030-11-16), SHA-256
`A3:9F:E4:9D:3A:F8:98:39:2D:62:A4:62:38:70:50:30:99:DE:1B:C0:7A:20:E5:F8:E1:A7:57:96:F3:57:79:9A`.
To see what the VPN presents now: `openssl s_client -connect github.com:443 -servername github.com -showcerts </dev/null | grep -E '^ *[0-9] s:|i:'`.

**The sample `/Users/jim.clark/orch/sandbox-ca-certs/corporate-ca.cer` will not work.**
It is DER, it is `CN=Okta Inc Root CA` (not the root the VPN chains to), and it
**expired** (`notAfter=Dec 14 22:30:17 2025 GMT`). An expired anchor is useless:
curl, git and node all refuse to chain to it. Whenever the VPN's root is
reissued, re-export it and re-run `check-ca.sh`.

### Enable it on this laptop

* `start.sh`: `export WORKINGMAN_EXTRA_CA_CERTS=~/.workingman/extra-ca.pem` before `./start.sh` (orch reads it; start.sh echoes it).
* launchd: `orch service install --root ~/orch -- --extra-ca-certs ~/.workingman/extra-ca.pem`.
* `sbx-create.sh`: reads the same variable, or `--extra-ca-certs PEM`.

orch exits at startup if the file is unreadable or holds no PEM certificate,
and warns about expired certs. Every launch then installs the CA into the
sandbox (`/usr/local/share/ca-certificates/workingman-extra-ca.crt` +
`update-ca-certificates`) before the preflights run.

### Prove it: check-ca.sh

```bash
./check-ca.sh ~/.workingman/extra-ca.pem   # or: task check:ca -- ~/.workingman/extra-ca.pem
./check-ca.sh                              # uses $WORKINGMAN_EXTRA_CA_CERTS
```

* prints each cert's subject, issuer, notAfter and SHA-256, and flags expired ones loudly
* creates a throwaway probe sandbox `ca-probe-<pid>` like the daemon (`sbx create claude --kit $ACP_KIT -t $ACP_IMAGE --pull missing`, sbx-create.sh's defaults) and allows `github.com,api.anthropic.com` for it
* runs `git ls-remote https://github.com/slimslenderslacks/workingman HEAD`, `curl https://github.com` and `curl https://api.anthropic.com` **without** the CA, installs it with the daemon's script (from sbx-create.sh's heredoc; run twice to check idempotence), then runs them again **with** it
* if `sbx stop` exists, stops the probe and re-probes without re-installing (informational)
* prints a probe × without/with table and removes the probe sandbox on exit

It exits non-zero unless every probe passes with the CA and at least one cert
is unexpired. On the VPN, the expected result is CERT-FAIL without and ok with.
If everything passes without the CA, the network does not need one right now.

