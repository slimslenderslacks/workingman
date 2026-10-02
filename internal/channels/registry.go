package channels

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
)

// Factory builds a Channel of one transport type. name is the config
// instance name (the returned Channel's Name() must equal it); spec carries
// the type-specific Options; creds are the resolved secrets.
type Factory func(name string, spec ChannelConfig, creds Credentials) (Channel, error)

// ErrNotRunning is returned when sending through a channel that was never
// started, failed to start, or has been closed.
var ErrNotRunning = errors.New("channel not running")

// SendResult is the outcome of delivering to one route.
type SendResult struct {
	Route     Route
	MessageID string
	Err       error
}

// Registry owns the configured channels: it starts and stops them with the
// daemon context, and fans outbound messages out across them. A failing or
// panicking channel never prevents the others from starting, stopping, or
// receiving a message. Safe for concurrent use.
type Registry struct {
	mu        sync.Mutex
	order     []string // insertion order, for deterministic start/close
	channels  map[string]Channel
	running   map[string]bool
	routes    []Route
	closed    bool
	stopWatch context.CancelFunc
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{channels: map[string]Channel{}, running: map[string]bool{}}
}

// Build constructs a Registry from cfg: every enabled channel is created via
// factories[type] with its credentials resolved relative to baseDir, and the
// active routes are installed. All problems are reported together. Nothing is
// started.
func Build(cfg *Config, baseDir string, factories map[string]Factory) (*Registry, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	reg := NewRegistry()
	var errs []error
	for _, name := range cfg.ChannelNames() {
		spec := cfg.Channels[name]
		if !spec.IsEnabled() {
			continue
		}
		f, ok := factories[spec.Type]
		if !ok {
			errs = append(errs, fmt.Errorf("channels.%s.type: unknown channel type %q", name, spec.Type))
			continue
		}
		creds, err := cfg.ResolveCredentials(name, baseDir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ch, err := f(name, spec, creds)
		if err != nil {
			errs = append(errs, fmt.Errorf("channels.%s: %w", name, err))
			continue
		}
		if ch != nil && ch.Name() != name {
			errs = append(errs, fmt.Errorf("channels.%s: factory returned a channel with name %q, want %q", name, ch.Name(), name))
			continue
		}
		if err := reg.Add(ch); err != nil {
			errs = append(errs, fmt.Errorf("channels.%s: %w", name, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	reg.SetRoutes(cfg.ActiveRoutes())
	return reg, nil
}

// BuildFromFile loads the config at path ("" = default location; a missing
// default file yields an empty Registry) and calls Build. Secrets-file paths
// that are relative resolve against the config file's directory.
func BuildFromFile(path string, factories map[string]Factory) (*Registry, error) {
	resolved, err := resolveConfigPath(path)
	if err != nil {
		return nil, err
	}
	cfg, err := LoadOptional(resolved)
	if err != nil {
		return nil, err
	}
	return Build(cfg, filepath.Dir(resolved), factories)
}

// Add registers ch under its Name(). Names must be unique and non-empty, and
// channels cannot be added once the Registry has been closed.
func (r *Registry) Add(ch Channel) error {
	if ch == nil {
		return errors.New("channels: nil channel")
	}
	name := ch.Name()
	if name == "" {
		return errors.New("channels: channel has empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("channels: registry closed")
	}
	if _, dup := r.channels[name]; dup {
		return fmt.Errorf("channels: duplicate channel %q", name)
	}
	r.channels[name] = ch
	r.order = append(r.order, name)
	return nil
}

// SetRoutes replaces the topic routing table.
func (r *Registry) SetRoutes(routes []Route) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes = append([]Route(nil), routes...)
}

// Get returns the named channel.
func (r *Registry) Get(name string) (Channel, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch, ok := r.channels[name]
	return ch, ok
}

// Names returns the registered channel names in registration order.
func (r *Registry) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

// Start starts every channel, wiring inbound messages to h. A channel that
// fails to start is skipped (and cannot be sent to); the rest keep running
// and the failures are returned joined. When ctx is cancelled the Registry
// closes itself, so tying it to the daemon context stops all channels on
// shutdown. h may be nil to discard inbound messages.
func (r *Registry) Start(ctx context.Context, h InboundHandler) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("channels: registry closed")
	}
	names := append([]string(nil), r.order...)
	r.mu.Unlock()

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, name := range names {
		r.mu.Lock()
		ch, already := r.channels[name], r.running[name]
		r.mu.Unlock()
		if already {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := safely(func() error { return ch.Start(ctx, wrapHandler(name, h)) })
			if err != nil {
				// Release anything the channel half-built before failing.
				_ = safely(ch.Close)
				mu.Lock()
				errs = append(errs, fmt.Errorf("channels: start %s: %w", name, err))
				mu.Unlock()
				return
			}
			r.mu.Lock()
			r.running[name] = true
			r.mu.Unlock()
		}()
	}
	wg.Wait()

	watchCtx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	if r.stopWatch != nil {
		r.stopWatch()
	}
	r.stopWatch = cancel
	r.mu.Unlock()
	go func() {
		<-watchCtx.Done()
		if ctx.Err() != nil { // daemon context ended, not a replaced watcher
			_ = r.Close()
		}
	}()

	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return errors.Join(errs...)
}

// wrapHandler stamps the channel name onto inbound messages and recovers
// handler panics.
func wrapHandler(name string, h InboundHandler) InboundHandler {
	return func(ctx context.Context, msg InboundMessage) {
		if h == nil {
			return
		}
		if msg.Channel == "" {
			msg.Channel = name
		}
		defer func() { _ = recover() }()
		h(ctx, msg)
	}
}

// Close stops every running channel (in reverse start order) and marks the
// Registry closed. Close errors are joined; one channel failing to close does
// not stop the others. Idempotent.
func (r *Registry) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	if r.stopWatch != nil {
		r.stopWatch()
	}
	var toClose []Channel
	var names []string
	for i := len(r.order) - 1; i >= 0; i-- {
		n := r.order[i]
		if r.running[n] {
			toClose = append(toClose, r.channels[n])
			names = append(names, n)
		}
		r.running[n] = false
	}
	r.mu.Unlock()

	var errs []error
	for i, ch := range toClose {
		if err := safely(ch.Close); err != nil {
			errs = append(errs, fmt.Errorf("channels: close %s: %w", names[i], err))
		}
	}
	return errors.Join(errs...)
}

// Send delivers msg through the named channel.
func (r *Registry) Send(ctx context.Context, channel string, msg OutboundMessage) (string, error) {
	r.mu.Lock()
	ch, ok := r.channels[channel]
	running := r.running[channel]
	r.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("channels: unknown channel %q", channel)
	}
	if !running {
		return "", fmt.Errorf("channels: send via %s: %w", channel, ErrNotRunning)
	}
	var id string
	err := safely(func() (err error) { id, err = ch.Send(ctx, msg); return })
	if err != nil {
		return "", fmt.Errorf("channels: send via %s: %w", channel, err)
	}
	return id, nil
}

// RoutesFor returns the routes that receive a message with the given topic:
// those whose topic matches exactly, or — only if there are none — the
// wildcard ("*") routes.
func (r *Registry) RoutesFor(topic string) []Route {
	r.mu.Lock()
	defer r.mu.Unlock()
	var exact, wild []Route
	for _, rt := range r.routes {
		switch rt.Topic {
		case topic:
			exact = append(exact, rt)
		case WildcardTopic:
			wild = append(wild, rt)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return wild
}

// SendTopic delivers text to every route for topic, concurrently, so a slow
// or failing channel cannot hold up the others. It returns one SendResult per
// route (in route order) plus the failures joined; a topic with no routes is
// not an error and yields no results.
func (r *Registry) SendTopic(ctx context.Context, topic, text string) ([]SendResult, error) {
	routes := r.RoutesFor(topic)
	results := make([]SendResult, len(routes))
	var wg sync.WaitGroup
	for i, rt := range routes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := r.Send(ctx, rt.Channel, OutboundMessage{ChatID: rt.Chat, Text: text, Topic: topic})
			results[i] = SendResult{Route: rt, MessageID: id, Err: err}
		}()
	}
	wg.Wait()

	var errs []error
	for _, res := range results {
		if res.Err != nil {
			errs = append(errs, res.Err)
		}
	}
	return results, errors.Join(errs...)
}

// safely runs fn, converting a panic into an error.
func safely(fn func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return fn()
}
