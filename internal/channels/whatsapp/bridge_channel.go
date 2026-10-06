package whatsapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/channels/whatsapp/bridge"
)

// BridgeLogName is the bridge process's log file inside FactoryDeps.LogDir.
const BridgeLogName = "whatsapp-bridge.log"

// credsFile is what Baileys writes into the session dir once a phone is linked.
const credsFile = "creds.json"

const (
	defaultPollWait    = 25 * time.Second
	defaultConnectWait = 30 * time.Second
	pollRetryInitial   = time.Second
	pollRetryMax       = 15 * time.Second
)

// BridgeChannel is the Baileys backend: the same channels.Channel as the Cloud
// API one, but over a personal number linked by QR code. It supervises the
// vendored Node bridge, long-polls its loopback HTTP API, and runs every
// inbound message through the AccessPolicy (including the self-chat owner gate
// and echo suppression) before the daemon sees it.
//
// Ban risk: Baileys drives an unofficial WhatsApp Web session. Use a number you
// can afford to lose, keep volume human-like, and read hermes-agent's
// website/docs/user-guide/messaging/whatsapp.md before pairing a primary number.
type BridgeChannel struct {
	name        string
	opts        BridgeOptions
	policy      *AccessPolicy
	log         *slog.Logger
	logPath     string
	selfChat    bool
	replyPrefix string

	// Seams for tests; the zero values do the real thing.
	preflight   func() (node string, err error)
	launcher    func(node string, port int, token string, logw io.Writer) bridge.Starter
	endpoint    string // base URL of an already-running (fake) bridge: skips launching
	token       string
	pollWait    time.Duration
	connectWait time.Duration
	backoff     bridge.Backoff

	mu     sync.Mutex
	state  bridgeState
	client *bridge.Client
	sup    *bridge.Supervisor
	cancel context.CancelFunc
	fatal  error
	logf   *os.File
	wg     sync.WaitGroup
}

type bridgeState int

const (
	stateNew bridgeState = iota
	stateRunning
	stateClosed
)

var _ channels.Channel = (*BridgeChannel)(nil)

// NewBridgeChannel builds the channel. access is the policy block from the
// channel options; its mode (bot or self-chat) decides who may talk to the
// daemon. logDir is where whatsapp-bridge.log goes; "" uses the bridge install
// dir's parent.
func NewBridgeChannel(name string, opts BridgeOptions, access AccessConfig, logDir string, logger *slog.Logger) (*BridgeChannel, error) {
	if logger == nil {
		logger = slog.Default()
	}
	policy, err := NewAccessPolicy(access,
		WithResolver(DirResolver{FS: os.DirFS(opts.SessionDir)}),
		WithLogger(logger))
	if err != nil {
		return nil, err
	}
	if logDir == "" {
		logDir = filepath.Dir(opts.InstallDir)
	}
	return &BridgeChannel{
		name:        name,
		opts:        opts,
		policy:      policy,
		log:         logger.With("channel", name),
		logPath:     filepath.Join(logDir, BridgeLogName),
		selfChat:    access.Mode == ModeSelfChat,
		replyPrefix: access.ReplyPrefix,
	}, nil
}

func newBridgeFromSpec(name string, spec channels.ChannelConfig, deps FactoryDeps) (channels.Channel, error) {
	opts, err := ParseBridgeOptions(spec.Options)
	if err != nil {
		return nil, err
	}
	access, err := ParseAccessOptions(spec.Options)
	if err != nil {
		return nil, err
	}
	return NewBridgeChannel(name, opts, access, deps.LogDir, deps.Logger)
}

// Name implements channels.Channel.
func (c *BridgeChannel) Name() string { return c.name }

// Policy exposes the access policy (for status reporting and tests).
func (c *BridgeChannel) Policy() *AccessPolicy { return c.policy }

// Status reports the bridge process's supervision state; the zero value before Start.
func (c *BridgeChannel) Status() bridge.SupervisorStatus {
	c.mu.Lock()
	sup := c.sup
	c.mu.Unlock()
	if sup == nil {
		return bridge.SupervisorStatus{}
	}
	return sup.Status()
}

// Health asks the running bridge for its state.
func (c *BridgeChannel) Health(ctx context.Context) (bridge.Health, error) {
	client, err := c.running()
	if err != nil {
		return bridge.Health{}, err
	}
	return client.Health(ctx)
}

// Restart makes the supervisor replace the bridge process immediately.
func (c *BridgeChannel) Restart() {
	c.mu.Lock()
	sup := c.sup
	c.mu.Unlock()
	if sup != nil {
		sup.Restart()
	}
}

