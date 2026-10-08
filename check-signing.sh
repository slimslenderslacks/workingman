#!/usr/bin/env bash
# check-signing.sh — diagnose (and, on confirmation, fix) commit signing inside
# sbx sandboxes.
#
# Background: the orch commit agent SSH-signs commits using a key held by the
# private ssh-agent that start-orch.sh starts (key loaded from 1Password via
# `op`, never on disk) and exports as SSH_AUTH_SOCK. sbx exposes an agent inside
# every sandbox at /run/ssh-agent.sock, but only by proxying whatever agent the
# *sandboxd daemon* was launched with. If Docker Desktop later restarts sandboxd
# (an update, a crash, a sleep/wake that bounces the VM), it comes back bound to
# launchd's empty system agent, and every sandbox loses the signing key for the
# rest of the session. This script detects that and offers to re-point sandboxd.
#
# The private agent socket is taken from SSH_AUTH_SOCK in this shell if it is a
# live non-system agent, otherwise from the environment of the running
# start-orch.sh process.
#
# It distinguishes the failure modes:
#   - no private agent / agent has no key -> restart start-orch.sh (no sandboxd
#     restart helps).
#   - sandboxd forwarding an empty agent -> restart sandboxd with that socket.
#
# Usage:
#   ./check-signing.sh            # detect; prompt before restarting sandboxd
#   ./check-signing.sh --check    # detect only; never restart (exit 3 if broken)
#   ./check-signing.sh --yes      # detect; restart without prompting (for cron)
set -euo pipefail

AGENT_SOCK=""
PROBE="signing-doctor-$$"
ASSUME_YES=0
CHECK_ONLY=0

for arg in "$@"; do
  case "$arg" in
    -y|--yes)   ASSUME_YES=1 ;;
    --check)    CHECK_ONLY=1 ;;
    -h|--help)  sed -n '2,28p' "$0"; exit 0 ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

log()  { printf '%s\n' "$*"; }
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

# --- locate the private agent socket ---------------------------------------
# Echoes the socket path of the private start-orch.sh agent, or nothing.
detect_agent_sock() {
  local cand pid
  cand="${SSH_AUTH_SOCK:-}"
  if [ -n "$cand" ] && [ -S "$cand" ] && [[ "$cand" != *com.apple.launchd* ]]; then
    printf '%s\n' "$cand"; return
  fi
  # Fall back to the environment of the running start-orch.sh (ps eww prints it).
  for pid in $(pgrep -f 'start-orch\.sh' 2>/dev/null || true); do
    cand="$(ps eww -p "$pid" 2>/dev/null | tr ' ' '\n' | sed -n 's/^SSH_AUTH_SOCK=//p' | head -n1)"
    if [ -n "$cand" ] && [ -S "$cand" ] && [[ "$cand" != *com.apple.launchd* ]]; then
      printf '%s\n' "$cand"; return
    fi
  done
}

# --- fix: restart sandboxd bound to the private agent ----------------------
# Guarded by a confirmation prompt unless --yes was passed. Stops running
# sandboxes (they are recreated on demand), so we ask first.
restart_sandboxd() {
  if [ "$CHECK_ONLY" -eq 1 ]; then
    log "  (--check: not restarting; re-run without --check to fix)"
    return 1
  fi
  if [ "$ASSUME_YES" -ne 1 ]; then
    if [ ! -t 0 ]; then
      fail "signing is broken but stdin is not a TTY; re-run with --yes to allow the sandboxd restart"
    fi
    printf 'Restart sandboxd with the private ssh-agent? This stops running sandboxes (recreated on demand) [y/N] '
    read -r reply
    case "$reply" in
      y|Y|yes|YES) ;;
      *) log "aborted; sandboxd left as-is"; return 1 ;;
    esac
  fi
  log "restarting sandboxd bound to the private ssh-agent ($AGENT_SOCK)..."
  sbx daemon stop >/dev/null 2>&1 || true
  SSH_AUTH_SOCK="$AGENT_SOCK" sbx daemon start -d >/dev/null 2>&1 || fail "sbx daemon start failed"
  return 0
}

