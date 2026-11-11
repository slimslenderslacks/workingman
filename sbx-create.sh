#!/usr/bin/env bash
# sbx-create.sh --name <sandbox> [options] [workspace...]
#
# Creates a sandbox wired to its own MCP sub-gateway on the host's
# standalone-codemode gateway, the same way workingman does:
#
#   1. POST <gateway>/gateways {"servers":[...],"dynamic":true}
#      (default: github; dynamic adds mcp-find/mcp-add over the whole catalog)
#   2. MCP_URL = the returned URL, localhost rewritten to host.docker.internal
#   3. sbx create ... --kit $ACP_KIT -e MCP_URL=... plus acp-wrapper's claude
#      env (gateway model aliases, and ANTHROPIC_BASE_URL=https://llm.es.atko.services
#      when sbx holds a secret for that host); allow the gateway port
#   4. sbx run --name <sandbox> -e ...   (attach; skip with --no-run)
#
# The gateway is recorded at $WORKINGMAN_GATEWAY_STATE/<sandbox>.json
# (default ~/.workingman/gateways, workingman's own record format), which
# sbx-rm.sh reads to DELETE it.
#
# Options:
#   --name NAME          sandbox name (required)
#   --servers a,b        catalog servers for the gateway (default: github)
#   --static             no mcp-find/mcp-add (default: a dynamic gateway)
#   --gateway-url URL    host gateway (default $WORKINGMAN_MCP_GATEWAY_URL,
#                        else http://localhost:3333)
#   --image IMAGE        template image (default $ACP_IMAGE, else
#                        slimslenderslacks/claude-code-acp:0.90.0)
#   --kit KIT            acp-kit mixin (default $ACP_KIT, else this workspace's
#                        ../acp-kit, else ~/dev/repos/slimslenderslacks/acp-kit;
#                        "" for none). The kit's install step rewrites
#                        claude-acp-client, so it must be as new as the image.
#   --agent AGENT        sbx agent (default: claude)
#   --no-run             create only; don't attach
#   workspace...         paths to mount (default: current directory)
set -euo pipefail

usage() { sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; }
die() { echo "sbx-create: $*" >&2; exit 1; }

NAME=""
SERVERS="github"
GATEWAY_URL="${WORKINGMAN_MCP_GATEWAY_URL:-http://localhost:3333}"
IMAGE="${ACP_IMAGE-slimslenderslacks/claude-code-acp:0.90.0}"
HERE="$(cd "$(dirname "$0")" && pwd)"
if [ -f "$HERE/../acp-kit/spec.yaml" ]; then DEFAULT_KIT="$(cd "$HERE/../acp-kit" && pwd)"
else DEFAULT_KIT="$HOME/dev/repos/slimslenderslacks/acp-kit"; fi
KIT="${ACP_KIT-$DEFAULT_KIT}"
AGENT="claude"
DYNAMIC=true
RUN=1
WORKSPACES=()
while [ $# -gt 0 ]; do
  case "$1" in
    --name) NAME="${2:?--name needs a value}"; shift 2 ;;
    --name=*) NAME="${1#*=}"; shift ;;
    --servers) SERVERS="${2:?--servers needs a value}"; shift 2 ;;
    --servers=*) SERVERS="${1#*=}"; shift ;;
    --gateway-url) GATEWAY_URL="${2:?--gateway-url needs a value}"; shift 2 ;;
    --gateway-url=*) GATEWAY_URL="${1#*=}"; shift ;;
    --image) IMAGE="${2?--image needs a value}"; shift 2 ;;
    --image=*) IMAGE="${1#*=}"; shift ;;
    --kit) KIT="${2?--kit needs a value}"; shift 2 ;;
    --kit=*) KIT="${1#*=}"; shift ;;
    --agent) AGENT="${2:?--agent needs a value}"; shift 2 ;;
    --no-run) RUN=0; shift ;;
    --static) DYNAMIC=false; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) die "unknown option $1 (see --help)" ;;
    *) WORKSPACES+=("$1"); shift ;;
  esac
