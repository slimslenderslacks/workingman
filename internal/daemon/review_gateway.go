package daemon

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/mcpgateway"
)

// The daemon's own MCP gateway: a static sub-gateway of the host's
// standalone-codemode gateway over ONLY the github server, used by the PR
// fingerprint check (no `gh`, no local GitHub auth). It is an
// mcpgateway.DaemonGateway recorded under a fixed key, which owns the mint,
// record, re-mint-on-loss and delete lifecycle:
//
//   - Run starts it (startReviewGateway), deleting a stale recorded gateway
//     and creating the new one off the startup path. A failure is logged and
//     retried with exponential backoff; it never stops the daemon.
//   - URL hands the gateway to callers, trying a creation on demand once the
//     backoff window has passed. Invalidate drops a gateway a caller found
//     gone (e.g. standalone-codemode restarted) so a fresh one is minted.
//   - shutdown DELETEs the gateway and removes its record.
//
// Audit events are review_gateway_*; daemon.log lines start "review gateway:".

const (
	// reviewGatewayKey is the Store key of the daemon's gateway record.
	reviewGatewayKey = "orch-daemon"
	// reviewGatewayServer is the only catalog server the gateway is minted over.
	reviewGatewayServer = "github"
	// reviewName prefixes the gateway's and the session's audit events and
	// log lines.
	reviewName = "review"
)

// ReviewGatewayConfig configures the daemon's github-only MCP gateway.
type ReviewGatewayConfig struct {
	// BaseURL is the standalone-codemode root, e.g. http://localhost:3333
	// (cmd/orch's --mcp-gateway-url). Empty disables the gateway.
	BaseURL string
	// StateDir is the mcpgateway.Store directory for the gateway record. Use a
	// directory of its own (see ReviewGatewayStateDir), not the per-sandbox
	// gateways directory: acp-wrapper's Sweep releases every record there whose
	// name is not a live sandbox.
	StateDir string
}

// ReviewGatewayStateDir is the default ReviewGatewayConfig.StateDir for a
// per-sandbox gateway store directory (mcpgateway.StateDir): a "daemon"
// subdirectory, which mcpgateway.Store.List — and so acp-wrapper's Sweep —
// skips.
func ReviewGatewayStateDir(gatewaysDir string) string {
	return filepath.Join(gatewaysDir, "daemon")
}

// WithReviewGateway gives the daemon its own github-only MCP gateway for the
// PR fingerprint check (see review_gateway.go). An empty BaseURL leaves it
// disabled.
func WithReviewGateway(cfg ReviewGatewayConfig) Option {
	return func(d *Daemon) {
		base := strings.TrimSuffix(strings.TrimSpace(cfg.BaseURL), "/")
		if base == "" {
			return
		}
		d.reviewGW = newReviewGateway(base, mcpgateway.Store{Dir: cfg.StateDir}, d.audit)
	}
}

func newReviewGateway(endpoint string, store mcpgateway.Store, a *audit.Logger) *mcpgateway.DaemonGateway {
	return &mcpgateway.DaemonGateway{
		Key:     reviewGatewayKey,
		Servers: []string{reviewGatewayServer},
		BaseURL: endpoint,
		Store:   store,
		Name:    reviewName,
		Audit:   a.Log,
	}
}

// startReviewGateway launches the gateway's create/retry loop. No-op when the
// gateway is not configured.
func (d *Daemon) startReviewGateway(ctx context.Context) {
	if d.reviewGW == nil {
		return
	}
	d.reviewGW.Start(ctx)
}

// reviewGateway returns the daemon's gateway, or nil when not configured.
func (d *Daemon) reviewGateway() mcpgateway.GatewaySource {
	if d.reviewGatewaySrc != nil {
		return d.reviewGatewaySrc
	}
	if d.reviewGW == nil {
		return nil
	}
	return d.reviewGW
}
