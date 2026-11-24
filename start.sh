#!/usr/bin/env bash
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

# --check-agent (or ORCH_CHECK_AGENT=1): start the private ssh-agent, load the
# key, print status, tear down, and exit — no build, no sbx restart, no orch.
CHECK_AGENT="${ORCH_CHECK_AGENT:-0}"
if [ "${1:-}" = "--check-agent" ]; then
  CHECK_AGENT=1
  shift
fi

# Rebuild orch, acp-wrapper and op-read from source so the running binaries always match
# HEAD. The daemon execs acp-wrapper fresh per session, so a stale binary here
# silently changes agent behaviour (commit signing, sandbox wiring) until
# someone remembers to `task build` — a footgun we hit more than once. go's
# build cache makes this near-instant when nothing changed. Abort on a build
# failure (set -e) rather than launch a stale binary; only skip when go isn't
# on PATH, where launching the prebuilt binary beats not starting at all.
if [ "$CHECK_AGENT" = 1 ]; then
  :
elif command -v go >/dev/null 2>&1; then
  echo "building orch, acp-wrapper and op-read from source..."
  ( cd "$HERE" && go build -o orch ./cmd/orch && go build -o acp-wrapper ./cmd/acp-wrapper && go build -o op-read ./cmd/op-read )
else
  echo "start: warning: go not found on PATH; launching prebuilt binaries (may be stale)" >&2
fi

# Commit signing needs an SSH key, but 1Password's SSH agent requires a human
# to unlock/approve every signing operation, so unattended runs hang. Instead
# run a private ssh-agent used only by this daemon and load the key into it
# straight from 1Password, never touching disk.
#
# The key is read by op-read (cmd/op-read), which uses the 1Password Go SDK
# authenticated as a 1Password *service account* — no `op` CLI, no `op signin`,
# no biometric prompt. Its token comes from OP_SERVICE_ACCOUNT_TOKEN if set,
# else the macOS keychain item with service "workingman:service_account" and
# account "jim.clark" (override with OP_SA_KEYCHAIN_SERVICE /
# OP_SA_KEYCHAIN_ACCOUNT / OP_SA_KEYCHAIN). Seed it once with
#
#   security add-generic-password -U -a jim.clark -s workingman:service_account -w
#
# ORCH_SSH_KEY_REF is the op:// secret reference to the openssh-format private
# key. A service account cannot see a Personal vault, so the item must live in
# a shared vault granted to the service account (default: vault "workingman-okta").
# ORCH_OP_READ overrides the op-read binary (tests point it at a stub).
#
# This op-read is the ONLY 1Password call in workingman, and it runs exactly once
# per start.sh. The key then lives only in the private agent's memory for the
# life of the daemon: nothing re-reads it — not orch, acp-wrapper, the
# preflights, nor check-signing.sh's sandboxd restart, which rebinds sandboxd
# to this same in-memory agent. orch also pins its host-side git/ssh to this
# agent (GIT_SSH_COMMAND IdentityAgent, gpg.ssh.program=ssh-keygen) so a
# 1Password IdentityAgent/op-ssh-sign in the user's ssh/git config never fires.
ORCH_SSH_KEY_REF="${ORCH_SSH_KEY_REF:-op://workingman-okta/SSH github/private key?ssh-format=openssh}"
ORCH_OP_READ="${ORCH_OP_READ:-$HERE/op-read}"

AGENT_DIR=""
AGENT_PID=""
ORCH_PID=""

cleanup() {
  trap - EXIT INT TERM
  if [ -n "$AGENT_PID" ]; then
    SSH_AGENT_PID="$AGENT_PID" ssh-agent -k >/dev/null 2>&1 || kill "$AGENT_PID" 2>/dev/null || true
  fi
  if [ -n "$AGENT_DIR" ]; then
    rm -rf "$AGENT_DIR"
  fi
}
# Signals during setup: exit (status 128+n), which runs the EXIT trap.
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

