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

# Rebuild orch and acp-wrapper from source so the running binaries always match
# HEAD. The daemon execs acp-wrapper fresh per session, so a stale binary here
# silently changes agent behaviour (commit signing, sandbox wiring) until
# someone remembers to `task build` — a footgun we hit more than once. go's
# build cache makes this near-instant when nothing changed. Abort on a build
# failure (set -e) rather than launch a stale binary; only skip when go isn't
# on PATH, where launching the prebuilt binary beats not starting at all.
if [ "$CHECK_AGENT" = 1 ]; then
  :
elif command -v go >/dev/null 2>&1; then
  echo "building orch and acp-wrapper from source..."
  ( cd "$HERE" && go build -o orch ./cmd/orch && go build -o acp-wrapper ./cmd/acp-wrapper )
else
  echo "start-orch: warning: go not found on PATH; launching prebuilt binaries (may be stale)" >&2
fi

# Commit signing needs an SSH key, but 1Password's SSH agent requires a human
# to unlock/approve every signing operation, so unattended runs hang. Instead
# run a private ssh-agent used only by this daemon and load the key into it
# straight from 1Password, never touching disk.
#
# NOTE: `op` must be able to authenticate non-interactively here — set
# OP_SERVICE_ACCOUNT_TOKEN, or have an already-unlocked CLI session — otherwise
# this step needs the human again. Override the key location with
# ORCH_SSH_KEY_REF (an op:// secret reference to an openssh-format private key).
ORCH_SSH_KEY_REF="${ORCH_SSH_KEY_REF:-op://Personal/SSH github/private key?ssh-format=openssh}"

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
  out="$(ssh-agent -a "$sock" -s)" || { echo "start-orch: ssh-agent failed to start" >&2; exit 1; }
  # Take only the numeric pid from the agent's output; don't eval it.
  AGENT_PID="$(printf '%s\n' "$out" | sed -n 's/^SSH_AGENT_PID=\([0-9][0-9]*\);.*/\1/p' | head -n1)"
  if [ -z "$AGENT_PID" ] || [ ! -S "$sock" ]; then
    echo "start-orch: could not determine private ssh-agent pid/socket" >&2
    exit 1
  fi

  if ! command -v op >/dev/null 2>&1; then
    echo "start-orch: error: 1Password CLI 'op' not found on PATH; cannot load the signing key" >&2
    exit 1
  fi
  # The key flows op -> pipe -> ssh-add; never a file, argv, or stdout.
  # (pipefail makes a failed `op read` fail the pipeline.)
  if ! op read "$ORCH_SSH_KEY_REF" | SSH_AUTH_SOCK="$sock" ssh-add - >/dev/null 2>&1; then
    echo "start-orch: error: failed to read the key from 1Password ($ORCH_SSH_KEY_REF) and load it into the private agent." >&2
    echo "start-orch: 'op' must authenticate non-interactively (OP_SERVICE_ACCOUNT_TOKEN or an unlocked CLI session)." >&2
    exit 1
  fi
  if ! SSH_AUTH_SOCK="$sock" ssh-add -l >/dev/null 2>&1; then
    echo "start-orch: error: private ssh-agent has no keys after load" >&2
    exit 1
  fi
  export SSH_AUTH_SOCK="$sock"
  echo "start-orch: private ssh-agent ready (pid $AGENT_PID); loaded key:"
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
if command -v sbx >/dev/null 2>&1; then
  sbx daemon stop  >/dev/null 2>&1 || true
  sbx daemon start -d >/dev/null 2>&1 || true
fi

# The orch binary moved into this workspace, so the old $HERE/../acp-kit default
# now resolves to a path with no kit checked out, which fails sandbox creation
# for every project ("resolve kits: path does not exist"). Point ACP_KIT at the
# real kit checkout; still overridable from the environment.
export ACP_KIT="${ACP_KIT:-/Users/slim/dev/repos_docker/slimslenderslacks/acp-kit}"

# Run orch as a child (not exec) so the EXIT trap can stop the private agent.
# Forward INT/TERM to it for a clean shutdown and preserve its exit status.
"$HERE/orch" \
  --root ~/orch \
  --audit-log ~/orch/audit.log \
  --acp-kit "${ACP_KIT:-$HERE/../acp-kit}" \
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
