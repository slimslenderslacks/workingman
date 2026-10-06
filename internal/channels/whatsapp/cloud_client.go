package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
)

// Defaults for CloudConfig.
const (
	DefaultAPIVersion   = "v20.0"
	DefaultGraphBaseURL = "https://graph.facebook.com"
)

// Graph error codes this client treats specially.
const (
	// GraphCodeReEngagement ("Re-engagement message") is returned when a
	// free-form message is sent more than 24 hours after the recipient's last
	// inbound message. See ErrOutsideServiceWindow.
	GraphCodeReEngagement = 131047
	// GraphCodeInvalidParameter is returned for, among other things, a
	// read/typing receipt on a wamid older than 30 days.
	GraphCodeInvalidParameter = 131009
	// GraphCodeAuth is an expired or invalid access token (OAuthException).
	GraphCodeAuth = 190
)

// ErrOutsideServiceWindow matches (errors.Is) the failure Meta returns when a
// free-form message is sent outside the 24-hour customer-service window.
//
// LIMITATION: the Cloud API only accepts free-form messages (text, media,
// interactive) within 24 hours of the recipient's last inbound message. Past
// that, the only thing it accepts is a pre-approved message template, and the
// send fails with Graph error 131047. A daemon that notifies a phone it has not
// heard from in a day (a wolf agent starting overnight, say) will hit this.
// This client does not send templates; it reports the condition as a typed
// error so the caller can fall back to a template, to another route, or ask
// the user to message the number first to reopen the window. hermes-agent has
// the same limitation (website/docs/user-guide/messaging/whatsapp-cloud.md,
// "24-hour conversation window").
var ErrOutsideServiceWindow = errors.New("whatsapp: outside the 24-hour customer-service window; a template message or a new inbound message is required")

// ErrEmptyMessage is returned for text with nothing in it.
var ErrEmptyMessage = errors.New("whatsapp: empty message")

// ErrUnsupportedRecipient is returned for chat ids the Cloud API cannot
// address: groups, broadcasts and newsletters.
var ErrUnsupportedRecipient = errors.New("whatsapp: the Cloud API can only message individual users")

// IsOutsideServiceWindow reports whether err is the 24-hour-window failure.
func IsOutsideServiceWindow(err error) bool { return errors.Is(err, ErrOutsideServiceWindow) }

// GraphError is a non-2xx answer from the Graph API, parsed from the
// {"error": {"message", "type", "code", "error_subcode", "fbtrace_id"}} body.
// Every string in it has had credentials redacted.
type GraphError struct {
	Status    int    // HTTP status
	Code      int    // Graph error code; 0 when the body had none
	Subcode   int    // error_subcode; 0 when absent
	Type      string // e.g. "OAuthException"
	Message   string
	FBTraceID string
	// RetryAfter is the server's Retry-After, if it sent one.
	RetryAfter time.Duration
}

func (e *GraphError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("whatsapp: graph error %d (HTTP %d): %s", e.Code, e.Status, e.Message)
	}
	return fmt.Sprintf("whatsapp: HTTP %d: %s", e.Status, e.Message)
}

// Is makes errors.Is(err, ErrOutsideServiceWindow) work for code 131047.
func (e *GraphError) Is(target error) bool {
	return target == ErrOutsideServiceWindow && e.Code == GraphCodeReEngagement
}

// Retryable reports whether sending the same request again might succeed:
// HTTP 429, any 5xx, and Meta's rate-limit codes (which arrive on 400s).
func (e *GraphError) Retryable() bool {
	if e.Status == http.StatusTooManyRequests || e.Status >= 500 {
		return true
	}
	switch e.Code {
	case 4, 17, 32, 613, 80007, 130429, 131056: // app / user / pair rate limits
		return true
	}
	return false
}

// AuthFailure reports an expired or invalid access token. Temporary tokens
// from the Meta dashboard last 24 hours; use a System User permanent token.
func (e *GraphError) AuthFailure() bool {
	return e.Code == GraphCodeAuth || e.Status == http.StatusUnauthorized
}