start_private_agent() {
  # Short path: macOS unix socket paths are limited to ~104 chars.
  AGENT_DIR="$(mktemp -d "${TMPDIR:-/tmp}/orch-ssh.XXXXXX")"
  chmod 700 "$AGENT_DIR"
  local sock="$AGENT_DIR/agent.sock" out
  out="$(ssh-agent -a "$sock" -s)" || { echo "start: ssh-agent failed to start" >&2; exit 1; }
  # Take only the numeric pid from the agent's output; don't eval it.
  AGENT_PID="$(printf '%s\n' "$out" | sed -n 's/^SSH_AGENT_PID=\([0-9][0-9]*\);.*/\1/p' | head -n1)"
  if [ -z "$AGENT_PID" ] || [ ! -S "$sock" ]; then
    echo "start: could not determine private ssh-agent pid/socket" >&2
    exit 1
  fi

  if [ ! -x "$ORCH_OP_READ" ]; then
    echo "start: error: $ORCH_OP_READ not found or not executable; cannot load the signing key. Build it with 'task op-read' (or 'go build -o op-read ./cmd/op-read')." >&2
    exit 1
  fi
  # The key flows op-read -> pipe -> ssh-add; never a file, argv, or stdout.
  # op-read's own errors (no token, no access to the vault) go to stderr.
  # (pipefail makes a failed op-read fail the pipeline.)
  if ! "$ORCH_OP_READ" "$ORCH_SSH_KEY_REF" | SSH_AUTH_SOCK="$sock" ssh-add - >/dev/null 2>&1; then
    echo "start: error: failed to read the key from 1Password ($ORCH_SSH_KEY_REF) and load it into the private agent." >&2
    echo "start: op-read needs a 1Password service account token (OP_SERVICE_ACCOUNT_TOKEN, or the keychain item workingman:service_account / jim.clark) for a service account that can read that vault; see docs/ssh-agent.md." >&2
    exit 1
  fi
  if ! SSH_AUTH_SOCK="$sock" ssh-add -l >/dev/null 2>&1; then
    echo "start: error: private ssh-agent has no keys after load" >&2
    exit 1
  fi
  export SSH_AUTH_SOCK="$sock"
  echo "start: private ssh-agent ready (pid $AGENT_PID); loaded key:"
  ssh-add -l
}

start_private_agent

if [ "$CHECK_AGENT" = 1 ]; then
  # Dry run for tests: report the agent, then exit (trap tears it down).
  echo "SSH_AUTH_SOCK=$SSH_AUTH_SOCK"
  echo "SSH_AGENT_PID=$AGENT_PID"
  exit 0
fi

# sbx exposes an SSH agent inside every sandbox at /run/ssh-agent.sock,
# proxied to whatever agent the *sandboxd daemon* was launched with — NOT the
# sbx client's and NOT a bind-mounted host socket (a macOS unix socket has no
# listener reachable across the Docker VM boundary). If sandboxd is already
# running under another agent, the commit agent's ssh-keygen sees the wrong
# keys and signing fails. Restart it so it adopts the private agent we just
# exported. This stops running sandboxes; they are recreated on demand, so
# it's safe at orchestrator startup.
# Failures are no longer swallowed: a sandboxd that did not restart (or came up
# without our SSH_AUTH_SOCK) makes every ssh push/sign fail later, so abort here
# with an actionable message instead.
if command -v sbx >/dev/null 2>&1; then
  sbx daemon stop >/dev/null 2>&1 || true
  if ! sbx daemon start -d >/dev/null 2>&1; then
    echo "start: error: 'sbx daemon start -d' failed; sandboxd is not bound to the private ssh-agent ($SSH_AUTH_SOCK). Fix sbx and re-run." >&2
    exit 1
  fi
  # Verify sandboxd is bound to the private agent (probes via a throwaway
  # sandbox) and, if sandboxd runs without our SSH_AUTH_SOCK, restarts it bound
  # to the private agent (--yes). Skip with ORCH_SKIP_AGENT_CHECK=1.
  if [ "${ORCH_SKIP_AGENT_CHECK:-0}" != 1 ] && [ -x "$HERE/check-signing.sh" ]; then
    if ! "$HERE/check-signing.sh" --yes; then
      echo "start: error: sandboxd does not forward the private ssh-agent; ssh push/signing would fail. Run ./check-signing.sh --yes, or set ORCH_SKIP_AGENT_CHECK=1 to ignore." >&2
      exit 1
    fi
  fi
