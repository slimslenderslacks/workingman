package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/channels/signal"
	"github.com/slimslenderslacks/work/internal/channels/whatsapp"
	"github.com/slimslenderslacks/work/internal/daemon"
	"github.com/slimslenderslacks/work/internal/notify"
	"github.com/slimslenderslacks/work/internal/router"
	"github.com/slimslenderslacks/work/internal/runner"
)

// conversationIndexFile is the wolf-notification correlation table, kept in
// the daemon's state dir beside the snapshot.
const conversationIndexFile = "channel-conversations.json"

// daemonChannels is the messaging-channel wiring for one daemon run. The zero
// value (also what a missing config yields) is "channels disabled": the daemon
// is built with exactly the options it had before channels existed.
type daemonChannels struct {
	reg   *channels.Registry
	index channels.ConversationIndex
	cfg   *channels.Config
	log   *os.File
	// router is the inbound-message router, created by start once the daemon
	// exists; nil until then and when channels are disabled.
	router *router.Router
}

// enabled reports whether any channel is configured.
func (c *daemonChannels) enabled() bool { return c != nil && c.reg != nil }

// setupChannels loads the channels config at configPath ("" = default
// location) and builds — without starting — the Registry. A missing file, or
// one that enables no channel, disables channels and is not an error; an
// invalid config is, so a typo is not mistaken for "channels off".
func setupChannels(configPath, logDir, stateDir string, a *audit.Logger) (*daemonChannels, error) {
	resolved, err := channels.ResolveConfigPath(configPath)
	if err != nil {
		return nil, err
	}
	cfg, err := channels.LoadOptional(resolved)
	if err != nil {
		return nil, err
	}
	if len(cfg.ChannelNames()) == 0 {
		a.Log("channels_disabled", "config", resolved)
		return &daemonChannels{}, nil
	}

	logFile, err := os.OpenFile(filepath.Join(logDir, "channels.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open channels log: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(logFile, nil))
	factories := map[string]channels.Factory{
		"whatsapp": whatsapp.Factory(whatsapp.FactoryDeps{
			LogDir: logDir,
			Logger: logger,
			Cloud:  whatsapp.CloudFactory(logger),
		}),
		signal.Type: signal.Factory(logger),
	}
	reg, err := channels.Build(cfg, filepath.Dir(resolved), factories)
	if err != nil {
		_ = logFile.Close()
		return nil, err
	}
	if len(reg.Names()) == 0 { // every channel is `enabled: false`
		_ = logFile.Close()
		a.Log("channels_disabled", "config", resolved)
		return &daemonChannels{}, nil
	}

	c := &daemonChannels{reg: reg, cfg: cfg, log: logFile}
	indexPath := filepath.Join(stateDir, conversationIndexFile)
	idx, err := channels.NewFileConversationIndex(indexPath)
	if err != nil {
		// A corrupt table only costs reply routing for old notifications.
		a.Log("channel_index_error", "path", indexPath, "err", err.Error())
		idx, _ = channels.NewFileConversationIndex("")
	}
	c.index = idx
	a.Log("channels_configured", "config", resolved, "channels", fmt.Sprint(reg.Names()))
	return c, nil
}

// options returns the daemon options that wire channels in: the notifier
// becomes Multi(macOS, channels) — the channel half async so a slow network
// never stalls dispatch — and WithChannels carries the wolf lifecycle
// messages. Disabled channels keep the plain macOS notifier.
func (c *daemonChannels) options(a *audit.Logger) []daemon.Option {
	return c.optionsWithLocal(a, &notify.Osascript{})
}

// optionsWithLocal is options with the local (macOS) half of the notifier
// replaced, so a test can run the production wiring without pop-ups.
func (c *daemonChannels) optionsWithLocal(a *audit.Logger, local notify.Sender) []daemon.Option {
	if !c.enabled() {
		return []daemon.Option{daemon.WithNotifier(local)}
	}
	chNotifier := notify.NewAsync(channels.NewNotifier(c.reg), func(err error) {
		a.Log("channel_send_error", "event", "notify", "err", err.Error())
	})
	return []daemon.Option{
		daemon.WithNotifier(notify.NewMulti(local, chNotifier)),
		daemon.WithChannels(c.reg, c.index,
			daemon.WithWolfStartInterval(c.cfg.Notify.WolfStartIntervalDuration())),
	}
}

// start brings the channels up under the daemon context; they stop when it is
// cancelled. Inbound messages go to the router (see internal/router and
// docs/channels.md), which talks to the workingman agent and the live wolves
// through d. A channel that fails to start is skipped and audit-logged — the
// daemon carries on, and sends to it fail soft.
func (c *daemonChannels) start(ctx context.Context, a *audit.Logger, d *daemon.Daemon) {
	if !c.enabled() {
		return
	}
	rt, err := router.New(ctx, c.routerConfig(a, d))
	if err != nil {
		a.Log("router_error", "err", err.Error())
	}
	var handler channels.InboundHandler
	if rt != nil {
		c.router = rt
		handler = rt.Handler()
	}
	if err := c.reg.Start(ctx, handler); err != nil {
		a.Log("channel_start_error", "err", err.Error())
	}
}

// routerConfig is the inbound router's wiring: the daemon as its Source, the
// channel registry as its Outbound, and the timeouts from channels.yaml.
func (c *daemonChannels) routerConfig(a *audit.Logger, d *daemon.Daemon) router.Config {
	rc := c.cfg.Router
	return router.Config{
		Source:            daemonSource{d},
		Out:               c.reg,
		Index:             c.index,
		Audit:             a,
		PermissionTimeout: rc.PermissionTimeoutDuration(),
		TurnTimeout:       rc.TurnTimeoutDuration(),
		ThinkingAfter:     rc.ThinkingAfterDuration(),
		QueueSize:         rc.QueueSize,
	}
}

// daemonSource adapts the daemon to router.Source.
type daemonSource struct{ d *daemon.Daemon }

func (s daemonSource) WorkingmanAgent() (router.Session, bool) {
	ws, ok := s.d.WorkingmanAgentSession()
	if !ok {
		return router.Session{}, false
	}
	return router.Session{Key: runner.WorkingmanAgentSession, ID: ws.ID, Ref: sessionRef(ws.Dir, ws.ID)}, true
}

func (s daemonSource) WolfSessions() []router.Session {
	var out []router.Session
	for _, w := range s.d.WolfSessions() {
		sess := router.Session{Key: w.Key, ID: w.ID, WorkStream: w.WorkStream}
		if w.Attachable {
			sess.Ref = sessionRef(w.Dir, w.ID)
		}
		out = append(out, sess)
	}
	return out
}

// sessionRef is what acpchat.Attach takes: the session directory, or the bare
// session id (resolved under the sessions root) when the directory is unknown.
func sessionRef(dir, id string) string {
	if dir != "" {
		return dir
	}
	return id
}

func (s daemonSource) Status() string { return s.d.StatusSummary() }

// stop closes the channels (idempotent; they also close when the daemon
// context is cancelled).
func (c *daemonChannels) stop(a *audit.Logger) {
	if !c.enabled() {
		return
	}
	if err := c.reg.Close(); err != nil {
		a.Log("channel_close_error", "err", err.Error())
	}
	if c.router != nil {
		c.router.Close()
	}
	if c.log != nil {
		_ = c.log.Close()
	}
}

// stateDirFor is the daemon's state directory: where the snapshot lives by
// default, derived from the sessions root.
func stateDirFor(sessionsRoot string) (string, error) {
	p, err := daemon.DefaultStateFile(sessionsRoot)
	if err != nil {
		return "", err
	}
	return filepath.Dir(p), nil
}
