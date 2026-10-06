package whatsapp

import (
	"container/list"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
)

// Webhook defaults, mirroring hermes-agent's whatsapp_cloud.py
// (DEFAULT_WEBHOOK_PORT / DEFAULT_WEBHOOK_PATH / WEBHOOK_MAX_BODY_BYTES /
// WAMID_DEDUP_CACHE_SIZE). The bind host differs on purpose: hermes listens on
// every interface, we listen on loopback only.
const (
	DefaultWebhookHost = "127.0.0.1"
	DefaultWebhookPort = 8090
	DefaultWebhookPath = "/whatsapp/webhook"
	// HealthPath answers GET with a small status document.
	HealthPath = "/health"

	// WebhookMaxBodyBytes is Meta's documented maximum payload size.
	WebhookMaxBodyBytes = 3 << 20
	// WamidDedupCacheSize bounds the FIFO of already-seen message ids.
	WamidDedupCacheSize = 5000

	// webhookQueueSize is how many acknowledged payloads may wait for the
	// worker before the webhook sheds load with a 503 (Meta retries those).
	webhookQueueSize = 256
	// shutdownGrace is how long Close waits for in-flight requests.
	shutdownGrace = 5 * time.Second
)

// Webhook option keys, under `options:` in the channel config.
const (
	OptWebhookHost           = "webhook_host"
	OptWebhookPort           = "webhook_port"
	OptWebhookPath           = "webhook_path"
	OptInsecureSkipSignature = "insecure_skip_signature"
)

const (
	signatureHeader = "X-Hub-Signature-256"
	signaturePrefix = "sha256="
	wabaObject      = "whatsapp_business_account"
	messagesField   = "messages"
	// maxCaptionRunes caps a media caption carried into the placeholder text.
	maxCaptionRunes = 1000
)

// WebhookConfig configures the inbound half of the Cloud channel: the HTTP
// server Meta delivers messages to.
//
//	channels:
//	  whatsapp:
//	    type: whatsapp
//	    credentials:
//	      access_token: {env: WHATSAPP_ACCESS_TOKEN}
//	      app_secret:   {env: WHATSAPP_APP_SECRET}     # required for the webhook
//	      verify_token: {env: WHATSAPP_VERIFY_TOKEN}   # the hub.challenge handshake
//	    options:
//	      phone_number_id: "109876543210987"
//	      # waba_id: "123456789"                # also match the WhatsApp Business Account
//	      # webhook_host: 127.0.0.1             # default; loopback only
//	      # webhook_port: 8090
//	      # webhook_path: /whatsapp/webhook
//	      # insecure_skip_signature: true       # DANGEROUS: accept unsigned POSTs
//
// The server binds loopback by default and is not meant to face the internet
// directly. Meta needs an https URL it can reach, so front the port with a
// tunnel (cloudflared, ngrok, tailscale funnel, ...) that forwards to
// http://127.0.0.1:8090/whatsapp/webhook, and register that public URL plus
// the verify token in the Meta app dashboard (WhatsApp > Configuration >
// Webhook, subscribed to the "messages" field).
type WebhookConfig struct {
	// Host is the bind address. Default 127.0.0.1.
	Host string
	// Port is the TCP port. Zero picks a free one (WebhookAddr reports it); the
	// config parser defaults an absent webhook_port to DefaultWebhookPort.
	Port int
	// Path is the URL path Meta calls. Default DefaultWebhookPath.
	Path string
	// InsecureSkipSignature lets the server run without an app secret, accepting
	// unauthenticated POSTs. With an app secret configured it has no effect:
	// signatures are always verified then.
	InsecureSkipSignature bool
}

func (w WebhookConfig) withDefaults() WebhookConfig {
	w.Host = strings.TrimSpace(w.Host)
	if w.Host == "" {
		w.Host = DefaultWebhookHost
	}
	w.Path = strings.TrimSpace(w.Path)
	if w.Path == "" {
		w.Path = DefaultWebhookPath
	}
	if !strings.HasPrefix(w.Path, "/") {
		w.Path = "/" + w.Path
	}
	return w
}

func (w WebhookConfig) validate() error {
	var errs []error
	if w.Port < 0 || w.Port > 65535 {
		errs = append(errs, fmt.Errorf("options.%s: %d is not a port", OptWebhookPort, w.Port))
	}
	if w.Path == HealthPath {
		errs = append(errs, fmt.Errorf("options.%s: %s is reserved for the health check", OptWebhookPath, HealthPath))
	}
	if strings.ContainsAny(w.Path, "?# ") {
		errs = append(errs, fmt.Errorf("options.%s: %q is not a plain URL path", OptWebhookPath, w.Path))
	}
	return errors.Join(errs...)
}