// CloudConfig configures a CloudClient. Only PhoneNumberID and AccessToken are
// needed to send; the rest is carried for the webhook half of the channel.
type CloudConfig struct {
	// PhoneNumberID is the WhatsApp Business phone number's id (not the number
	// itself) from the Meta developer dashboard.
	PhoneNumberID string
	// AccessToken is the Graph bearer token. Held as a Secret so it cannot be
	// printed by accident.
	AccessToken channels.Secret
	// APIVersion is the Graph version path segment, e.g. "v20.0". Default DefaultAPIVersion.
	APIVersion string
	// GraphBaseURL overrides https://graph.facebook.com (tests point it at httptest).
	GraphBaseURL string

	// Used by the inbound webhook, not by sending:
	AppSecret   channels.Secret // verifies X-Hub-Signature-256
	WABAID      string          // WhatsApp Business Account id, to match webhook entries
	VerifyToken channels.Secret // answers Meta's GET hub.challenge handshake
}

var (
	versionRE = regexp.MustCompile(`^v\d+\.\d+$`)
	phoneIDRE = regexp.MustCompile(`^\d+$`)
)

// withDefaults returns cfg with the optional fields filled in and trimmed.
func (cfg CloudConfig) withDefaults() CloudConfig {
	cfg.PhoneNumberID = strings.TrimSpace(cfg.PhoneNumberID)
	cfg.WABAID = strings.TrimSpace(cfg.WABAID)
	cfg.APIVersion = strings.TrimSpace(cfg.APIVersion)
	if cfg.APIVersion == "" {
		cfg.APIVersion = DefaultAPIVersion
	}
	cfg.GraphBaseURL = strings.TrimRight(strings.TrimSpace(cfg.GraphBaseURL), "/")
	if cfg.GraphBaseURL == "" {
		cfg.GraphBaseURL = DefaultGraphBaseURL
	}
	return cfg
}

// Validate reports every problem with cfg at once. It never includes a secret
// value in its message.
func (cfg CloudConfig) Validate() error {
	cfg = cfg.withDefaults()
	var errs []error
	switch {
	case cfg.PhoneNumberID == "":
		errs = append(errs, errors.New("phone_number_id is required"))
	case !phoneIDRE.MatchString(cfg.PhoneNumberID):
		errs = append(errs, errors.New("phone_number_id must be the numeric id from the Meta dashboard, not the phone number"))
	}
	if cfg.AccessToken.Empty() {
		errs = append(errs, errors.New("access_token is required"))
	}
	if !versionRE.MatchString(cfg.APIVersion) {
		errs = append(errs, fmt.Errorf("api_version %q must look like v20.0", cfg.APIVersion))
	}
	if u, err := url.Parse(cfg.GraphBaseURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		errs = append(errs, fmt.Errorf("graph_base_url %q is not an http(s) URL", cfg.GraphBaseURL))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("whatsapp cloud config: %w", err)
	}
	return nil
}

// RetryPolicy bounds how a send retries retryable failures (see
// GraphError.Retryable): up to MaxAttempts tries in total, sleeping an
// exponentially growing delay (BaseDelay, 2x, 4x, … capped at MaxDelay,
// randomly shortened by up to Jitter) between them. A server Retry-After
// longer than MaxDelay is not waited out: the call fails right away with the
// GraphError so the caller decides.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Jitter      float64 // 0..1; fraction of the delay that may be shaved off at random
}

// DefaultRetryPolicy is 4 attempts over roughly 3.5 seconds.
var DefaultRetryPolicy = RetryPolicy{MaxAttempts: 4, BaseDelay: 500 * time.Millisecond, MaxDelay: 8 * time.Second, Jitter: 0.2}

// NoRetry makes every request a single attempt.
var NoRetry = RetryPolicy{MaxAttempts: 1}

