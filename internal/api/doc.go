// Package api is the daemon's optional, read-only HTTP API: the listener, its
// lifecycle and authentication, and the response conventions every endpoint
// shares.
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
// host.docker.internal. It must then carry `Authorization: Bearer <token>`,
// else it gets 401. The token lives in ~/.workingman/api/token (0600),
// created on first start and managed with `orch api token`. A rotated token
// takes effect on the next request, without a daemon restart.
//
// Responses are JSON. Each carries an ETag, and a request whose
// If-None-Match matches gets 304. Lists use the envelope
// {"items": [...], "generated_at": ..., "cursor": <seq>} ([List]), and errors
// the shape {"error": {"code": ..., "message": ...}}. A list's cursor is
// read before its items are built, so the items are at least as new as it.
//
// Endpoints, all GET (and HEAD): /status and /snapshot; /projects,
// /projects/{id} and, under it, /tasks, /tasks/{name}, /graph, /intake,
// /intake/{name}, /pulls and /audit; /sessions, /sessions/{sid} and its
// /transcript (NDJSON, paged by byte offset) and /prompt; /sandboxes;
// /audit; /messages; /agents. The server parses and checks the query
// parameters (400 on a bad one) and maps [ErrNotFound] to 404 and
// [ErrUnavailable] to 503; the resources themselves come from [Resources].
//
// # Key types
//
//   - [Config] and [ParseConfig] — the --api / --api-listen flags.
//   - [Server] — the listener and its handlers, run by the daemon with
//     [Server.Start] and [Server.Close].
//   - [Source] — what the server reads, [Resources] included; implemented
//     by internal/daemon.
//   - [ProjectQuery], [AuditQuery], [SessionQuery], ... — the parsed query
//     parameters handed to [Resources].
//   - [EnsureToken], [ReadToken], [TokenPath] — the token file.
//   - [List] — the list envelope.
//
// # Collaborators
//
// No internal imports. Used by internal/daemon (which implements [Source]
// over its snapshot builder) and cmd/orch (flags, `orch api token`). See
// docs/proposals/workingman-gui.md §A.2 and §A.6.
package api
