package mcpgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/policy"
)

// SweepGrace is how old a record must be before Sweep may release it for a
// sandbox that `sbx ls` doesn't show. A record is saved only after its
// sandbox is created, but another wrapper can save one between our `sbx ls`
// and our sweep; the grace keeps us from deleting a gateway a concurrent
// launch just minted.
const SweepGrace = 10 * time.Minute

// Manager mints, validates and releases the per-sandbox gateways for one
// workingman process. The zero value is disabled (Enabled is false) and every
// method is a no-op on it, so callers need no nil checks.
type Manager struct {
	// Client talks to the standalone-codemode gateway. An empty BaseURL
	// disables the manager.
	Client Client
	// Store persists one Record per sandbox. An empty Dir disables
	// persistence (and so reuse, Release and Sweep).
	Store Store
	// Servers are the catalog server names requested for every gateway
	// minted. Empty disables a static manager (a static gateway over nothing
	// would serve nothing); a Dynamic one starts empty and grows with mcp-add.
	Servers []string
	// Dynamic asks for gateways with mcp-find and mcp-add, so the agent can
	// find and attach catalog servers beyond Servers.
	Dynamic bool
	// Log receives diagnostics; nil means os.Stderr.
	Log io.Writer
	// Prefix starts every diagnostic line (e.g. "acp-wrapper: session <id>: ");
	// empty means "mcpgateway: ".
	Prefix string
	// RetryBase is WaitReady's first retry delay; zero means 1s. Tests shrink
	// it.
	RetryBase time.Duration
}

// Enabled reports whether sandboxes should get a gateway at all.
func (m Manager) Enabled() bool {
	return m.Client.base() != "" && (len(m.Servers) > 0 || m.Dynamic)
}

func (m Manager) logf(format string, args ...any) {
	w := m.Log
	if w == nil {
		w = os.Stderr
	}
	prefix := m.Prefix
	if prefix == "" {
		prefix = "mcpgateway: "
	}
	fmt.Fprintf(w, prefix+format+"\n", args...)
}

// PolicyRules are the sbx network rules every sandbox gets once a gateway
// URL is configured (see the package-level PolicyRules) — even one whose
// gateway could not be minted, so a later relaunch that reuses it can reach a
// gateway again. Nil when no URL is configured.
func (m Manager) PolicyRules() ([]policy.Rule, error) {
	if m.Client.base() == "" {
		return nil, nil
	}
	return PolicyRules(m.Client.BaseURL)
}

// Reusable reports whether the gateway recorded for an existing sandbox can
// keep serving it: there is a record, it was minted on this BaseURL over
// these Servers with the same Dynamic, and the gateway still exists. A gateway we cannot reach
// (standalone-codemode down) counts as reusable — minting a replacement would
// fail the same way, so recreating the sandbox would only lose its state.
func (m Manager) Reusable(ctx context.Context, sandbox string) (Record, bool) {
	if !m.Enabled() || m.Store.Dir == "" {
		return Record{}, false
	}
	rec, ok, err := m.Store.Load(sandbox)
	if err != nil {
		m.logf("sandbox %s: %v", sandbox, err)
		return Record{}, false
	}
	if !ok || rec.ID == "" || rec.URL == "" {
		return Record{}, false
	}
	// Re-derive the sandbox URL rather than trusting the stored one, so a
	// record written with a loopback host never reaches the sandbox as-is.
	if rec.SandboxURL, err = SandboxURL(rec.URL); err != nil {
		m.logf("sandbox %s: %v", sandbox, err)
		return Record{}, false
	}
	if rec.BaseURL != m.Client.base() || !slices.Equal(rec.Requested, m.Servers) || rec.Dynamic != m.Dynamic {
		return rec, false
	}
	alive, err := m.Client.Alive(ctx, rec.ID)
	if err != nil {
		m.logf("sandbox %s: WARNING cannot check gateway %s, keeping it: %v", sandbox, rec.ID, err)
		return rec, true
	}
	return rec, alive
}

// Mint releases any gateway already recorded for sandbox, then POSTs a new
// one over m.Servers and returns its record. The record is NOT saved: call
// Save once the sandbox it belongs to exists, or Discard if creating it fails.
func (m Manager) Mint(ctx context.Context, sandbox string) (Record, error) {
	if !m.Enabled() {
		return Record{}, fmt.Errorf("mcpgateway: not configured")
	}
	m.Release(ctx, sandbox)
	servers := slices.Clone(m.Servers)
	gw, err := m.Client.Create(ctx, servers, m.Dynamic)
	// A task's static_mcps may name servers this catalog doesn't carry; serve
	// the ones it does rather than none at all.
	var unknown *UnknownServersError
	if errors.As(err, &unknown) {
		servers = slices.DeleteFunc(servers, func(s string) bool { return slices.Contains(unknown.Names, s) })
		if len(servers) == 0 && !m.Dynamic {
			return Record{}, err
		}
		m.logf("sandbox %s: WARNING %v; creating the gateway over %s only", sandbox, err, strings.Join(servers, ","))
		gw, err = m.Client.Create(ctx, servers, m.Dynamic)
	}
	if err != nil {
		return Record{}, err
	}
	m.reportGaps(sandbox, gw)
	sandboxURL, err := SandboxURL(gw.URL)
	if err == nil && strings.ContainsAny(sandboxURL, "'\n") {
		err = fmt.Errorf("mcpgateway: refusing gateway url %q", sandboxURL)
	}
	if err != nil {
		_ = m.Client.Delete(ctx, gw.ID)
		return Record{}, err
	}
	return Record{
		Sandbox:    sandbox,
		BaseURL:    m.Client.base(),
		ID:         gw.ID,
		URL:        gw.URL,
		SandboxURL: sandboxURL,
		Servers:    servers,
		Requested:  slices.Clone(m.Servers),
		Dynamic:    m.Dynamic,
		CreatedAt:  time.Now().UTC(),
	}, nil
}

