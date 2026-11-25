package mcpserver

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/slimslenderslacks/work/internal/mcpgateway"
	"github.com/slimslenderslacks/work/internal/postoffice"
	"github.com/slimslenderslacks/work/internal/session"
	"github.com/slimslenderslacks/work/internal/task"
)

// GatewayIDHeader carries the calling sub-gateway's id. standalone-codemode
// sets it on every backend request it makes for a /gateways/{id}
// sub-gateway, overriding any catalog header of the same name, and sends
// none for its parent /mcp (§4.3).
const GatewayIDHeader = "X-Codemode-Gateway-Id"

// ServerName is the catalog name a gateway must have been minted with for
// its sandbox to use post-office (§4.2).
const ServerName = "post-office"

// DefaultIdentityTTL is how long a resolved caller is cached per gateway id.
const DefaultIdentityTTL = 30 * time.Second

// Rejections of a caller (§4.3). Each is reported to the client as a
// JSON-RPC error carrying the message.
var (
	ErrNoGatewayID    = errors.New("post-office: no " + GatewayIDHeader + " header; call post-office through a sandbox's sub-gateway")
	ErrUnknownGateway = errors.New("post-office: unknown gateway")
	ErrNotEnabled     = errors.New("post-office is not enabled for this sandbox (add static_mcps: [post-office] to the task)")
	ErrNoSession      = errors.New("post-office: no live session for the sandbox")
)

// singletons maps the fixed sandbox names of the project-independent agents
// to their agent.Kind strings (runner.WorkingmanAgentSandbox,
// runner.SleepyHeadSandbox, and the Step-Up agent's sandbox). They are
// identified by name alone; a session, when one is live, only adds its id.
var singletons = map[string]string{
	"workingman-agent": "workingman",
	"sleepy-head":      "sleepy-head",
	"step-up":          "step-up",
}

// Caller is a resolved post-office caller.
type Caller struct {
	GatewayID string
	Sender    postoffice.Sender
}

// Resolver turns a sub-gateway id into the Sender behind it (§4.3): the
// gateway record gives the sandbox and the servers the gateway was minted
// with, and the sandbox's newest live session gives the agent kind, project
// and task. A Resolver is safe for concurrent use.
type Resolver struct {
	// Gateways holds the per-sandbox gateway records
	// (~/.workingman/gateways/<sandbox>.json).
	Gateways mcpgateway.Store
	// Sessions is the ACP sessions root (~/.workingman/sessions).
	Sessions session.Store
	// TTL is how long a successful resolution is cached; zero means
	// DefaultIdentityTTL. Failures are not cached, so a gateway record saved
	// just after its sub-gateway connected is picked up on the next call.
	TTL time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu    sync.Mutex
	cache map[string]cachedCaller
}

type cachedCaller struct {
	caller Caller
	until  time.Time
}

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Resolve returns the caller for gatewayID, or an error wrapping one of
// ErrNoGatewayID, ErrUnknownGateway, ErrNotEnabled or ErrNoSession.
func (r *Resolver) Resolve(gatewayID string) (Caller, error) {
	gatewayID = strings.TrimSpace(gatewayID)
	if gatewayID == "" {
		return Caller{}, ErrNoGatewayID
	}
	now := r.now()
	r.mu.Lock()
	if c, ok := r.cache[gatewayID]; ok && now.Before(c.until) {
		r.mu.Unlock()
		return c.caller, nil
	}
	r.mu.Unlock()

	c, err := r.resolve(gatewayID)
	if err != nil {
		return Caller{}, err
	}
	ttl := r.TTL
	if ttl <= 0 {
		ttl = DefaultIdentityTTL
	}
	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[string]cachedCaller{}
	}
	for id, cc := range r.cache {
		if !now.Before(cc.until) {
			delete(r.cache, id)
		}
	}
	r.cache[gatewayID] = cachedCaller{caller: c, until: now.Add(ttl)}
	r.mu.Unlock()
	return c, nil
}

func (r *Resolver) resolve(gatewayID string) (Caller, error) {
	recs, err := r.Gateways.List()
	if err != nil {
		return Caller{}, fmt.Errorf("post-office: gateway records: %w", err)
	}
	var rec mcpgateway.Record
	found := false
	for _, x := range recs {
		if x.ID == gatewayID {
			rec, found = x, true
			break
		}
	}
	if !found {
		return Caller{}, fmt.Errorf("%w %q: no gateway record has that id", ErrUnknownGateway, gatewayID)
	}
	// Servers is what the gateway was minted with; mcp-add on a dynamic
	// gateway never changes it, so it cannot grant the capability.
	if !slices.Contains(rec.Servers, ServerName) {
		return Caller{}, fmt.Errorf("%w; sandbox %s", ErrNotEnabled, rec.Sandbox)
	}
	sender := postoffice.Sender{Sandbox: rec.Sandbox}
	sess, haveSession, err := r.newestLive(rec.Sandbox)
	if err != nil {
		return Caller{}, err
	}
	if kind, ok := singletons[rec.Sandbox]; ok {
		sender.Agent = kind
		if haveSession {
			sender.SessionID = sess.ID
		}
		return Caller{GatewayID: gatewayID, Sender: sender}, nil
	}
	if !haveSession {
		return Caller{}, fmt.Errorf("%w %s", ErrNoSession, rec.Sandbox)
	}
	sender.Agent = sess.Kind
	sender.Project = sess.ProjectPath
	sender.Task = taskName(sess.TaskPath)
	sender.SessionID = sess.ID
	return Caller{GatewayID: gatewayID, Sender: sender}, nil
}

// newestLive returns the most recently created starting or running session
// for sandbox ("newest live session", §10.3 decision 8).
func (r *Resolver) newestLive(sandbox string) (session.Session, bool, error) {
	all, err := r.Sessions.List()
	if err != nil {
		return session.Session{}, false, fmt.Errorf("post-office: sessions: %w", err)
	}
	// List is oldest first.
	for _, s := range slices.Backward(all) {
		if s.SandboxName == sandbox && (s.Status == session.StatusRunning || s.Status == session.StatusStarting) {
			return s, true, nil
		}
	}
	return session.Session{}, false, nil
}

// taskName reads the task's name from its file, falling back to the file
// name without its extension.
func taskName(path string) string {
	if path == "" {
		return ""
	}
	if t, err := task.Load(path); err == nil && t.Name != "" {
		return t.Name
	}
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}