# --- preflight ------------------------------------------------------------
command -v sbx >/dev/null 2>&1 || fail "sbx not found on PATH"
AGENT_SOCK="$(detect_agent_sock)"
[ -n "$AGENT_SOCK" ] || fail "no private ssh-agent found: SSH_AUTH_SOCK is unset/the system agent and no running start-orch.sh exports one. Start orch via ./start-orch.sh."

# Does the private agent itself hold the key right now? ssh-add -l exits
# 0 (has identities), 1 (none — empty), or 2 (cannot connect).
if SSH_AUTH_SOCK="$AGENT_SOCK" ssh-add -l >/dev/null 2>&1; then
  :
else
  case $? in
    1) fail "private ssh-agent at $AGENT_SOCK has NO identities — restart start-orch.sh (check 'op' can authenticate non-interactively); a sandboxd restart will not help." ;;
    *) fail "cannot connect to the private ssh-agent at $AGENT_SOCK — start-orch.sh may have exited (the agent dies with it)." ;;
  esac
fi
log "private ssh-agent: OK (holds the signing key) at $AGENT_SOCK"

# --- probe the agent as a sandbox sees it ---------------------------------
# The truth we care about is what ssh-keygen sees inside a sandbox, i.e. what
# sandboxd's forwarder serves at /run/ssh-agent.sock. Probe with a throwaway
# sandbox so the check reflects the real commit-agent path. Always cleaned up.
PROBE_WS="$(mktemp -d)"
cleanup() {
  sbx rm --force "$PROBE" >/dev/null 2>&1 || true
  rmdir "$PROBE_WS" 2>/dev/null || true
}
trap cleanup EXIT

probe_forwarded() {
  # echoes "ok" if the forwarded agent has the key, "empty" if not, "createfail"
  # if the sandbox couldn't even be created (sandboxd itself unhealthy).
  if ! sbx create claude --name "$PROBE" "$PROBE_WS" >/dev/null 2>&1; then
    echo createfail; return
  fi
  local out
  out="$(sbx exec "$PROBE" -- ssh-add -l 2>&1 || true)"
  if printf '%s' "$out" | grep -q "SHA256:"; then echo ok; else echo empty; fi
}

# Push needs more than a key: github must accept it. github answers `ssh -T`
# with exit 1 even on success, so judge by the text. Runs in the probe sandbox.
probe_push() {
  local out
  out="$(sbx exec "$PROBE" -- sh -c 'ssh -T -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 git@github.com 2>&1' 2>&1 || true)"
  if printf '%s' "$out" | grep -qi "successfully authenticated"; then
    echo ok
  elif printf '%s' "$out" | grep -qi "permission denied"; then
    echo denied
  else
    echo unreachable
  fi
}

log "probing the agent forwarded into sandboxes (creating a throwaway sandbox)..."
result="$(probe_forwarded)"

case "$result" in
  ok)
    log "sandbox agent: OK — signing key is reachable inside sandboxes."
    push="$(probe_push)"
    case "$push" in
      ok) log "ssh push: OK — github.com accepts the forwarded key. Nothing to fix."; exit 0 ;;
      denied) fail "ssh push: github.com rejected the forwarded key (Permission denied). The key from ORCH_SSH_KEY_REF is not authorised for push; a sandboxd restart will not help." ;;
      *) fail "ssh push: could not reach github.com:22 from the sandbox (firewall? try 'sbx policy log'); a sandboxd restart will not help." ;;
    esac
    ;;
  empty)
    log "sandbox agent: BROKEN — the private agent has the key, but sandboxes see an EMPTY agent."
    log "  => sandboxd is forwarding the wrong agent (it was likely restarted since orch launched)."
    ;;
  createfail)
    log "sandbox agent: could not create a probe sandbox — sandboxd may be unhealthy."
    ;;
esac

# Broken. Clean up the (possibly created) probe before touching the daemon so
# the restart isn't fighting a live sandbox, then attempt the fix.
cleanup
trap - EXIT

if ! restart_sandboxd; then
  exit 3
fi

# --- confirm the fix worked ----------------------------------------------
PROBE_WS="$(mktemp -d)"
trap cleanup EXIT
log "re-probing after restart..."
if [ "$(probe_forwarded)" = "ok" ]; then
  log "FIXED — signing key is now reachable inside sandboxes."
  exit 0
fi
fail "still broken after restart — check that sandboxd inherited SSH_AUTH_SOCK=$AGENT_SOCK"