// Gaps lists, one line each, what gw does not provide of what m asked for:
// every requested server that is not attached and serving tools (with its
// state and why), and mcp-find / mcp-add when Dynamic was asked for and the
// gateway did not confirm it. Empty when the gateway gave everything, or when
// it is too old to report (no Servers in its answer).
func (m Manager) Gaps(gw Gateway) []string {
	if gw.Servers == nil {
		return nil
	}
	var gaps []string
	for _, name := range m.Servers {
		st, ok := gw.Servers[name]
		switch {
		case !ok:
			gaps = append(gaps, fmt.Sprintf("%s: not in the gateway", name))
		case !st.Serving():
			line := fmt.Sprintf("%s: %s, %d tools", name, st.State, st.Tools)
			if st.Error != "" {
				line += ": " + st.Error
			}
			switch st.State {
			case "needs_auth":
				line += " (authorize it on the host: gateway authorize " + name + ")"
			case "reconnecting", "deferred":
				line += " (remote unreachable; the gateway keeps retrying)"
			}
			gaps = append(gaps, line)
		}
	}
	if m.Dynamic && !gw.Dynamic {
		gaps = append(gaps, "mcp-find/mcp-add: dynamic was requested but the gateway is not dynamic")
	}
	return gaps
}

// reportGaps logs Gaps for a freshly minted gateway, one WARNING per gap. The
// sandbox still gets the gateway: a server whose remote is down may come
// back, and the rest of the gateway is usable meanwhile.
func (m Manager) reportGaps(sandbox string, gw Gateway) {
	gaps := m.Gaps(gw)
	for _, g := range gaps {
		m.logf("sandbox %s: WARNING gateway %s does not provide %s", sandbox, gw.ID, g)
	}
	if len(gaps) == 0 && gw.Servers != nil {
		m.logf("sandbox %s: gateway %s provides every requested server (%s)%s", sandbox, gw.ID, strings.Join(m.Servers, ","), dynamicNote(m.Dynamic))
	}
}

func dynamicNote(dynamic bool) string {
	if dynamic {
		return " plus mcp-find/mcp-add"
	}
	return ""
}

// DefaultReadyTimeout is how long WaitReady waits for a gateway that is not
// answering yet: a restarted standalone-codemode attaches its whole catalog
// (some backends take ~30s) before it listens.
const DefaultReadyTimeout = 2 * time.Minute

// WaitReady blocks until the gateway answers /healthz, retrying with backoff
// (1s doubling to 10s) for up to timeout (DefaultReadyTimeout when <= 0). It
// is a no-op for a disabled manager. Each wait is logged once at the start and
// once when it ends, so a sandbox that started late — or without its gateway
// — says why.
func (m Manager) WaitReady(ctx context.Context, sandbox string, timeout time.Duration) error {
	if !m.Enabled() {
		return nil
	}
	if timeout <= 0 {
		timeout = DefaultReadyTimeout
	}
	err := m.Client.Healthy(ctx)
	if err == nil {
		return nil
	}
	start := time.Now()
	m.logf("sandbox %s: gateway %s not ready (%v); waiting up to %s before creating the sandbox", sandbox, m.Client.base(), err, timeout)
	deadline := start.Add(timeout)
	delay := m.retryBase()
	for {
		wait := delay
		if left := time.Until(deadline); left < wait {
			wait = left
		}
		if wait <= 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		if err = m.Client.Healthy(ctx); err == nil {
			m.logf("sandbox %s: gateway %s ready after %s", sandbox, m.Client.base(), time.Since(start).Round(time.Second))
			return nil
		}
		if delay *= 2; delay > 10*time.Second {
			delay = 10 * time.Second
		}
	}
	return fmt.Errorf("mcpgateway: gateway %s not ready after %s: %w", m.Client.base(), timeout, err)
}

func (m Manager) retryBase() time.Duration {
	if m.RetryBase > 0 {
		return m.RetryBase
	}
	return time.Second
}

