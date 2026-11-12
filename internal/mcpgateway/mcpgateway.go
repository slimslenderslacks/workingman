package mcpgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/policy"
)

// EnvVar is the sandbox environment variable carrying the per-sandbox gateway
// URL. acp-kit's claude-acp-client registers it as the `okta-gateway` MCP
// server before starting the ACP bridge.
const EnvVar = "MCP_URL"

// SandboxHost is how a sandbox reaches a service bound to the host's
// localhost.
const SandboxHost = "host.docker.internal"

// defaultTimeout bounds one delete or liveness call when the caller's context
// has no deadline of its own.
const defaultTimeout = 15 * time.Second

// createTimeout bounds one POST /gateways. Creating a gateway attaches each of
// its backends before answering, and standalone-codemode allows each attach
// up to 60s (aggregategw.DefaultAttachTimeout); a slow one — gmail through
// the llm.atko.ai proxy takes ~30s — must not time the request out here and
// leave the sandbox without MCP_URL.
const createTimeout = 90 * time.Second

// Gateway is one sub-gateway minted by POST /gateways.
type Gateway struct {
	// ID is the gateway's id (a UUID), the last path segment of URL.
	ID string `json:"id"`
	// URL is the gateway's MCP endpoint as the host reaches it (built by
	// standalone-codemode from the Host header we POSTed with).
	URL string `json:"url"`
	// Servers is what each requested server was doing when the gateway was
	// created. Nil from a gateway too old to report it.
	Servers map[string]ServerState `json:"servers,omitempty"`
	// Dynamic is whether the gateway reports having mcp-find and mcp-add.
	// Only meaningful when Servers is non-nil (an older gateway omits both).
	Dynamic bool `json:"dynamic"`
}

// ServerState is POST /gateways' report on one requested server.
type ServerState struct {
	// State is "connected", "needs_auth", "reconnecting", "failed" or
	// "deferred".
	State string `json:"state"`
	// Tools is how many tools the server published.
	Tools int `json:"tools"`
	// Error says why, for failed and deferred.
	Error string `json:"error,omitempty"`
}

// Serving reports whether the server is attached and publishing tools.
func (s ServerState) Serving() bool { return s.State == "connected" && s.Tools > 0 }

// UnknownServersError is POST /gateways' 400 for server names outside the
// gateway's catalog.
type UnknownServersError struct {
	Names []string
}

func (e *UnknownServersError) Error() string {
	return "mcpgateway: unknown catalog server(s): " + strings.Join(e.Names, ", ")
}

// Client talks to a standalone-codemode gateway's /gateways API.
type Client struct {
	// BaseURL is the gateway's root, e.g. http://localhost:3333.
	BaseURL string
	// HTTP is the client used for requests; nil means a client with a 90s
	// timeout for Create and 15s for everything else.
	HTTP *http.Client
}

func (c Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: defaultTimeout}
}

func (c Client) createClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: createTimeout}
}

func (c Client) base() string { return strings.TrimSuffix(strings.TrimSpace(c.BaseURL), "/") }

