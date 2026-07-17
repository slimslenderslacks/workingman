#!/usr/bin/env bash
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

# Rebuild orch and acp-wrapper from source so the running binaries always match
# HEAD. The daemon execs acp-wrapper fresh per session, so a stale binary here
# silently changes agent behaviour (commit signing, sandbox wiring) until
# someone remembers to `task build` — a footgun we hit more than once. go's
# build cache makes this near-instant when nothing changed. Abort on a build
# failure (set -e) rather than launch a stale binary; only skip when go isn't
# on PATH, where launching the prebuilt binary beats not starting at all.
if command -v go >/dev/null 2>&1; then
  echo "building orch and acp-wrapper from source..."
  ( cd "$HERE" && go build -o orch ./cmd/orch && go build -o acp-wrapper ./cmd/acp-wrapper )
else
  echo "start-orch: warning: go not found on PATH; launching prebuilt binaries (may be stale)" >&2
fi

# 1Password's SSH agent holds the commit-signing key. A GUI/login shell's
# default SSH_AUTH_SOCK points at the macOS system agent, which lacks that key,
# so orch (and the sandboxes it forwards the agent into) couldn't sign commits.
# Point it at the 1Password agent when that socket exists. orch defaults this
# too, but exporting it here also covers running the binary directly.
OP_AGENT="$HOME/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"
if [ -S "$OP_AGENT" ]; then
  export SSH_AUTH_SOCK="$OP_AGENT"

  # sbx exposes an SSH agent inside every sandbox at /run/ssh-agent.sock,
  # proxied to whatever agent the *sandboxd daemon* was launched with — NOT the
  # sbx client's and NOT a bind-mounted host socket (a macOS unix socket has no
  # listener reachable across the Docker VM boundary). If sandboxd is already
  # running under launchd's empty system agent, the commit agent's ssh-keygen
  # sees no key and signing fails. Restart it so it adopts the 1Password agent
  # we just exported. This stops running sandboxes; they are recreated on
  # demand, so it's safe at orchestrator startup.
  if command -v sbx >/dev/null 2>&1; then
    sbx daemon stop  >/dev/null 2>&1 || true
    sbx daemon start -d >/dev/null 2>&1 || true
  fi
fi

"$HERE/orch" \
  --root ~/orch \
  --audit-log ~/orch/audit.log \
  --acp-kit "${ACP_KIT:-$HERE/../acp-kit}" \
  --acp-wrapper "$HERE/acp-wrapper"
