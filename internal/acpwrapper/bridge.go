package acpwrapper

import (
	"bufio"
	"io"
	"net"
	"sync"
)

// The agent.sock bridge multiplexes a single sandboxed ACP client's stdio
// across any number of connected TUI clients.
//
// ACP's wire format is newline-delimited JSON-RPC 2.0: each request, response,
// and session/update notification is one '\n'-terminated JSON object (see
// acp-kit's claude-acp-client entrypoint). The bridge is framed on those
// boundaries so two hazards of the naive byte-for-byte io.Copy can't happen:
//
//   - Fan-out corruption. The ACP client has exactly one stdout. If every
//     connection ran its own io.Copy(conn, stdout), several readers would race
//     on that single stream and each TUI would see an arbitrary, interleaved
//     fraction of every message. Instead ONE reader drains stdout, splits it
//     into whole frames, and copies each frame to every connected client — so
//     every watcher sees every complete message.
//   - Stdin interleave. Several TUIs share the one ACP client's stdin. If two
//     wrote concurrently their bytes could interleave mid-JSON-object and hand
//     the agent a corrupt line. The bridge reads each client's input into whole
//     frames and writes each frame to stdin under a mutex, so a frame from one
//     TUI is never split by a frame from another.
//
// Reconnection falls out of the fan-out: a watcher that connects later is just
// another client registered with the hub, and from that moment forward it
// receives every frame the agent streams. The live socket only carries the
// stream from "now"; replaying earlier scrollback is the job of the optional
// session log (session.Session.LogPath), not this transport.

// clientQueue bounds how many outbound frames the hub buffers for a single
// connected TUI before treating it as too slow to keep up. A watcher that falls
// this far behind is dropped (its connection closed) rather than being allowed
// to stall the shared fan-out for every other client; it can reconnect and
// resume from the live stream. The buffer absorbs normal bursts so a briefly
// busy TUI is not disconnected.
const clientQueue = 256

// hub fans one ACP client's stdout out to every connected TUI and serializes
// each TUI's framed input into the client's stdin.
type hub struct {
	// stdin is the ACP client's standard input — where TUI prompts/requests go.
	stdin io.Writer
	// writeMu serializes whole-frame writes to stdin so frames from different
	// clients never interleave on the shared stream.
	writeMu sync.Mutex

	// log, when non-nil, receives a copy of every agent stdout frame so a TUI
	// reconnecting after a restart can replay the session's prior output (the
	// live socket only carries the stream from "now"). Only run's single stdout
	// reader writes it, so no mutex is needed.
	log io.Writer

	// mu guards clients and closed. broadcast, registration, and teardown all
	// take it, so a client is never sent a frame after it has been removed.
	mu      sync.Mutex
	closed  bool
	clients map[*client]struct{}
}

// client is one connected TUI. Frames bound for it are queued on out and
// drained to conn by a dedicated writer goroutine, so a slow or blocked socket
// write never stalls the hub's fan-out.
type client struct {
	conn net.Conn
	out  chan []byte
	// once makes teardown idempotent: out is closed and conn shut exactly once
	// no matter which goroutine (reader, writer, broadcast-drop, shutdown)
	// retires the client first.
	once sync.Once
}

func newHub(stdin, log io.Writer) *hub {
	return &hub{stdin: stdin, log: log, clients: make(map[*client]struct{})}
}

// run is the single stdout fan-out reader. It frames the ACP client's stdout
// and broadcasts each frame to every connected client, blocking until stdout
// reaches EOF or errors (the ACP client has exited). On return it tears the hub
// down so no client is left dangling on a dead agent.
func (h *hub) run(stdout io.Reader) {
	scanFrames(stdout, func(frame []byte) bool {
		// Persist before broadcasting so a frame is never shown to a live client
		// without also being recorded for a later reconnect to replay. A log write
		// error is non-fatal: the live fan-out must not stall on a bad log.
		if h.log != nil {
			_, _ = h.log.Write(frame)
		}
		h.broadcast(frame)
		return true
	})
	h.shutdown()
}