// Create POSTs {"servers": servers, "dynamic": dynamic} to <BaseURL>/gateways
// and returns the new gateway. A dynamic gateway also serves mcp-find and
// mcp-add over the whole catalog, so the agent can attach more servers. Anything but a 201 with an id and url is an error carrying the
// response body.
//
// servers may be empty only for a dynamic gateway, which then starts with just
// mcp-find and mcp-add.
func (c Client) Create(ctx context.Context, servers []string, dynamic bool) (Gateway, error) {
	if len(servers) == 0 && !dynamic {
		return Gateway{}, fmt.Errorf("mcpgateway: at least one server is required")
	}
	if servers == nil {
		servers = []string{} // "servers": [], not null
	}
	body, err := json.Marshal(struct {
		Servers []string `json:"servers"`
		Dynamic bool     `json:"dynamic,omitempty"`
	}{servers, dynamic})
	if err != nil {
		return Gateway{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+"/gateways", bytes.NewReader(body))
	if err != nil {
		return Gateway{}, fmt.Errorf("mcpgateway: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.createClient().Do(req)
	if err != nil {
		return Gateway{}, fmt.Errorf("mcpgateway: POST %s/gateways: %w", c.base(), err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusBadRequest {
		var e struct {
			Error   string   `json:"error"`
			Unknown []string `json:"unknown"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error == "unknown_servers" && len(e.Unknown) > 0 {
			return Gateway{}, &UnknownServersError{Names: e.Unknown}
		}
	}
	if resp.StatusCode != http.StatusCreated {
		return Gateway{}, fmt.Errorf("mcpgateway: POST %s/gateways: %s: %s", c.base(), resp.Status, strings.TrimSpace(string(data)))
	}
	var gw Gateway
	if err := json.Unmarshal(data, &gw); err != nil {
		return Gateway{}, fmt.Errorf("mcpgateway: decode POST /gateways response: %w", err)
	}
	if gw.ID == "" || gw.URL == "" {
		return Gateway{}, fmt.Errorf("mcpgateway: POST /gateways response missing id or url: %s", strings.TrimSpace(string(data)))
	}
	return gw, nil
}

// Delete tears the gateway with the given id down (DELETE
// <BaseURL>/gateways/<id>). An already-gone gateway (404) is not an error.
func (c Client) Delete(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.base()+"/gateways/"+url.PathEscape(id), nil)
	if err != nil {
		return fmt.Errorf("mcpgateway: %w", err)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("mcpgateway: DELETE %s/gateways/%s: %w", c.base(), id, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		return nil
	default:
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("mcpgateway: DELETE %s/gateways/%s: %s: %s", c.base(), id, resp.Status, strings.TrimSpace(string(data)))
	}
}

// isLoopback reports whether host names the host machine's loopback.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SandboxURL rewrites a host-side gateway URL so it is reachable from inside
// a sandbox: a loopback host (localhost, 127.0.0.1, ::1) becomes
// host.docker.internal, keeping the scheme, port and path. Any other host is
// returned unchanged.
func SandboxURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("mcpgateway: parse url %q: %w", raw, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("mcpgateway: url %q has no host", raw)
	}
	if isLoopback(u.Hostname()) {
		if port := u.Port(); port != "" {
			u.Host = net.JoinHostPort(SandboxHost, port)
		} else {
			u.Host = SandboxHost
		}
	}
	return u.String(), nil
}

// Port is the TCP port of a gateway URL: the explicit one, else the scheme's
// default (80 for http, 443 for https).
func Port(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("mcpgateway: parse url %q: %w", raw, err)
	}
	if p := u.Port(); p != "" {
		return p, nil
	}
	switch u.Scheme {
	case "https":
		return "443", nil
	case "http":
		return "80", nil
	}
	return "", fmt.Errorf("mcpgateway: url %q has no port and an unknown scheme", raw)
}

// PolicyRules are the sbx network rules that let a sandbox reach the gateway
// at baseURL. A loopback base gets two rules on the gateway's port:
// host.docker.internal:<port> (the name the sandbox dials) and
// localhost:<port> (how sbx's proxy names a host-loopback destination). Any
// other host gets one rule, <host>:<port>.
func PolicyRules(baseURL string) ([]policy.Rule, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("mcpgateway: parse url %q: %w", baseURL, err)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("mcpgateway: url %q has no host", baseURL)
	}
	port, err := Port(baseURL)
	if err != nil {
		return nil, err
	}
	allow := func(host string) policy.Rule {
		return policy.Rule{Action: policy.ActionAllow, Kind: policy.KindNetwork, Resource: net.JoinHostPort(host, port)}
	}
	if isLoopback(u.Hostname()) {
		return []policy.Rule{allow(SandboxHost), allow("localhost")}, nil
	}
	return []policy.Rule{allow(u.Hostname())}, nil
}

// Healthy reports whether the gateway answers GET <BaseURL>/healthz with 200.
// standalone-codemode only starts listening once it has attached the
// backends of its whole catalog, which takes a while after a restart.
func (c Client) Healthy(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+"/healthz", nil)
	if err != nil {
		return fmt.Errorf("mcpgateway: %w", err)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("mcpgateway: GET %s/healthz: %w", c.base(), err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mcpgateway: GET %s/healthz: %s", c.base(), resp.Status)
	}
	return nil
}

// Alive reports whether the gateway with the given id is still registered:
// false only on a 404 (standalone-codemode keeps sub-gateways in memory, so a
// gateway restart drops every one). Any other answer — the endpoint is an MCP
// handler and rejects a bare GET — means it exists. A transport error is
// returned as-is: an unreachable gateway is not proof the id is gone.
func (c Client) Alive(ctx context.Context, id string) (bool, error) {
	if strings.TrimSpace(id) == "" {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+"/gateways/"+url.PathEscape(id), nil)
	if err != nil {
		return false, fmt.Errorf("mcpgateway: %w", err)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return false, fmt.Errorf("mcpgateway: GET %s/gateways/%s: %w", c.base(), id, err)
	}
	resp.Body.Close()
	return resp.StatusCode != http.StatusNotFound, nil
}
