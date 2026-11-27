// Package api is the daemon's optional HTTP API: the listener, its lifecycle
// and authentication, the response conventions every endpoint shares, the
// live event stream, the write actions' rules, and the web GUI's static
// bundle and browser login.
//
// Role: daemon
//
// The API runs inside the orch daemon process and is off by default
// (--api=off). With --api=unix it serves on a unix socket,
// ~/.workingman/api.sock (mode 0600, in a 0700 directory when it creates
// one); with --api=loopback it serves on loopback TCP, 127.0.0.1:7078 by
// default. A non-loopback address is refused. When the listener cannot be
// set up, the daemon logs api_listen_error and keeps running: nothing in the
// daemon depends on the API.
//
// Every request must name an allowed Host (localhost, 127.0.0.1 or [::1],
// with the listener's port on TCP), else it gets 421; this is the guard
// against DNS rebinding and against sandboxes reaching the API as
// host.docker.internal. Paths outside /api/ are the GUI: a small vanilla
// JS/CSS bundle under web/, embedded with embed.FS (no JS build step), served
// without credentials, with index.html for every app route. A request under
// /api/ with a foreign Origin, or a Sec-Fetch-Site other than same-origin or
// none, gets 403 (no CORS). It must then carry `Authorization: Bearer
// <token>` or the wm_session cookie, else it gets 401. The token lives in
// ~/.workingman/api/token (0600), created on first start and managed with
// `orch api token`. A rotated token takes effect on the next request,
// without a daemon restart.
//
// Browser login (§A.6.2): `orch gui` posts to /login-codes with the token
// for a one-time code (60 s, single use) and opens /login#t=<code>. The page
// posts the code to /session (Origin and a JSON body required), which sets
// wm_session (HttpOnly, SameSite=Strict, Path=/), a secret distinct from the
// token that expires after 12 h idle, and returns the session's scopes
// (read, write, answer) and its CSRF token. A browser session's requests
// other than GET and HEAD need Origin and X-WM-CSRF equal to that token, and
// every such request, bearer or browser, needs Content-Type: application/json
// when it has a body (415 otherwise), so a cross-site form is never a
// request the server accepts. Sessions live in
// memory, so a daemon restart logs browsers out. The GUI's pages get a CSP of
// default-src 'self' with frame-ancestors 'none'; API responses get
// default-src 'none'; both get X-Frame-Options: DENY.
//
// Responses are JSON. Each carries an ETag, and a request whose
// If-None-Match matches gets 304. Lists use the envelope
// {"items": [...], "generated_at": ..., "cursor": <seq>} ([List]), and errors
// the shape {"error": {"code": ..., "message": ...}}. A list's cursor is
// read before its items are built, so the items are at least as new as it.
//
// GET /events is the live event stream (Server-Sent Events, §A.3). A [Hub]
// keeps the last [DefaultRingSize] events and fans them out; the daemon's
// publisher feeds it. Each event carries a per-process seq, and its SSE id
// is "<epoch>.<seq>", where the epoch is the daemon's start time. A client
// that reconnects with Last-Event-ID (or ?since=<seq>, a list's cursor)
// inside the ring gets hello{replay:"delta"} and every event it missed. A
// cursor older than the ring or from another epoch gets hello{replay:"reset"}
// and a reset frame. A client with no cursor gets hello{replay:"full"} and
// the current state as created/started events. ?project= and ?types= (glob
// patterns; the default is every type but audit) filter the stream. Quiet
// streams get a heartbeat every 15 s, and [Server.Close] ends every stream.
// List cursors and /status's cursor are the hub's seq, so a client can read
// a list and then follow events from its cursor without a gap. [Client] is a
// small reader for both, used by `orch status --api`.
//
// Read endpoints, all GET (and HEAD): /status and /snapshot; /projects,
// /projects/{id} and, under it, /tasks, /tasks/{name}, /graph, /intake,
// /intake/{name}, /pulls and /audit; /sessions, /sessions/{sid} and its
// /transcript (NDJSON, paged by byte offset) and /prompt; /sandboxes;
// /audit; /messages; /agents; /events. Besides those: GET and DELETE
// /session, POST /session (login) and POST /login-codes. The server parses
// and checks the query parameters (400 on a bad one) and maps [ErrNotFound]
// to 404 and [ErrUnavailable] to 503; the resources themselves come from
// [Resources].
//
// Write endpoints, done by the source's [Actions]: the create-only actions
// of §A.4.3, POST /projects (seed a work stream's project.md), POST
// /projects/{id}/intake (queue an intake request) and DELETE
// /projects/{id}/intake/{name} (withdraw a pending one); the project
// actions of §A.4.2, POST /projects/{id}/actions/{action} for each of
// [ProjectActions] (stop, start, wolf, review, cleanup, hide, show,
// catchup; not archive, which stays in the TUI), where those
// [IfMatchRequired] lists are a 428 without If-Match; the task actions of
// §A.4.4, POST /projects/{id}/tasks/{name}/actions/{action} for retry
// (If-Match required) and save-sandbox, and PATCH
// /projects/{id}/tasks/{name}, a merge-patch of description, depends_on,
// touches and areas only ([TaskPatch]; If-Match required, 422 when the edit
// would break the task graph); the host tabs of §A.4.6, open-session and
// open-shell ({"where": "orch"|"workspace"}) on a project and on a task,
// which open a tab in the daemon's terminal and write nothing; and the
// inbox answers of §A.4.5, POST /messages/{id}/answer and /dismiss. The
// project, task and intake writes need the write scope, the inbox answers
// the answer scope (403 without it; the bearer token has every scope).
// Every write follows §A.4.1:
// a JSON body with no unknown fields; If-Match, checked by the action
// against the resource's etag ([MatchETag]; 412 with the current resource on
// a mismatch); an optional Idempotency-Key, under which the first response is
// kept for 24 h per client and replayed to a retry (422 if the key is reused
// for another request; a 5xx is not kept); 202 with the written resource and
// its ETag, or 200 with "noop": true; errors in the shape above plus
// "current", with codes bad_request, not_found, conflict, invalid_state,
// precondition_failed, precondition_required, unprocessable, forbidden and
// unavailable;
// and an api_write (or
// api_write_refused) audit line with the action, path, etags and client.
//
// # Key types
//
//   - [Config] and [ParseConfig] — the --api / --api-listen flags.
//   - [Server] — the listener and its handlers, run by the daemon with
//     [Server.Start] and [Server.Close].
//   - [Source] — what the server reads, [Resources] included; implemented
//     by internal/daemon.
//   - [Actions], [WriteRequest], [WriteResult] — the write actions, which
//     the daemon's source implements too; [ProjectActionRequest],
//     [TaskActionRequest], [TaskPatch] and [AnswerRequest] are their
//     bodies, [ProjectActions] and [TaskActions] the actions served;
//     [ErrInvalid], [ErrConflict], [ErrInvalidState], [ErrPreconditionFailed],
//     [ErrPreconditionRequired], [ErrUnprocessable] and [WithCurrent] pick a
//     write's error response.
//   - [ProjectQuery], [AuditQuery], [SessionQuery], ... — the parsed query
//     parameters handed to [Resources].
//   - [EnsureToken], [ReadToken], [TokenPath] — the token file.
//   - [List] — the list envelope.
//   - [Hub], [Event], [NewEvent] — the event stream's ring, fan-out and
//     envelope.
//   - [Client], [ReadSSE] — a client for the API and its event stream;
//     [Client.LoginCode] mints a browser login code.
//   - [LoginCode], [SessionInfo] — the login flow's bodies.
//
// # Collaborators
//
// No internal imports. Used by internal/daemon (which implements [Source]
// over its snapshot builder and publishes the events) and cmd/orch (flags,
// `orch api token`, `orch status --api`, `orch gui`). See
// docs/proposals/workingman-gui.md §A.2, §A.3, §A.4 and §A.6.
package api
