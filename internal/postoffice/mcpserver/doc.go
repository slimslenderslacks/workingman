// Package mcpserver is the post-office MCP server: the sender tools agents
// use to notify and ask the operator or another agent, and the receiver
// tools agent recipients such as Step-Up answer them with, over the shared
// postoffice.Store. cmd/post-office serves it; see docs/messaging-design.md
// §4.
//
// Role: daemon (the post-office process, cmd/post-office)
//
// Agents never reach post-office directly. Each call arrives through the
// standalone-codemode sub-gateway of the agent's sandbox, which sets
// X-Codemode-Gateway-Id ([GatewayIDHeader]) to its own id. The server:
//
//  1. requires `Authorization: Bearer <token>` ([RequireBearer]; the token
//     is DIR/token, made by `post-office setup`, [EnsureToken]);
//  2. resolves the gateway id to a caller ([Resolver]): the gateway record
//     (~/.workingman/gateways/<sandbox>.json) gives the sandbox and the
//     servers it was minted with, which must include post-office — mcp-add
//     never changes that list, so a dynamic gateway cannot grant itself the
//     capability (§4.2) — and the sandbox's newest live session gives the
//     agent kind, project and task. The fixed-name agents (workingman-agent,
//     sleepy-head, step-up) are identified by sandbox name. Results are
//     cached for 30 s; a rejected caller gets a JSON-RPC error;
//  3. serves the tools with the schemas and descriptions of §4.4
//     (send_notification, send_request, wait_for_response, check_response,
//     cancel_request, list_requests). Every result's text starts with
//     "status: <state>" followed by "key: value" lines, a multi-line
//     response last; the same fields are the structuredContent.
//
// Registered recipients (Config.Recipients; v1 step-up → sandbox step-up)
// also get the receiver tools (§4.5): next_message (the oldest unreceived
// message addressed to them, marked received with a compare-and-set so it is
// returned once; a notification is then dismissed), reply (an agent answer,
// or status: closed with the outcome that won the claim) and list_inbox. On
// send_request they get in_reply_to, linking their own request to the one it
// serves; it is set automatically when they hold exactly one received,
// unanswered request. Callers known not to be recipients do not see these in
// tools/list and are refused if they call them.
//
// Requests are durable files, so waiting is a bounded loop (§4.6):
// send_request and wait_for_response block for at most Config.MaxWait
// (default 50 s), waking when post-office finalises the request, send a
// notifications/progress heartbeat every 15 s when the call has a progress
// token, and return "status: pending" when time runs out. A client cancel
// ends the call, never the request. [Server.Run] finalises claims as they
// appear in outbox/ (fsnotify plus poll), expires overdue messages every
// 30 s, prunes done/, and does all three at startup.
//
// post-office://messages/<id> is a resource template returning a message's
// inbox or done record, to its sender, or the agent recipient it is
// addressed to, only (§4.7).
//
// # Key types
//
//   - [Server] — New, Handler (streamable HTTP, stateless), Run.
//   - [Config] — store, resolver, recipient registry, max wait, intervals.
//   - [Resolver] / [Caller] — gateway id → postoffice.Sender.
//
// # Collaborators
//
// [github.com/slimslenderslacks/work/internal/postoffice] (the store),
// [github.com/slimslenderslacks/work/internal/mcpgateway] (gateway records),
// [github.com/slimslenderslacks/work/internal/session] (live sessions) and
// [github.com/slimslenderslacks/work/internal/task] (task names), over
// github.com/modelcontextprotocol/go-sdk at the version standalone-codemode
// pins.
package mcpserver