func (p RetryPolicy) delay(retry int, serverHint time.Duration) time.Duration {
	d := p.BaseDelay
	for i := 1; i < retry && d < p.MaxDelay; i++ {
		d *= 2
	}
	if p.MaxDelay > 0 && d > p.MaxDelay {
		d = p.MaxDelay
	}
	if serverHint > d {
		d = serverHint
	}
	if p.Jitter > 0 && p.Jitter <= 1 {
		d -= time.Duration(float64(d) * p.Jitter * rand.Float64())
	}
	return d
}

// CloudOption customizes a CloudClient.
type CloudOption func(*CloudClient)

// WithCloudHTTPClient replaces the default http.Client (30s timeout).
func WithCloudHTTPClient(hc *http.Client) CloudOption { return func(c *CloudClient) { c.http = hc } }

// WithCloudLogger sets the logger. Messages and tokens are never logged.
func WithCloudLogger(l *slog.Logger) CloudOption {
	return func(c *CloudClient) {
		if l != nil {
			c.log = l
		}
	}
}

// WithRetryPolicy overrides DefaultRetryPolicy.
func WithRetryPolicy(p RetryPolicy) CloudOption { return func(c *CloudClient) { c.retry = p } }

// CloudClient sends messages through the WhatsApp Cloud API. It is safe for
// concurrent use. The access token travels only in the Authorization header
// and is scrubbed from every error and log line this type produces.
type CloudClient struct {
	cfg   CloudConfig
	http  *http.Client
	log   *slog.Logger
	retry RetryPolicy
	redac *redactor

	// sleep waits between retries; false means ctx ended first. Tests replace it.
	sleep func(ctx context.Context, d time.Duration) bool
}

// maxResponseBytes caps how much of a Graph response is read.
const maxResponseBytes = 1 << 20

// NewCloudClient validates cfg and returns a client.
func NewCloudClient(cfg CloudConfig, opts ...CloudOption) (*CloudClient, error) {
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	c := &CloudClient{
		cfg:   cfg,
		http:  &http.Client{Timeout: 30 * time.Second},
		log:   slog.Default(),
		retry: DefaultRetryPolicy,
		sleep: sleepCtx,
		redac: newRedactor(cfg.AccessToken, cfg.AppSecret, cfg.VerifyToken),
	}
	for _, o := range opts {
		o(c)
	}
	if c.retry.MaxAttempts < 1 {
		c.retry.MaxAttempts = 1
	}
	c.log = c.log.With("component", "whatsapp-cloud", "phone_number_id", cfg.PhoneNumberID)
	return c, nil
}

// Config returns the client's configuration with defaults applied.
func (c *CloudClient) Config() CloudConfig { return c.cfg }

// Redact scrubs the client's credentials (and anything shaped like a bearer
// token) from s. Use it on any string derived from a request or response
// before it is logged or returned.
func (c *CloudClient) Redact(s string) string { return c.redac.redact(s) }

// graphURL builds {base}/{version}/{phone_number_id}/{path}.
func (c *CloudClient) graphURL(path string) string {
	return c.cfg.GraphBaseURL + "/" + c.cfg.APIVersion + "/" + c.cfg.PhoneNumberID + "/" + strings.TrimPrefix(path, "/")
}

