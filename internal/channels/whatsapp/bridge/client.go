package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Event is one inbound message as the bridge reports it (see helpers.js
// buildEvent). It is raw transport data: no policy has been applied.
type Event struct {
	MessageID         string      `json:"messageId"`
	ChatID            string      `json:"chatId"`
	SenderID          string      `json:"senderId"`
	SenderAltID       string      `json:"senderAltId"`
	SenderName        string      `json:"senderName"`
	IsGroup           bool        `json:"isGroup"`
	FromMe            bool        `json:"fromMe"`
	Body              string      `json:"body"`
	MediaType         string      `json:"mediaType"`
	MentionedIDs      []string    `json:"mentionedIds"`
	QuotedMessageID   string      `json:"quotedMessageId"`
	QuotedParticipant string      `json:"quotedParticipant"`
	BotIDs            []string    `json:"botIds"`
	ReadReceiptKey    ReadKey     `json:"readReceiptKey"`
	Timestamp         json.Number `json:"timestamp"`
}

// Time returns the event timestamp (unix seconds), or the zero time.
func (e Event) Time() time.Time {
	if n, err := strconv.ParseInt(e.Timestamp.String(), 10, 64); err == nil && n > 0 {
		return time.Unix(n, 0)
	}
	return time.Time{}
}

// ReadKey identifies a message for a read receipt.
type ReadKey struct {
	RemoteJID   string `json:"remoteJid"`
	ID          string `json:"id"`
	Participant string `json:"participant,omitempty"`
	FromMe      bool   `json:"fromMe"`
}

// SendResult is what the bridge reports for a send. A long message is split,
// so there may be several ids; MessageID is the first.
type SendResult struct {
	MessageID  string   `json:"messageId"`
	MessageIDs []string `json:"messageIds"`
}

// Health is the bridge's /health payload.
type Health struct {
	Status      string  `json:"status"` // "connected" | "disconnected"
	QueueLength int     `json:"queueLength"`
	Uptime      float64 `json:"uptime"`
	ScriptHash  string  `json:"scriptHash"`
	User        struct {
		ID   string `json:"id"`
		LID  string `json:"lid"`
		Name string `json:"name"`
	} `json:"user"`
}

// Connected reports whether the bridge is linked and online.
func (h Health) Connected() bool { return h.Status == "connected" }

// APIError is a non-2xx answer from the bridge. MessageIDs lists any chunks of
// a split send that were delivered before the failure.
type APIError struct {
	Status     int
	Message    string
	MessageIDs []string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("whatsapp bridge: HTTP %d: %s", e.Status, e.Message)
}

// Client talks to one bridge. The base URL must be loopback: the bridge is a
// local component with no business being reached over a network.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// NewClient validates baseURL and returns a Client that authenticates with
// token (empty = no Authorization header).
func NewClient(baseURL, token string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("bridge: bad URL %q: %w", baseURL, err)
	}
	if u.Scheme != "http" || !isLoopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("bridge: URL %q must be http on a loopback address", baseURL)
	}
	return &Client{
		base:  u,
		token: token,
		// No proxy: loopback traffic must never leave the machine, whatever
		// HTTP_PROXY says.
		http: &http.Client{Transport: &http.Transport{Proxy: nil}},
	}, nil
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// maxResponse bounds how much of a bridge response is read.
const maxResponse = 8 << 20

func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	u := *c.base
	u.Path = path
	u.RawQuery = query.Encode()
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		ae := &APIError{Status: resp.StatusCode, Message: http.StatusText(resp.StatusCode)}
		var e struct {
			Error      string   `json:"error"`
			MessageIDs []string `json:"messageIds"`
		}
		if json.Unmarshal(data, &e) == nil {
			if e.Error != "" {
				ae.Message = e.Error
			}
			ae.MessageIDs = e.MessageIDs
		}
		return ae
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("whatsapp bridge: bad response from %s: %w", path, err)
	}
	return nil
}

// Messages long-polls for queued events, waiting up to wait for the first one.
// ctx should outlive wait by a margin.
func (c *Client) Messages(ctx context.Context, wait time.Duration) ([]Event, error) {
	q := url.Values{"timeout": {strconv.Itoa(int(wait.Seconds()))}}
	var evs []Event
	if err := c.do(ctx, http.MethodGet, "/messages", q, nil, &evs); err != nil {
		return nil, err
	}
	return evs, nil
}

// Send delivers text to chatID (a JID). replyTo optionally names an inbound
// message id to quote.
func (c *Client) Send(ctx context.Context, chatID, text, replyTo string) (SendResult, error) {
	in := map[string]string{"chatId": chatID, "message": text}
	if replyTo != "" {
		in["replyTo"] = replyTo
	}
	var out SendResult
	err := c.do(ctx, http.MethodPost, "/send", nil, in, &out)
	if err != nil {
		return SendResult{}, err
	}
	if len(out.MessageIDs) == 0 && out.MessageID != "" {
		out.MessageIDs = []string{out.MessageID}
	}
	return out, nil
}

// Typing shows the "typing…" indicator in chatID.
func (c *Client) Typing(ctx context.Context, chatID string) error {
	return c.do(ctx, http.MethodPost, "/typing", nil, map[string]string{"chatId": chatID}, nil)
}

// MarkRead sends a read receipt for the message key.
func (c *Client) MarkRead(ctx context.Context, key ReadKey) error {
	return c.do(ctx, http.MethodPost, "/read", nil, map[string]ReadKey{"key": key}, nil)
}

// Health fetches the bridge's state.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	err := c.do(ctx, http.MethodGet, "/health", nil, nil, &h)
	return h, err
}

// IsAPIStatus reports whether err is an APIError with the given HTTP status.
func IsAPIStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}