// add registers a freshly accepted TUI connection and starts its reader and
// writer goroutines. If the hub has already shut down (the ACP client exited
// between Accept and here) the connection is closed immediately — the session's
// transport is gone, so there is nothing to bridge it to.
func (h *hub) add(conn net.Conn) {
	c := &client{conn: conn, out: make(chan []byte, clientQueue)}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		conn.Close()
		return
	}
	h.clients[c] = struct{}{}
	h.mu.Unlock()

	go h.clientWriter(c)
	go h.clientReader(c)
}

// broadcast copies frame to every connected client's queue. A client whose
// queue is full is lagging; it is dropped here rather than allowed to block the
// fan-out (and thereby every other watcher). frame is never mutated after
// framing, so sharing the same slice across all clients is safe.
func (h *hub) broadcast(frame []byte) {
	h.mu.Lock()
	for c := range h.clients {
		select {
		case c.out <- frame:
		default:
			h.dropLocked(c)
		}
	}
	h.mu.Unlock()
}

// clientReader frames the TUI's input and writes each whole frame to the ACP
// client's stdin under writeMu. It returns when the TUI hangs up or a stdin
// write fails (the agent is gone), retiring the client either way.
func (h *hub) clientReader(c *client) {
	scanFrames(c.conn, func(frame []byte) bool {
		h.writeMu.Lock()
		_, err := h.stdin.Write(frame)
		h.writeMu.Unlock()
		return err == nil
	})
	h.remove(c)
}

// clientWriter drains the client's queue to its socket, preserving frame
// boundaries (one Write per frame). It exits when the queue is closed (the
// client was retired) or a socket write fails, retiring the client on error.
func (h *hub) clientWriter(c *client) {
	for frame := range c.out {
		if _, err := c.conn.Write(frame); err != nil {
			h.remove(c)
			// Drain any remaining queued frames so a concurrent broadcast that
			// already enqueued doesn't block; the channel is closed by remove.
			for range c.out {
			}
			return
		}
	}
}

// remove retires a single client (idempotently). Safe to call from any
// goroutine; the actual close happens at most once.
func (h *hub) remove(c *client) {
	h.mu.Lock()
	h.dropLocked(c)
	h.mu.Unlock()
}

// dropLocked removes c from the client set and closes it. Caller holds h.mu, so
// it cannot race a broadcast that is mid-send to c: broadcast only sends to
// clients still in the map, and the close (which shuts c.out) happens here under
// the same lock after the delete.
func (h *hub) dropLocked(c *client) {
	if _, ok := h.clients[c]; !ok {
		return
	}
	delete(h.clients, c)
	c.close()
}

// shutdown retires every client and refuses future registrations. Called when
// the ACP client's stdout reaches EOF (agent exited) or the listener stops.
// Idempotent.
func (h *hub) shutdown() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	for c := range h.clients {
		delete(h.clients, c)
		c.close()
	}
	h.mu.Unlock()
}

// close shuts the client's queue and connection exactly once. Closing out
// signals clientWriter to stop; closing conn unblocks clientReader's pending
// read.
func (c *client) close() {
	c.once.Do(func() {
		close(c.out)
		c.conn.Close()
	})
}

// scanFrames reads newline-delimited frames from r and invokes onFrame for each
// complete frame, including its trailing '\n'. A bufio.Reader reassembles
// partial reads, so a frame split across several Read calls is delivered whole,
// and frames of any size are supported (no fixed line-length cap). A final,
// unterminated chunk at EOF is delivered too, so nothing the agent wrote before
// exiting is silently dropped. Scanning stops when r is exhausted/errors or
// onFrame returns false.
//
// Each returned frame is a freshly allocated slice (bufio.Reader.ReadBytes
// copies), so callers may retain or share it across goroutines without it being
// overwritten by the next read.
func scanFrames(r io.Reader, onFrame func(frame []byte) bool) {
	br := bufio.NewReader(r)
	for {
		frame, err := br.ReadBytes('\n')
		if len(frame) > 0 {
			if !onFrame(frame) {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