// checkReady verifies everything that can be known without launching: node,
// the installed bridge, and a paired session. Errors are written for a person.
func (c *BridgeChannel) checkReady() (string, error) {
	if c.preflight != nil {
		return c.preflight()
	}
	node, err := bridge.FindNode(c.opts.Node)
	if err != nil {
		return "", err
	}
	if err := bridge.Extract(c.opts.InstallDir); err != nil {
		return "", err
	}
	if !bridge.Ready(c.opts.InstallDir) {
		return "", fmt.Errorf("whatsapp bridge dependencies are not installed in %s; run `orch whatsapp pair` (it installs them and links your phone)", c.opts.InstallDir)
	}
	if !Paired(c.opts.SessionDir) {
		return "", fmt.Errorf("whatsapp is not paired (no session in %s); run `orch whatsapp pair`", c.opts.SessionDir)
	}
	return node, nil
}

// Paired reports whether sessionDir holds linked-device credentials.
func Paired(sessionDir string) bool {
	fi, err := os.Stat(filepath.Join(sessionDir, credsFile))
	return err == nil && fi.Mode().IsRegular()
}

// Start launches the bridge and begins delivering admitted messages to h. It
// returns once the process is launched; the WhatsApp connection comes up in the
// background and Send waits for it. It fails fast, with a message saying what
// to fix, when node is missing or the number is not paired.
func (c *BridgeChannel) Start(ctx context.Context, h channels.InboundHandler) error {
	if h == nil {
		return errors.New("whatsapp: nil inbound handler")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case stateRunning:
		return errors.New("whatsapp: channel already started")
	case stateClosed:
		return errors.New("whatsapp: channel closed")
	}

	node, err := c.checkReady()
	if err != nil {
		return err
	}

	var starter bridge.Starter
	baseURL, token := c.endpoint, c.token
	if baseURL == "" {
		port := c.opts.Port
		if port == 0 {
			if port, err = freeLoopbackPort(); err != nil {
				return fmt.Errorf("whatsapp: pick a bridge port: %w", err)
			}
		}
		if token, err = randomToken(); err != nil {
			return err
		}
		baseURL = "http://127.0.0.1:" + strconv.Itoa(port)
		logw, err := bridge.OpenLog(c.logPath)
		if err != nil {
			return err
		}
		c.logf = logw
		if c.launcher != nil {
			starter = c.launcher(node, port, token, logw)
		} else {
			starter = c.execStarter(node, port, token, logw)
		}
	} else if c.launcher != nil {
		starter = c.launcher(node, 0, token, io.Discard)
	}
	client, err := bridge.NewClient(baseURL, token)
	if err != nil {
		c.closeLog()
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	c.client, c.cancel, c.state = client, cancel, stateRunning
	if starter != nil {
		c.sup = &bridge.Supervisor{Start: starter, Backoff: c.backoff, Log: c.log}
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			if err := c.sup.Run(runCtx); err != nil {
				c.mu.Lock()
				c.fatal = err
				c.mu.Unlock()
				cancel()
			}
		}()
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.pollLoop(runCtx, client, h)
	}()
	c.log.Info("whatsapp bridge channel started", "backend", string(BackendBridge), "mode", c.accessMode())
	return nil
}

func (c *BridgeChannel) accessMode() string {
	if c.selfChat {
		return string(ModeSelfChat)
	}
	return string(ModeBot)
}

func (c *BridgeChannel) execStarter(node string, port int, token string, logw io.Writer) bridge.Starter {
	return bridge.ExecStarter(bridge.Launch{
		Node: node,
		Dir:  c.opts.InstallDir,
		Args: []string{"--port", strconv.Itoa(port), "--session", c.opts.SessionDir},
		Env:  []string{"WORKINGMAN_BRIDGE_TOKEN=" + token},
		Log:  logw,
	})
}

func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("whatsapp: generate bridge token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func (c *BridgeChannel) closeLog() {
	if c.logf != nil {
		_ = c.logf.Close()
		c.logf = nil
	}
}

// running returns the client, or why there is none.
func (c *BridgeChannel) running() (*bridge.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fatal != nil {
		return nil, c.fatal
	}
	if c.state != stateRunning {
		return nil, channels.ErrNotRunning
	}
	return c.client, nil
}