// SendText delivers text to the user `to` and returns the wamid of every
// message it became.
//
// Markdown is converted to WhatsApp markup (FormatMessage), then split under
// MaxMessageLength on paragraph, line, then word boundaries (ChunkMessage);
// each chunk is its own message. replyToWamid, if set, quotes that message on
// the first chunk only.
//
// If a later chunk fails, the wamids already delivered are returned alongside
// the error, so the caller knows the recipient saw a partial message. A
// failure because the 24-hour window is closed satisfies
// errors.Is(err, ErrOutsideServiceWindow).
func (c *CloudClient) SendText(ctx context.Context, to, text, replyToWamid string) ([]string, error) {
	recipient, err := cloudRecipient(to)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		return nil, ErrEmptyMessage
	}
	var wamids []string
	for i, chunk := range ChunkMessage(FormatMessage(text), MaxMessageLength) {
		payload := textPayload(recipient, chunk)
		if i == 0 && replyToWamid != "" {
			payload["context"] = map[string]string{"message_id": replyToWamid}
		}
		ids, err := c.post(ctx, payload, true)
		wamids = append(wamids, ids...)
		if err != nil {
			c.log.Warn("send failed", "to", RedactID(recipient), "chunk", i+1, "delivered", len(wamids), "error", err)
			return wamids, err
		}
	}
	return wamids, nil
}

func textPayload(to, body string) map[string]any {
	return map[string]any{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                to,
		"type":              "text",
		"text":              map[string]any{"body": body, "preview_url": true},
	}
}

// MarkRead marks an inbound message as read (the blue ticks). Best effort:
// one attempt, and the caller is expected to ignore the error. Meta rejects
// wamids older than 30 days (GraphCodeInvalidParameter).
func (c *CloudClient) MarkRead(ctx context.Context, wamid string) error {
	return c.receipt(ctx, wamid, false)
}

// Typing shows the typing indicator against the user's inbound message wamid.
// Meta couples it with a read receipt in a single call, so this also marks the
// message read. The indicator clears when a reply is sent or after 25 seconds.
// Best effort: one attempt, no retries.
func (c *CloudClient) Typing(ctx context.Context, wamid string) error {
	return c.receipt(ctx, wamid, true)
}

func (c *CloudClient) receipt(ctx context.Context, wamid string, typing bool) error {
	if strings.TrimSpace(wamid) == "" {
		return errors.New("whatsapp: message id required")
	}
	payload := map[string]any{"messaging_product": "whatsapp", "status": "read", "message_id": wamid}
	if typing {
		payload["typing_indicator"] = map[string]string{"type": "text"}
	}
	_, err := c.post(ctx, payload, false)
	if err != nil {
		var ge *GraphError
		if errors.As(err, &ge) && ge.Code == GraphCodeInvalidParameter {
			c.log.Info("read receipt rejected; the message is probably older than 30 days")
		} else {
			c.log.Debug("read receipt failed", "error", err)
		}
	}
	return err
}

// post sends one /messages payload and returns the wamids in the response. With
// retry set, retryable failures are retried per the RetryPolicy.
func (c *CloudClient) post(ctx context.Context, payload map[string]any, retry bool) ([]string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: encode request: %w", err)
	}
	attempts := 1
	if retry {
		attempts = c.retry.MaxAttempts
	}
	for attempt := 1; ; attempt++ {
		ids, err := c.postOnce(ctx, body)
		if err == nil {
			return ids, nil
		}
		hint, again := retryable(err)
		if !again || attempt >= attempts || ctx.Err() != nil || (c.retry.MaxDelay > 0 && hint > c.retry.MaxDelay) {
			return nil, err
		}
		wait := c.retry.delay(attempt, hint)
		c.log.Warn("send attempt failed; retrying", "attempt", attempt, "of", attempts, "in", wait, "error", err)
		if !c.sleep(ctx, wait) {
			return nil, ctx.Err()
		}
	}
}

// retryable classifies err. Graph errors defer to GraphError.Retryable. A
// transport error is retried only if the connection was never made: a request
// that may have reached Meta (timeout, reset mid-response) is not repeated,
// because that could deliver the message twice.
func retryable(err error) (hint time.Duration, ok bool) {
	var ge *GraphError
	if errors.As(err, &ge) {
		return ge.RetryAfter, ge.Retryable()
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return 0, true
	}
	return 0, false
}

