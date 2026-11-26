package daemon

import (
	"context"
	"time"

	"github.com/slimslenderslacks/work/internal/api"
)

// apiShutdownGrace is how long shutdown waits for API requests in flight.
const apiShutdownGrace = 2 * time.Second

// WithAPI enables the optional HTTP API (internal/api) with cfg.
// A disabled cfg (--api=off) leaves the daemon without one. The listener is
// started by Run and closed by its shutdown; a failure to bind is logged as
// api_listen_error and never stops the daemon.
func WithAPI(cfg api.Config) Option {
	return func(d *Daemon) {
		if !cfg.Enabled() {
			d.api, d.events = nil, nil
			return
		}
		d.events = newEventPublisher(d)
		srv := api.New(cfg, apiSource{d})
		srv.Log = d.audit.Log
		d.api = srv
	}
}

// API returns the daemon's API server, or nil when the API is off.
func (d *Daemon) API() *api.Server { return d.api }

// startAPI binds the API listener, if one is configured. A bind failure is
// already in the audit log (api_listen_error) and the daemon carries on
// without the API.
func (d *Daemon) startAPI() {
	if d.api != nil {
		_ = d.api.Start()
	}
}

// stopAPI closes the API listener, if one is running.
func (d *Daemon) stopAPI() {
	if d.api == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), apiShutdownGrace)
	defer cancel()
	if err := d.api.Close(ctx); err != nil {
		d.audit.Log("api_stop_error", "err", err.Error())
	}
}

// apiSource serves the API's reads from the snapshot builder, so the API
// and snapshot.json never disagree, and its writes through internal/control
// (api_writes.go).
type apiSource struct{ d *Daemon }

func (s apiSource) DaemonInfo() any {
	info := s.d.daemonInfo()
	redactStrings(info)
	return info
}

func (s apiSource) Snapshot(auditEvents int) any {
	return s.d.buildSnapshotN(true, auditEvents)
}

// Cursor is the event stream's seq. A list read before the first pass (or
// without a publisher) has cursor 0, which a client resumes from with a
// reset if the ring has moved past it.
func (s apiSource) Cursor() uint64 {
	if s.d.events == nil {
		return 0
	}
	return s.d.events.hub.Cursor()
}

func (s apiSource) Events() *api.Hub {
	if s.d.events == nil {
		return nil
	}
	return s.d.events.hub
}
