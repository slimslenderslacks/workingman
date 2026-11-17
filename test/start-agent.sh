#!/usr/bin/env bash
# Exercises start.sh --check-agent with a stub op-read and a throwaway key.
set -euo pipefail
HERE="$(cd "$(dirname "$0")/.." && pwd)"
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
mkdir "$T/bin"
ssh-keygen -q -t ed25519 -N '' -f "$T/key" -C test
cat > "$T/bin/op-read" <<STUB
#!/usr/bin/env bash
[ "\$#" -eq 1 ] && [ "\$1" = 'op://x/y/z?ssh-format=openssh' ] || exit 2
echo x >> "$T/op-read-calls"
cat "$T/key"
STUB
chmod +x "$T/bin/op-read"
fail() { echo "FAIL: $*" >&2; exit 1; }

out="$(ORCH_OP_READ="$T/bin/op-read" TMPDIR="$T" ORCH_SSH_KEY_REF='op://x/y/z?ssh-format=openssh' "$HERE/start.sh" --check-agent)"
sock="$(sed -n 's/^SSH_AUTH_SOCK=//p' <<<"$out")"
pid="$(sed -n 's/^SSH_AGENT_PID=//p' <<<"$out")"
[ -n "$sock" ] && [ -n "$pid" ] || fail "no sock/pid reported: $out"
case "$sock" in "$T"/orch-ssh.*/agent.sock) ;; *) fail "SSH_AUTH_SOCK not the private agent: $sock" ;; esac
grep -q 'ED25519' <<<"$out" || fail "key not loaded"
grep -q 'PRIVATE KEY' <<<"$out" && fail "private key leaked"
[ "$(wc -l < "$T/op-read-calls")" -eq 1 ] || fail "op-read called $(wc -l < "$T/op-read-calls") times; want exactly 1"
! kill -0 "$pid" 2>/dev/null || fail "agent $pid still running"
[ ! -e "$sock" ] && [ ! -e "$(dirname "$sock")" ] || fail "socket dir not removed"

# Failure path: op-read fails -> startup aborts, nothing left behind.
printf '#!/bin/sh\nexit 1\n' > "$T/bin/op-read"
if ORCH_OP_READ="$T/bin/op-read" TMPDIR="$T" "$HERE/start.sh" --check-agent >/dev/null 2>&1; then fail "expected abort when op-read fails"; fi
[ -z "$(ls -d "$T"/orch-ssh.* 2>/dev/null)" ] || fail "leftover agent dir after failure"

# Missing binary -> startup aborts with a build hint, nothing left behind.
err="$(ORCH_OP_READ="$T/bin/no-such-op-read" TMPDIR="$T" "$HERE/start.sh" --check-agent 2>&1 >/dev/null)" && fail "expected abort when op-read is missing"
grep -q 'op-read' <<<"$err" || fail "missing-binary error does not name op-read: $err"
[ -z "$(ls -d "$T"/orch-ssh.* 2>/dev/null)" ] || fail "leftover agent dir after missing binary"
echo "PASS"