fi

# The orch binary moved into this workspace, so the old $HERE/../acp-kit default
# now resolves to a path with no kit checked out, which fails sandbox creation
# for every project ("resolve kits: path does not exist"). Point ACP_KIT at the
# real kit checkout; still overridable from the environment.
# Prefer a sibling acp-kit checkout (a wsp workspace) when there is one: the
# kit's install step rewrites claude-acp-client on every `sbx create`, so an
# older kit undoes the image's okta-gateway client.
#
# The kit is resolved from where THIS start.sh lives ($HERE), not from the
# shell's cwd — but that still means the checkout you start from picks the
# kit: start.sh in a wsp workspace with its own (possibly stale) ../acp-kit
# uses that one, a checkout without one falls back to the path below, and an
# ACP_KIT already in the environment wins over both. A kit whose
# claude-acp-client predates okta-gateway leaves new sandboxes with the stock
# mcp-gateway MCP server. orch warns about a stale or image-mismatched kit at
# startup (acp_kit_warning in the audit log), and the workingman agent's
# persistent sandbox is recreated whenever it was built from a different kit
# or image than the current one (see docs/mcp-gateway.md).
if [ -f "$HERE/../acp-kit/spec.yaml" ]; then
  export ACP_KIT="${ACP_KIT:-$(cd "$HERE/../acp-kit" && pwd)}"
else
  export ACP_KIT="${ACP_KIT:-$HOME/dev/repos/slimslenderslacks/acp-kit}"
fi

# Create every ACP sandbox from acp-kit's prebuilt image rather than the stock
# claude template. acp-kit is a mixin, and a user --kit mixin is never embedded
# into a base image, so its install step — a ~540MB `npm install -g` of the ACP
# bridge and the claude-agent-sdk's native binaries — otherwise runs on EVERY
# `sbx create`. This image bakes that same install (acp-kit's own Dockerfile),
# and the kit's install step short-circuits on it because acp-kit skips the
# Node/npm work when claude-agent-acp is already on PATH.
#
# The image must live in the SANDBOX RUNTIME's image store, which is separate
# from the host docker's — `docker build` alone is not enough, and `sbx create`
# fails at PREPARE IMAGE with a registry 403 for an image sitting right there in
# `docker images`. From the acp-kit checkout:
#
#   make image-build
#   docker save slimslenderslacks/claude-code-acp:0.90.0 -o /tmp/acp.tar
#   sbx template load /tmp/acp.tar
#
# Confirm with `sbx template ls`. The tag is acp-kit's IMAGE_VERSION, which is
# versioned independently of the bridge version its Dockerfile pins: it is
# bumped whenever anything baked into the image changes (0.90.0 adds the
# `claude` shim, so the interactive `claude` from `sbx run` also registers
# `okta-gateway` and drops the stock `mcp-gateway` MCP server, as
# claude-acp-client already does for ACP sessions). Keep this default in step with acp-kit's IMAGE_VERSION.
# sbx creates with `--pull missing`, so only a NEW tag gets a new image; an
# already-loaded tag is reused as-is. Existing sandboxes are not rebuilt when
# this changes (see docs/mcp-gateway.md). Set ACP_IMAGE="" to fall back to the
# stock template (and pay the ~31s per-create install again).
export ACP_IMAGE="${ACP_IMAGE-slimslenderslacks/claude-code-acp:0.90.0}"

