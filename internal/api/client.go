package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// Client reads the API of a running daemon. The zero value is not usable;
// set Addr and Token.
type Client struct {
	// Addr is the listener: a unix socket path (anything with a "/"), or a
	// loopback host:port.
	Addr string
	// Token is the bearer token (ReadToken).
	Token string
	// HTTP is the client to use; nil builds one for Addr. It must not set
	// a timeout, or it cuts event streams short.
	HTTP *http.Client
}

// ResolveAddr picks the listener a client on this machine should use when
// none is named: the default socket under dir if it exists, else the
// address a port-0 loopback listener recorded in api.json, else the default
// loopback address.
func ResolveAddr(dir string) string {
	sock := SocketPath(dir)
	if fi, err := os.Stat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
		return sock
	}
	if b, err := os.ReadFile(InfoPath(dir)); err == nil {
		var info struct {
			Addr string `json:"addr"`
		}
		if json.Unmarshal(b, &info) == nil && info.Addr != "" {
			return info.Addr
		}
	}
	return DefaultLoopbackAddr
}

func (c *Client) unix() bool { return strings.Contains(c.Addr, "/") }

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	if !c.unix() {
		c.HTTP = &http.Client{}
		return c.HTTP
	}
	sock := c.Addr
	c.HTTP = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	return c.HTTP
}

// url is the request URL for path (which starts with Prefix). On a socket
// the host is "localhost", which the server's Host check accepts.
func (c *Client) url(path string) string {
	if c.unix() {
		return "http://localhost" + path
	}
	return "http://" + c.Addr + path
}

func (c *Client) request(ctx context.Context, path string) (*http.Request, error) {
	return c.requestMethod(ctx, http.MethodGet, path)
}

func (c *Client) requestMethod(ctx context.Context, method, path string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.url(path), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	return req, nil
}

// StatusError is a non-2xx response, with the server's error body.
type StatusError struct {
	Status int
	Code   string
	Msg    string
}

func (e *StatusError) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("api: HTTP %d", e.Status)
	}
	return fmt.Sprintf("api: HTTP %d: %s", e.Status, e.Msg)
}

func statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var eb ErrorBody
	if json.Unmarshal(body, &eb) == nil && eb.Error.Message != "" {
		return &StatusError{Status: resp.StatusCode, Code: eb.Error.Code, Msg: eb.Error.Message}
	}
	return &StatusError{Status: resp.StatusCode, Msg: strings.TrimSpace(string(body))}
}

// Get requests path (Prefix included) and decodes the JSON body into v.
func (c *Client) Get(ctx context.Context, path string, v any) error {
	req, err := c.request(ctx, path)
	if err != nil {
		return err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// LoginCode mints a one-time browser login code (POST /login-codes), which
// `orch gui` opens the GUI with. It needs a loopback TCP listener: a browser
// cannot reach the unix socket.
func (c *Client) LoginCode(ctx context.Context) (LoginCode, error) {
	var lc LoginCode
	req, err := c.requestMethod(ctx, http.MethodPost, Prefix+"/login-codes")
	if err != nil {
		return lc, err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return lc, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return lc, statusError(resp)
	}
	return lc, json.NewDecoder(resp.Body).Decode(&lc)
}

// StreamEvent is one frame read from the event stream: ID is its SSE id
// ("" for frames without one: hello, heartbeat and a full replay's state).
type StreamEvent struct {
	ID string
	Event
}

// ErrStreamEnded is what [Client.Events] returns when the server ended the
// stream: the daemon stopped, or dropped a client that fell behind. A
// client resumes by calling Events again with the last ID it saw.
var ErrStreamEnded = errors.New("api: the server ended the event stream")

// Events opens GET /events and calls fn with every frame until ctx is done,
// the stream ends or fn returns an error, which Events returns. lastID,
// when set, is sent as Last-Event-ID, so the stream resumes after it;
// query holds the subscription (project, types, since).
func (c *Client) Events(ctx context.Context, lastID string, query url.Values, fn func(StreamEvent) error) error {
	path := Prefix + "/events"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	req, err := c.request(ctx, path)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusError(resp)
	}
	err = ReadSSE(resp.Body, fn)
	if err == nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrStreamEnded
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// ReadSSE parses a text/event-stream body, calling fn with each frame whose
// data is an [Event]. It returns nil at the end of the body, or the first
// error from fn or the reader. Comment lines and unknown fields are
// skipped, as the SSE spec says.
func ReadSSE(r io.Reader, fn func(StreamEvent) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	var (
		id   string
		data strings.Builder
		has  bool
	)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if has {
				var e Event
				if err := json.Unmarshal([]byte(data.String()), &e); err != nil {
					return fmt.Errorf("api: bad event data: %w", err)
				}
				if err := fn(StreamEvent{ID: id, Event: e}); err != nil {
					return err
				}
			}
			id, has = "", false
			data.Reset()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			id = value
		case "data":
			if has {
				data.WriteByte('\n')
			}
			data.WriteString(value)
			has = true
		}
	}
	return sc.Err()
}