// WebhookConfigFromSpec reads the webhook options of a channel spec, applying
// the defaults (loopback, port 8090, /whatsapp/webhook).
func WebhookConfigFromSpec(options map[string]any) (WebhookConfig, error) {
	cfg := WebhookConfig{Port: DefaultWebhookPort}
	var errs []error
	var err error
	if cfg.Host, err = stringOption(options, OptWebhookHost); err != nil {
		errs = append(errs, err)
	}
	if cfg.Path, err = stringOption(options, OptWebhookPath); err != nil {
		errs = append(errs, err)
	}
	if raw, ok := options[OptWebhookPort]; ok && raw != nil {
		port, err := intOption(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("whatsapp: options.%s must be a port number", OptWebhookPort))
		} else {
			cfg.Port = port
		}
	}
	if raw, ok := options[OptInsecureSkipSignature]; ok && raw != nil {
		b, isBool := raw.(bool)
		if !isBool {
			errs = append(errs, fmt.Errorf("whatsapp: options.%s must be true or false", OptInsecureSkipSignature))
		}
		cfg.InsecureSkipSignature = b
	}
	if err := errors.Join(errs...); err != nil {
		return WebhookConfig{}, err
	}
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return WebhookConfig{}, fmt.Errorf("whatsapp: %w", err)
	}
	return cfg, nil
}

func intOption(raw any) (int, error) {
	switch v := raw.(type) {
	case int:
		return v, nil
	case int64:
		return int(v), nil
	case uint64:
		return int(v), nil
	case string:
		return strconv.Atoi(strings.TrimSpace(v))
	}
	return 0, errors.New("not a number")
}

// webhook is the running inbound server of a Channel.
type webhook struct {
	ch      *Channel
	cfg     WebhookConfig
	handler channels.InboundHandler

	srv   *http.Server
	ln    net.Listener
	queue chan cloudPayload // verified payloads awaiting the worker
	done  chan struct{}
	stop  context.CancelFunc

	dedup *wamidCache

	accepted, duplicates, rejectedSignature, queueFull atomic.Int64
}

// WebhookAddr is the address the webhook listens on ("" before Start, or when
// the channel has no webhook). Useful when webhook_port is 0.
func (c *Channel) WebhookAddr() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hook == nil {
		return ""
	}
	return c.hook.ln.Addr().String()
}

// startWebhook validates the configuration, binds the listener and starts
// serving. Bind errors are returned here, not logged from a goroutine. The
// server stops when ctx is cancelled or Close is called. Called with c.mu not held.
func (c *Channel) startWebhook(ctx context.Context, h channels.InboundHandler) (*webhook, error) {
	cfg := c.webhookCfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("whatsapp: %w", err)
	}
	if h == nil {
		return nil, errors.New("whatsapp: webhook needs an inbound handler")
	}
	if c.policy == nil {
		// Fail closed: an inbound server without an access policy would let
		// anyone who can sign a request drive the agent.
		return nil, errors.New("whatsapp: webhook needs an access policy")
	}
	appSecret := c.client.Config().AppSecret
	switch {
	case !appSecret.Empty():
	case cfg.InsecureSkipSignature:
		c.log.Warn("whatsapp: insecure_skip_signature is ENABLED and no app_secret is set: webhook POSTs are NOT authenticated; anyone who can reach the webhook can inject messages")
	default:
		return nil, fmt.Errorf("whatsapp: refusing to start the webhook without credentials.%s (it authenticates Meta's POSTs); set it, or set options.%s: true to accept unsigned requests",
			CredAppSecret, OptInsecureSkipSignature)
	}
	if c.client.Config().VerifyToken.Empty() {
		c.log.Warn("whatsapp: credentials.verify_token is not set; Meta's webhook subscription handshake will be refused until it is")
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		return nil, fmt.Errorf("whatsapp: webhook listen on %s:%d: %w", cfg.Host, cfg.Port, err)
	}
	if ip := net.ParseIP(cfg.Host); ip == nil || !ip.IsLoopback() {
		c.log.Warn("whatsapp: webhook is bound to a non-loopback address; expose it only through a trusted tunnel or proxy", "addr", ln.Addr().String())
	}

	runCtx, stop := context.WithCancel(ctx)
	wh := &webhook{
		ch:      c,
		cfg:     cfg,
		handler: h,
		ln:      ln,
		queue:   make(chan cloudPayload, webhookQueueSize),
		done:    make(chan struct{}),
		stop:    stop,
		dedup:   newWamidCache(WamidDedupCacheSize),
	}
	wh.srv = &http.Server{
		Handler:           http.HandlerFunc(wh.serveHTTP),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		wh.work(runCtx)
	}()
	go func() {
		err := wh.srv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			c.log.Error("whatsapp: webhook server failed", "err", err)
			stop()
		}
	}()
	go func() {
		defer close(wh.done)
		<-runCtx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := wh.srv.Shutdown(sctx); err != nil {
			_ = wh.srv.Close()
		}
		// Shutdown has returned, so no request can enqueue any more: let the
		// worker finish what was already acknowledged to Meta.
		close(wh.queue)
		workers.Wait()
	}()

	c.log.Info("whatsapp: webhook listening", "addr", ln.Addr().String(), "path", cfg.Path,
		"signature_check", !appSecret.Empty())
	return wh, nil
}