// Prepare mints the gateway for a sandbox that is about to be created, so the
// gateway exists before the sandbox does. The record is not saved: call Save
// once `sbx create` and its policies succeed, or Discard if they fail. A
// disabled manager or a failed mint (logged) yields a zero Record — creating
// the sandbox still goes ahead, and Ensure retries the mint afterwards.
func (m Manager) Prepare(ctx context.Context, sandbox string) Record {
	if !m.Enabled() {
		return Record{}
	}
	rec, err := m.Mint(ctx, sandbox)
	if err != nil {
		m.logf("sandbox %s: WARNING no gateway before create: %v", sandbox, err)
		return Record{}
	}
	return rec
}

// Ensure returns the gateway sandbox should use: the recorded one when it is
// still valid (Reusable), otherwise a freshly minted one — the stale one is
// DELETEd first and the new one recorded.
func (m Manager) Ensure(ctx context.Context, sandbox string) (Record, error) {
	if rec, ok := m.Reusable(ctx, sandbox); ok {
		return rec, nil
	}
	rec, err := m.Mint(ctx, sandbox)
	if err != nil {
		return Record{}, err
	}
	if err := m.Save(rec); err != nil {
		m.Discard(ctx, rec)
		return Record{}, err
	}
	return rec, nil
}

// Save persists rec so later launches can reuse or release its gateway. A
// zero Record (nothing minted) is a no-op.
func (m Manager) Save(rec Record) error {
	if m.Store.Dir == "" || rec.ID == "" {
		return nil
	}
	return m.Store.Save(rec)
}

// Discard best-effort DELETEs a minted-but-unsaved gateway.
func (m Manager) Discard(ctx context.Context, rec Record) {
	if rec.ID == "" {
		return
	}
	if err := (Client{BaseURL: rec.BaseURL, HTTP: m.Client.HTTP}).Delete(ctx, rec.ID); err != nil {
		m.logf("sandbox %s: WARNING %v", rec.Sandbox, err)
	}
}

// Release best-effort DELETEs the gateway recorded for sandbox (on the
// BaseURL it was minted on, which may differ from the current one) and drops
// the record. Failures are logged, never returned: a leaked sub-gateway costs
// memory in standalone-codemode until its next restart, nothing more.
func (m Manager) Release(ctx context.Context, sandbox string) {
	if m.Store.Dir == "" {
		return
	}
	if _, err := os.Stat(m.Store.Dir); err != nil {
		return // nothing ever recorded
	}
	rec, ok, err := m.Store.Load(sandbox)
	if err != nil {
		m.logf("sandbox %s: %v", sandbox, err)
	}
	if ok {
		m.Discard(ctx, rec)
	}
	if err := m.Store.Remove(sandbox); err != nil {
		m.logf("sandbox %s: %v", sandbox, err)
	}
}

// Sweep releases the gateways of recorded sandboxes that no longer exist
// (live reports false) and whose record is older than grace — e.g. a kept
// sandbox someone later removed by hand, which no wrapper exit ever saw.
func (m Manager) Sweep(ctx context.Context, live func(sandbox string) bool, grace time.Duration) {
	recs, err := m.Store.List()
	if err != nil {
		m.logf("sweep: %v", err)
		return
	}
	now := time.Now()
	for _, rec := range recs {
		if live(rec.Sandbox) || now.Sub(rec.CreatedAt) < grace {
			continue
		}
		m.logf("sandbox %s is gone; releasing its gateway %s", rec.Sandbox, rec.ID)
		m.Release(ctx, rec.Sandbox)
	}
}

// CreateEnvArgs are the `sbx create` flags that set MCP_URL to rec's
// sandbox-reachable URL (loopback rewritten to host.docker.internal), or none
// for a zero Record. `sbx exec` still passes it per launch (Ensure may have
// replaced the gateway since); this covers everything else started in the
// sandbox, such as an interactive `sbx run`.
func CreateEnvArgs(rec Record) []string {
	if rec.SandboxURL == "" {
		return nil
	}
	return []string{"-e", EnvVar + "=" + rec.SandboxURL}
}

// EnvFile is the sandbox's persistent environment file, sourced by every
// shell inside it (login, interactive and, via BASH_ENV, non-interactive).
const EnvFile = "/etc/sandbox-persistent.sh"

// PersistEnvArgs is the argv (after the sbx binary) that records
// MCP_URL=<sandboxURL> in the sandbox's EnvFile, replacing any earlier
// MCP_URL line, so shells a human opens in the sandbox see the gateway too.
// The ACP client does not depend on it: it gets MCP_URL from `sbx exec -e`.
//
// The file is rewritten in place (read, filter, truncate-and-write) rather than
// with `sed -i`: the agent user owns the file but not /etc, where `sed -i`
// must create its temporary file.
func PersistEnvArgs(sandbox, sandboxURL string) []string {
	script := `f=` + EnvFile + `; touch "$f" && rest=$(grep -v '^export ` + EnvVar + `=' "$f"; true) && { [ -z "$rest" ] || printf '%s\n' "$rest"; printf "export ` + EnvVar + `='%s'\n" "$1"; } > "$f"`
	return []string{"exec", sandbox, "--", "sh", "-c", script, "sh", sandboxURL}
}
