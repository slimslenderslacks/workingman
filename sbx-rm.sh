#!/usr/bin/env bash
# sbx-rm.sh --name <sandbox>
#
# Removes a sandbox and DELETEs the MCP sub-gateway it uses. The gateway comes
# from $WORKINGMAN_GATEWAY_STATE/<sandbox>.json (default
# ~/.workingman/gateways, written by sbx-create.sh and by workingman); if
# there is no record, the gateway id is read from the sandbox's MCP_URL.
# A gateway that is already gone (404) is fine.
#
# Options:
#   --name NAME   sandbox name (required)
set -euo pipefail

usage() { sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; }
die() { echo "sbx-rm: $*" >&2; exit 1; }

NAME=""
while [ $# -gt 0 ]; do
  case "$1" in
    --name) NAME="${2:?--name needs a value}"; shift 2 ;;
    --name=*) NAME="${1#*=}"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument $1 (see --help)" ;;
  esac
done
[ -n "$NAME" ] || die "--name is required (see --help)"
case "$NAME" in */*|.|..) die "invalid sandbox name '$NAME'" ;; esac
for bin in sbx curl jq; do command -v "$bin" >/dev/null || die "'$bin' not found on PATH"; done

RECORD="${WORKINGMAN_GATEWAY_STATE:-$HOME/.workingman/gateways}/$NAME.json"
EXISTS=0
sbx ls --json 2>/dev/null | jq -e --arg n "$NAME" '(if type == "array" then .[] else .sandboxes[]? end) | select(.name == $n)' >/dev/null && EXISTS=1

# Which gateway: the record (host-side URL), else the sandbox's MCP_URL.
GW_DELETE_URL=""
if [ -f "$RECORD" ]; then
  GW_DELETE_URL=$(jq -r '.base_url + "/gateways/" + .id' "$RECORD")
elif [ "$EXISTS" = 1 ]; then
  url=$(sbx exec "$NAME" -- sh -c 'printf %s "${MCP_URL:-}"' 2>/dev/null || true)
  if [[ "$url" == */gateways/* ]]; then
    GW_DELETE_URL=$(sed -E 's#host\.docker\.internal#localhost#' <<<"$url")
  fi
fi

if [ "$EXISTS" = 1 ]; then
  sbx rm --force "$NAME"
  echo "sbx-rm: removed sandbox $NAME"
else
  echo "sbx-rm: no sandbox named $NAME"
fi

if [ -n "$GW_DELETE_URL" ]; then
  code=$(curl -sS -m 15 -o /dev/null -w '%{http_code}' -X DELETE "$GW_DELETE_URL" || echo 000)
  case "$code" in
    204|200) echo "sbx-rm: deleted gateway $GW_DELETE_URL" ;;
    404) echo "sbx-rm: gateway $GW_DELETE_URL was already gone" ;;
    *) echo "sbx-rm: WARNING: DELETE $GW_DELETE_URL -> $code; record kept at $RECORD" >&2; exit 1 ;;
  esac
else
  echo "sbx-rm: no MCP gateway recorded for $NAME"
fi
rm -f "$RECORD"