func (c *CloudClient) postOnce(ctx context.Context, body []byte) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.graphURL("messages"), bytes.NewReader(body))
	if err != nil {
		return nil, c.wrapTransport(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.AccessToken.Reveal())
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.wrapTransport(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, c.wrapTransport(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, c.graphError(resp, raw)
	}
	var out struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		// Accepted but unreadable: the message was sent, we just have no id.
		c.log.Warn("unparseable success response", "status", resp.StatusCode)
		return nil, nil
	}
	ids := make([]string, 0, len(out.Messages))
	for _, m := range out.Messages {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// wrapTransport redacts a transport error. The error chain is kept (so
// context cancellation and net.OpError checks still work) but its text is
// scrubbed.
func (c *CloudClient) wrapTransport(err error) error {
	return &redactedError{msg: "whatsapp: request failed: " + c.Redact(err.Error()), err: err}
}

// redactedError carries a scrubbed message over an unscrubbed cause.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// graphError parses a non-2xx Graph response.
func (c *CloudClient) graphError(resp *http.Response, raw []byte) *GraphError {
	ge := &GraphError{Status: resp.StatusCode}
	var body struct {
		Error struct {
			Message   string `json:"message"`
			Type      string `json:"type"`
			Code      int    `json:"code"`
			Subcode   int    `json:"error_subcode"`
			FBTraceID string `json:"fbtrace_id"`
			Data      struct {
				Details string `json:"details"`
			} `json:"error_data"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil && (body.Error.Message != "" || body.Error.Code != 0) {
		e := body.Error
		ge.Code, ge.Subcode, ge.Type, ge.FBTraceID = e.Code, e.Subcode, e.Type, e.FBTraceID
		ge.Message = e.Message
		if e.Data.Details != "" {
			ge.Message += " (" + e.Data.Details + ")"
		}
	} else {
		ge.Message = truncateRunes(strings.TrimSpace(string(raw)), 500)
		if ge.Message == "" {
			ge.Message = http.StatusText(resp.StatusCode)
		}
	}
	ge.Message = c.Redact(ge.Message)
	ge.Type = c.Redact(ge.Type)
	ge.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	return ge
}

// parseRetryAfter reads delta-seconds or an HTTP date; 0 for anything else.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return max(0, time.Duration(secs)*time.Second)
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(0, t.Sub(now))
	}
	return 0
}

// cloudRecipient reduces a chat id to the digits-only wa_id the Cloud API
// wants ("+1 (555) 123-4567", "15551234567@s.whatsapp.net" -> "15551234567").
func cloudRecipient(to string) (string, error) {
	switch KindOf(to) {
	case KindGroup, KindBroadcast:
		return "", ErrUnsupportedRecipient
	}
	id := digitsOnly(NormalizeID(to))
	if id == "" {
		return "", fmt.Errorf("whatsapp: %q is not a phone number", to)
	}
	return id, nil
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// redactor scrubs credentials from text. It knows the configured secrets
// exactly, and also recognises bearer-token shapes (Graph error messages and
// proxies sometimes echo a token the config never held, e.g. an old one).
type redactor struct{ secrets []string }

var tokenShapes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`(?i)(access_token=)[^&\s"']+`),
	regexp.MustCompile(`()\bEA[A-Za-z0-9]{20,}`), // Meta access tokens start "EA…"
}

func newRedactor(secrets ...channels.Secret) *redactor {
	r := &redactor{}
	for _, s := range secrets {
		if v := s.Reveal(); len(v) >= 4 { // shorter values would shred ordinary text
			r.secrets = append(r.secrets, v)
		}
	}
	return r
}

const redactedText = "[REDACTED]"

func (r *redactor) redact(s string) string {
	for _, sec := range r.secrets {
		s = strings.ReplaceAll(s, sec, redactedText)
		// A URL-escaped copy shows up when the token rides in a query string.
		if esc := url.QueryEscape(sec); esc != sec {
			s = strings.ReplaceAll(s, esc, redactedText)
		}
	}
	for _, re := range tokenShapes {
		s = re.ReplaceAllString(s, "${1}"+redactedText)
	}
	return s
}