done
[ -n "$NAME" ] || die "--name is required (see --help)"
case "$NAME" in */*|.|..) die "invalid sandbox name '$NAME'" ;; esac
[ ${#WORKSPACES[@]} -gt 0 ] || WORKSPACES=("$PWD")
for bin in sbx curl jq; do command -v "$bin" >/dev/null || die "'$bin' not found on PATH"; done

STATE_DIR="${WORKINGMAN_GATEWAY_STATE:-$HOME/.workingman/gateways}"
RECORD="$STATE_DIR/$NAME.json"
GATEWAY_URL="${GATEWAY_URL%/}"

if sbx ls --json 2>/dev/null | jq -e --arg n "$NAME" '(if type == "array" then .[] else .sandboxes[]? end) | select(.name == $n)' >/dev/null; then
  die "sandbox '$NAME' already exists; remove it first with ./sbx-rm.sh --name $NAME"
fi
curl -fsS -m 5 "$GATEWAY_URL/healthz" >/dev/null || die "gateway not reachable at $GATEWAY_URL/healthz"

# 1. Mint the sub-gateway.
body=$(jq -cn --arg s "$SERVERS" --argjson d "$DYNAMIC" '{servers: ($s | split(",") | map(select(length > 0))), dynamic: $d}')
resp=$(curl -sS -m 120 -w '\n%{http_code}' -X POST "$GATEWAY_URL/gateways" \
  -H 'Content-Type: application/json' -d "$body")
code=${resp##*$'\n'}; resp=${resp%$'\n'*}
[ "$code" = 201 ] || die "POST $GATEWAY_URL/gateways $body -> $code: $resp"
GW_ID=$(jq -r .id <<<"$resp"); GW_URL=$(jq -r .url <<<"$resp")
echo "sbx-create: gateway $GW_ID over [$SERVERS] (dynamic: $DYNAMIC)"

delete_gateway() { curl -sS -m 15 -o /dev/null -X DELETE "$GATEWAY_URL/gateways/$GW_ID" || true; }
trap 'echo "sbx-create: failed; deleting gateway $GW_ID" >&2; delete_gateway' ERR

# 2. The sandbox reaches the host's loopback as host.docker.internal.
MCP_URL=$(sed -E 's#^(https?://)(localhost|127\.0\.0\.1|\[::1\])([:/]|$)#\1host.docker.internal\3#' <<<"$GW_URL")
PORT=$(sed -E 's#^https?://[^/]*:([0-9]+).*#\1#' <<<"$GW_URL")
[[ "$PORT" =~ ^[0-9]+$ ]] || PORT=80
echo "sbx-create: MCP_URL=$MCP_URL"

# 3. Create the sandbox with MCP_URL and acp-wrapper's claude env (claudeEnv,
# and ANTHROPIC_BASE_URL only when sbx can authenticate to the LLM gateway:
# its proxy swaps in the real credential only for requests to that host).
LLM_GATEWAY_HOST=llm.es.atko.services
CLAUDE_ENV=(
  -e "MCP_URL=$MCP_URL"
  -e "ANTHROPIC_DEFAULT_OPUS_MODEL=claude-opus-5-5[1m]"
  -e "ANTHROPIC_DEFAULT_SONNET_MODEL=claude-sonnet-5-5[1m]"
  -e "ANTHROPIC_DEFAULT_HAIKU_MODEL=claude-haiku-5-5"
  -e "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"
  -e "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1"
)
if sbx secret ls --json 2>/dev/null | jq -e --arg h "$LLM_GATEWAY_HOST" --arg n "$NAME" \
    '.custom_secrets[]? | select(.scope == "global" or .scope == $n) | select(.targets | index($h))' >/dev/null; then
  CLAUDE_ENV+=(-e "ANTHROPIC_BASE_URL=https://$LLM_GATEWAY_HOST")
  echo "sbx-create: LLM gateway $LLM_GATEWAY_HOST"
else
  echo "sbx-create: WARNING no sbx secret targets $LLM_GATEWAY_HOST; using the default Anthropic API" >&2
fi
args=(create "$AGENT" --name "$NAME" "${CLAUDE_ENV[@]}")
[ -n "$KIT" ] && args+=(--kit "$KIT")
[ -n "$IMAGE" ] && args+=(-t "$IMAGE" --pull missing)
sbx "${args[@]}" "${WORKSPACES[@]}"
sbx policy allow network --sandbox "$NAME" "host.docker.internal:$PORT,localhost:$PORT" >/dev/null

mkdir -p "$STATE_DIR"
jq -n --arg sandbox "$NAME" --arg base "$GATEWAY_URL" --arg id "$GW_ID" --arg url "$GW_URL" \
  --arg surl "$MCP_URL" --arg s "$SERVERS" --argjson d "$DYNAMIC" --arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  '($s | split(",") | map(select(length > 0))) as $servers |
   {sandbox: $sandbox, base_url: $base, id: $id, url: $url, sandbox_url: $surl,
    requested: $servers, servers: $servers, dynamic: $d, created_at: $now}' >"$RECORD"
trap - ERR
echo "sbx-create: sandbox $NAME created (gateway recorded in $RECORD)"

# 4. Attach.
if [ "$RUN" = 1 ]; then
  exec sbx run --name "$NAME" "${CLAUDE_ENV[@]}"
fi