# Route every sandbox's MCP traffic through its own sub-gateway on the host's
# standalone-codemode gateway (see "MCP gateway" in README.md). Without a URL
# orch sets no MCP_URL, so acp-kit keeps the stock `mcp-gateway` MCP server
# instead of `okta-gateway`. The servers are the default set for a task with
# no static_mcps, and the workingman agent's starting set; wolf, task and
# workingman-agent gateways also get mcp-find/mcp-add over the whole catalog,
# so those agents can attach more servers (e.g. Slack) themselves.
# WORKINGMAN_AGENT_MCP_DYNAMIC=0 makes the workingman agent's gateway static;
# WORKINGMAN_MCP_GATEWAY_STATIC=1 makes every gateway static. Changing either
# recreates the persistent workingman-agent sandbox on its next launch.
# Set WORKINGMAN_MCP_GATEWAY_URL="" to turn the gateway off.
export WORKINGMAN_MCP_GATEWAY_URL="${WORKINGMAN_MCP_GATEWAY_URL-http://localhost:3333}"
export WORKINGMAN_MCP_GATEWAY_SERVERS="${WORKINGMAN_MCP_GATEWAY_SERVERS-github}"
export WORKINGMAN_AGENT_MCP_DYNAMIC="${WORKINGMAN_AGENT_MCP_DYNAMIC-1}"
# The gateway listens only after attaching its whole catalog, so right after
# a restart it refuses connections for a while. orch relaunches agents (a
# blocked project's wolf, say) as soon as it starts, so wait for the gateway
# here rather than have those agents come up without it. acp-wrapper also
# waits (--mcp-gateway-wait) and fails a launch rather than run without it.
if [ "$CHECK_AGENT" != 1 ] && [ -n "$WORKINGMAN_MCP_GATEWAY_URL" ]; then
  gw_health="${WORKINGMAN_MCP_GATEWAY_URL%/}/healthz"
  gw_wait="${WORKINGMAN_MCP_GATEWAY_WAIT:-120}"
  waited=0
  until curl -fsS -m 3 -o /dev/null "$gw_health" 2>/dev/null; do
    if [ "$waited" -eq 0 ]; then
      echo "start: waiting up to ${gw_wait}s for the MCP gateway at $gw_health (start standalone-codemode's bin/gateway)..." >&2
    fi
    if [ "$waited" -ge "$gw_wait" ]; then
      echo "start: warning: MCP gateway still not answering at $gw_health after ${gw_wait}s;" >&2
      echo "start: agents will wait for it at launch and fail if it never comes up." >&2
      break
    fi
    sleep 2
    waited=$((waited + 2))
  done
  if [ "$waited" -gt 0 ] && [ "$waited" -lt "$gw_wait" ]; then
    echo "start: MCP gateway ready after ${waited}s" >&2
  fi
fi

# orch reads WORKINGMAN_EXTRA_CA_CERTS (the per-laptop VPN root, see
# docs/sandbox-custom-ca.md) itself, so it needs no flag here; just echo it.
# Say which kit, image and gateway this run uses: the usual answer to "why does
# a sandbox have mcp-gateway instead of okta-gateway" is one of these three.
echo "start: acp-kit=$ACP_KIT acp-image=${ACP_IMAGE:-<stock claude template>} mcp-gateway-url=${WORKINGMAN_MCP_GATEWAY_URL:-<none: sandboxes keep mcp-gateway>} mcp-gateway-servers=${WORKINGMAN_MCP_GATEWAY_SERVERS:-<none>} workingman-agent-mcp-dynamic=$WORKINGMAN_AGENT_MCP_DYNAMIC extra-ca-certs=${WORKINGMAN_EXTRA_CA_CERTS:-<none>}" >&2

# Run orch as a child (not exec) so the EXIT trap can stop the private agent.
# Forward INT/TERM to it for a clean shutdown and preserve its exit status.
"$HERE/orch" \
  --root ~/orch \
  --audit-log ~/orch/audit.log \
  --acp-kit "$ACP_KIT" \
  --acp-image "$ACP_IMAGE" \
  --acp-wrapper "$HERE/acp-wrapper" <&0 &
ORCH_PID=$!
trap 'kill -TERM "$ORCH_PID" 2>/dev/null || true' INT TERM
rc=0
wait "$ORCH_PID" || rc=$?
# A forwarded signal interrupts `wait` early; keep waiting for the real status.
while kill -0 "$ORCH_PID" 2>/dev/null; do
  wait "$ORCH_PID" || rc=$?
done
exit "$rc"