// pollLoop long-polls the bridge until ctx ends. Transport errors (the bridge
// restarting, say) are retried with a capped backoff; a message already handed
// over is never re-read, so a crash loses at most the batch in flight.
func (c *BridgeChannel) pollLoop(ctx context.Context, client *bridge.Client, h channels.InboundHandler) {
	wait := c.pollWait
	if wait <= 0 {
		wait = defaultPollWait
	}
	retry := pollRetryInitial
	for ctx.Err() == nil {
		evs, err := client.Messages(ctx, wait)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.log.Debug("whatsapp bridge poll failed", "err", err, "retry_in", retry.String())
			if !sleepCtx(ctx, retry) {
				return
			}
			if retry *= 2; retry > pollRetryMax {
				retry = pollRetryMax
			}
			continue
		}
		retry = pollRetryInitial
		for _, ev := range evs {
			c.handleEvent(ctx, client, h, ev)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *BridgeChannel) handleEvent(ctx context.Context, client *bridge.Client, h channels.InboundHandler, ev bridge.Event) {
	in := Inbound{
		InboundMessage: channels.InboundMessage{
			Channel:    c.name,
			ChatID:     ev.ChatID,
			SenderID:   ev.SenderID,
			SenderName: ev.SenderName,
			MessageID:  ev.MessageID,
			ReplyToID:  ev.QuotedMessageID,
			Text:       ev.Body,
			Timestamp:  ev.Time(),
		},
		SenderAltID:       ev.SenderAltID,
		IsGroup:           ev.IsGroup,
		FromMe:            ev.FromMe,
		BotIDs:            ev.BotIDs,
		MentionedIDs:      ev.MentionedIDs,
		QuotedParticipant: ev.QuotedParticipant,
	}
	d := c.policy.Decide(in)
	if !d.Allowed {
		if d.Reply != "" {
			// A stranger's one rate-limited "not authorized" note. Best effort.
			if _, err := c.sendText(ctx, ToJID(ev.ChatID), d.Reply, ""); err != nil {
				c.log.Debug("whatsapp deny reply failed", "err", err)
			}
		}
		return
	}
	if c.opts.SendReadReceipts && !ev.FromMe && ev.ReadReceiptKey.ID != "" {
		if err := client.MarkRead(ctx, ev.ReadReceiptKey); err != nil {
			c.log.Debug("whatsapp read receipt failed", "err", err)
		}
	}
	msg := in.InboundMessage
	msg.Text = d.Text
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("whatsapp inbound handler panicked", "panic", fmt.Sprint(r))
		}
	}()
	h(ctx, msg)
}

// Send implements channels.Channel. A message sent right after Start waits
// (up to 30s) for the bridge to finish connecting rather than failing.
func (c *BridgeChannel) Send(ctx context.Context, msg channels.OutboundMessage) (string, error) {
	text := channels.FormatTopic(msg.Topic, msg.Text)
	if strings.TrimSpace(text) == "" {
		return "", errors.New("whatsapp: empty message")
	}
	jid := ToJID(msg.ChatID)
	if jid == "" {
		return "", errors.New("whatsapp: empty chat id")
	}
	return c.sendText(ctx, jid, text, msg.ReplyToID)
}

// Typing shows the typing indicator in chatID. Best effort by nature.
func (c *BridgeChannel) Typing(ctx context.Context, chatID string) error {
	client, err := c.running()
	if err != nil {
		return err
	}
	return client.Typing(ctx, ToJID(chatID))
}

func (c *BridgeChannel) sendText(ctx context.Context, jid, text, replyTo string) (string, error) {
	client, err := c.running()
	if err != nil {
		return "", err
	}
	// In self-chat the bot and the user share one number, so our replies need
	// a visible marker (and the policy a way to recognise them).
	if c.selfChat && c.replyPrefix != "" && !strings.HasPrefix(text, c.replyPrefix) {
		text = c.replyPrefix + text
	}
	res, err := client.Send(ctx, jid, text, replyTo)
	if err != nil && c.notYetConnected(err) {
		if werr := c.waitConnected(ctx, client); werr != nil {
			return "", werr
		}
		res, err = client.Send(ctx, jid, text, replyTo)
	}
	var ae *bridge.APIError
	if errors.As(err, &ae) {
		// Chunks delivered before a mid-send failure still echo back.
		for _, id := range ae.MessageIDs {
			c.policy.RecordSent(id)
		}
	}
	if err != nil {
		return "", err
	}
	for _, id := range res.MessageIDs {
		c.policy.RecordSent(id)
	}
	c.policy.RecordSent(res.MessageID)
	return res.MessageID, nil
}

// notYetConnected reports whether a send failed only because the bridge is
// still starting: a refused connection (no APIError) or 503 "Not connected".
func (c *BridgeChannel) notYetConnected(err error) bool {
	var ae *bridge.APIError
	if errors.As(err, &ae) {
		return ae.Status == 503
	}
	return true
}

// waitConnected polls /health until the bridge is linked and online.
func (c *BridgeChannel) waitConnected(ctx context.Context, client *bridge.Client) error {
	wait := c.connectWait
	if wait <= 0 {
		wait = defaultConnectWait
	}
	deadline := time.Now().Add(wait)
	var last error
	for {
		if _, err := c.running(); err != nil {
			return err // closed, or logged out while waiting
		}
		h, err := client.Health(ctx)
		if err == nil && h.Connected() {
			return nil
		}
		last = err
		if err == nil {
			last = errors.New("bridge is not connected to WhatsApp yet")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("whatsapp: bridge not connected after %s: %w", wait, last)
		}
		if !sleepCtx(ctx, 250*time.Millisecond) {
			return ctx.Err()
		}
	}
}

// Close stops polling and the bridge process and waits for both. Idempotent.
func (c *BridgeChannel) Close() error {
	c.mu.Lock()
	if c.state == stateClosed {
		c.mu.Unlock()
		return nil
	}
	wasRunning := c.state == stateRunning
	c.state = stateClosed
	cancel := c.cancel
	c.mu.Unlock()

	if wasRunning && cancel != nil {
		cancel()
		c.wg.Wait()
	}
	c.mu.Lock()
	c.closeLog()
	c.mu.Unlock()
	return nil
}