func (wh *webhook) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case HealthPath:
		wh.handleHealth(w, r)
	case wh.cfg.Path:
		switch r.Method {
		case http.MethodGet:
			wh.handleVerify(w, r)
		case http.MethodPost:
			wh.handleWebhook(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	default:
		http.NotFound(w, r)
	}
}

func (wh *webhook) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg := wh.ch.client.Config()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":                  "ok",
		"channel":                 wh.ch.name,
		"phone_number_id":         cfg.PhoneNumberID,
		"webhook_path":            wh.cfg.Path,
		"verify_token_configured": !cfg.VerifyToken.Empty(),
		"app_secret_configured":   !cfg.AppSecret.Empty(),
		"accepted":                wh.accepted.Load(),
		"duplicates":              wh.duplicates.Load(),
		"rejected_signature":      wh.rejectedSignature.Load(),
	})
}

// handleVerify answers Meta's subscription handshake: echo hub.challenge iff
// hub.mode is "subscribe" and hub.verify_token matches (constant-time).
func (wh *webhook) handleVerify(w http.ResponseWriter, r *http.Request) {
	token := wh.ch.client.Config().VerifyToken
	if token.Empty() {
		// Refuse rather than accept any token, which would let anyone subscribe.
		http.Error(w, "verify_token not configured", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	if q.Get("hub.mode") != "subscribe" {
		http.Error(w, "bad mode", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("hub.verify_token")), []byte(token.Reveal())) != 1 {
		http.Error(w, "verify_token mismatch", http.StatusForbidden)
		return
	}
	challenge := q.Get("hub.challenge")
	if challenge == "" {
		http.Error(w, "missing challenge", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, challenge)
}

// handleWebhook takes a delivery: bounded read, signature over the raw bytes,
// parse, then acknowledge and hand off. Everything after the 200 is
// the worker's: Meta retries non-200 answers for days, so a failure while
// processing must never surface as one.
func (wh *webhook) handleWebhook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, WebhookMaxBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if secret := wh.ch.client.Config().AppSecret; !secret.Empty() {
		if !verifySignature(secret.Reveal(), raw, r.Header.Get(signatureHeader)) {
			wh.rejectedSignature.Add(1)
			wh.ch.log.Warn("whatsapp: rejected webhook: missing or invalid "+signatureHeader, "body_len", len(raw))
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
	}
	var p cloudPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		wh.ch.log.Warn("whatsapp: webhook body is not a JSON object of the expected shape", "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	select {
	case wh.queue <- p:
		w.WriteHeader(http.StatusOK)
	default:
		wh.queueFull.Add(1)
		wh.ch.log.Warn("whatsapp: webhook queue full; asking Meta to retry")
		w.Header().Set("Retry-After", "5")
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}
}

// verifySignature checks a "sha256=<hex>" header against the HMAC-SHA256 of the
// raw body keyed by the app secret, in constant time.
func verifySignature(appSecret string, body []byte, header string) bool {
	header = strings.TrimSpace(header)
	hexSig, ok := strings.CutPrefix(header, signaturePrefix)
	if !ok {
		return false
	}
	got, err := hex.DecodeString(strings.TrimSpace(hexSig))
	if err != nil || len(got) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// work drains the queue in arrival order until it is closed. ctx cancels
// network calls made on a deny's behalf; the inbound handler gets a context
// that survives shutdown so messages Meta was already told about are delivered.
func (wh *webhook) work(ctx context.Context) {
	hctx := context.WithoutCancel(ctx)
	for p := range wh.queue {
		wh.process(ctx, hctx, p)
	}
}

func (wh *webhook) process(netCtx, handlerCtx context.Context, p cloudPayload) {
	defer func() {
		if r := recover(); r != nil {
			wh.ch.log.Error("whatsapp: webhook processing panicked", "panic", fmt.Sprint(r))
		}
	}()
	wh.dispatch(netCtx, handlerCtx, p)
}

// The subset of Meta's webhook payload this channel reads. Fields are
// json.RawMessage / loose where Meta's shapes vary, so one odd entry cannot
// make the whole delivery unreadable.
type (
	cloudPayload struct {
		Object string       `json:"object"`
		Entry  []cloudEntry `json:"entry"`
	}
	cloudEntry struct {
		ID      string        `json:"id"`
		Changes []cloudChange `json:"changes"`
	}
	cloudChange struct {
		Field string     `json:"field"`
		Value cloudValue `json:"value"`
	}
	cloudValue struct {
		Metadata struct {
			PhoneNumberID      string `json:"phone_number_id"`
			DisplayPhoneNumber string `json:"display_phone_number"`
		} `json:"metadata"`
		Contacts []struct {
			WAID    string `json:"wa_id"`
			Profile struct {
				Name string `json:"name"`
			} `json:"profile"`
		} `json:"contacts"`
		Messages []cloudMessage    `json:"messages"`
		Statuses []json.RawMessage `json:"statuses"`
	}
	cloudMessage struct {
		ID        string `json:"id"`
		From      string `json:"from"`
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Chat      any    `json:"chat"` // present on group-shaped payloads
		Context   struct {
			ID   string `json:"id"`
			From string `json:"from"`
		} `json:"context"`
		Text struct {
			Body string `json:"body"`
		} `json:"text"`
		Button struct {
			Text string `json:"text"`
		} `json:"button"`
		Interactive struct {
			ButtonReply struct {
				Title string `json:"title"`
			} `json:"button_reply"`
			ListReply struct {
				Title string `json:"title"`
			} `json:"list_reply"`
		} `json:"interactive"`
		raw map[string]json.RawMessage // the whole object, for media captions
	}
	mediaBlock struct {
		Caption string `json:"caption"`
	}
)

// UnmarshalJSON decodes the typed fields and keeps the raw object so media
// captions can be read from whichever key matches the message type.
func (m *cloudMessage) UnmarshalJSON(data []byte) error {
	type plain cloudMessage
	if err := json.Unmarshal(data, (*plain)(m)); err != nil {
		return err
	}
	return json.Unmarshal(data, &m.raw)
}

func (m *cloudMessage) caption(kind string) string {
	var b mediaBlock
	if raw, ok := m.raw[kind]; ok {
		_ = json.Unmarshal(raw, &b)
	}
	return b.Caption
}

// Message kinds. Contentless ones carry no user utterance and are dropped:
// "system" (number or user-id changes), "reaction" (an emoji tap),
// "unsupported" and "unknown" (payloads the Cloud API cannot render).
var contentlessKinds = map[string]bool{"system": true, "reaction": true, "unsupported": true, "unknown": true}

// mediaKinds are the non-text kinds. The daemon cannot read them, so the agent
// is handed a "[image]" style placeholder (plus any caption) and can say so.
var mediaKinds = map[string]bool{"image": true, "video": true, "audio": true, "voice": true, "document": true, "sticker": true, "location": true, "contacts": true}

// messageText extracts what the agent should read, or ok=false to drop the message.
func messageText(m *cloudMessage) (text string, ok bool) {
	kind := strings.ToLower(strings.TrimSpace(m.Type))
	if kind == "" {
		kind = "text"
	}
	switch {
	case contentlessKinds[kind]:
		return "", false
	case kind == "text":
		text = m.Text.Body
	case kind == "button":
		text = m.Button.Text
	case kind == "interactive":
		text = m.Interactive.ButtonReply.Title
		if text == "" {
			text = m.Interactive.ListReply.Title
		}
	case mediaKinds[kind]:
		text = "[" + kind + "]"
		if c := strings.TrimSpace(truncateRunes(m.caption(kind), maxCaptionRunes)); c != "" {
			text += " " + c
		}
	}
	if strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}

// dispatch walks entry[].changes[].value.{messages, contacts, statuses}.
// Statuses (sent/delivered/read/failed) are not consumed.
func (wh *webhook) dispatch(netCtx, handlerCtx context.Context, p cloudPayload) {
	log := wh.ch.log
	if p.Object != wabaObject {
		log.Debug("whatsapp: ignoring non-WABA webhook", "object", p.Object)
		return
	}
	cfg := wh.ch.client.Config()
	for _, entry := range p.Entry {
		for _, change := range entry.Changes {
			if change.Field != messagesField {
				continue // template_status_update, account_alerts, ...
			}
			v := change.Value
			if v.Metadata.PhoneNumberID != cfg.PhoneNumberID || (cfg.WABAID != "" && entry.ID != cfg.WABAID) {
				// A shared Meta app also delivers sibling numbers' receipts here;
				// only a foreign *message* is worth a warning.
				lvl := slog.LevelDebug
				if len(v.Messages) > 0 {
					lvl = slog.LevelWarn
				}
				log.Log(netCtx, lvl, "whatsapp: ignoring webhook for another number",
					"waba", entry.ID, "phone_number_id", v.Metadata.PhoneNumberID)
				continue
			}
			names := map[string]string{}
			for _, c := range v.Contacts {
				if id := strings.TrimSpace(c.WAID); id != "" {
					names[id] = strings.TrimSpace(c.Profile.Name)
				}
			}
			for i := range v.Messages {
				wh.ingest(netCtx, handlerCtx, &v.Messages[i], names)
			}
			for range v.Statuses {
				log.Debug("whatsapp: delivery status ignored")
			}
		}
	}
}

// ingest dedups, extracts, runs the access policy and finally the handler for
// one message. Nothing here may panic out or the rest of the batch is lost; the
// worker's recover is the backstop.
func (wh *webhook) ingest(netCtx, handlerCtx context.Context, m *cloudMessage, names map[string]string) {
	c := wh.ch
	wamid := strings.TrimSpace(m.ID)
	if !wh.dedup.firstSight(wamid) {
		wh.duplicates.Add(1)
		c.log.Debug("whatsapp: duplicate delivery skipped", "wamid", wamid)
		return
	}
	text, ok := messageText(m)
	if !ok {
		c.log.Debug("whatsapp: message has no readable content", "type", m.Type)
		return
	}
	from := strings.TrimSpace(m.From)
	if m.Chat != nil {
		// Cloud API DMs only; a group-shaped payload is refused, not treated as a DM.
		c.log.Warn("whatsapp: dropping group-shaped message; the Cloud API channel serves direct chats only", "sender", RedactID(from))
		return
	}
	in := Inbound{InboundMessage: channels.InboundMessage{
		Channel:    c.name,
		ChatID:     from,
		SenderID:   from,
		SenderName: names[from],
		MessageID:  wamid,
		ReplyToID:  strings.TrimSpace(m.Context.ID),
		Text:       text,
		Timestamp:  messageTime(m.Timestamp),
	}}
	d := c.policy.Decide(in)
	if !d.Allowed {
		if d.Reply != "" {
			// One rate-limited "not authorized" note to a stranger. Best effort,
			// off the worker so a slow Graph call cannot stall the queue.
			go func() {
				if _, err := c.client.SendText(netCtx, from, d.Reply, ""); err != nil {
					c.log.Debug("whatsapp: deny reply failed", "err", err)
				}
			}()
		}
		return
	}
	wh.accepted.Add(1)
	// After the policy, so refused senders never get typing or read receipts.
	c.NoteInbound(from, wamid)
	msg := in.InboundMessage
	msg.Text = d.Text
	// A handler bug costs this message, not the rest of the batch.
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("whatsapp: inbound handler panicked", "panic", fmt.Sprint(r))
		}
	}()
	wh.handler(handlerCtx, msg)
}

func messageTime(unix string) time.Time {
	if secs, err := strconv.ParseInt(strings.TrimSpace(unix), 10, 64); err == nil && secs > 0 {
		return time.Unix(secs, 0)
	}
	return time.Now()
}

// wamidCache is a bounded FIFO set of message ids.
type wamidCache struct {
	mu    sync.Mutex
	cap   int
	seen  map[string]struct{}
	order *list.List // oldest first; values are wamids
}

func newWamidCache(capacity int) *wamidCache {
	return &wamidCache{cap: capacity, seen: map[string]struct{}{}, order: list.New()}
}

// firstSight reports whether wamid is new, remembering it. A message without
// an id cannot be de-duplicated and is let through.
func (c *wamidCache) firstSight(wamid string) bool {
	if wamid == "" {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.seen[wamid]; dup {
		return false
	}
	c.seen[wamid] = struct{}{}
	c.order.PushBack(wamid)
	for c.order.Len() > c.cap {
		oldest := c.order.Front()
		c.order.Remove(oldest)
		delete(c.seen, oldest.Value.(string))
	}
	return true
}
