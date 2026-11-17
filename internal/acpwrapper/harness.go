package acpwrapper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/mcpgateway"
)

// harnessRecord is what a sandbox was built from: the kit layered on at
// `sbx create` (whose install step writes claude-acp-client) and the template
// image. It is recorded on the host because neither is visible from `sbx ls`,
// and a reused sandbox keeps whatever entrypoint its creating kit installed —
// so a sandbox made by an older kit keeps the stock `mcp-gateway` MCP server
// for as long as it is reused (see Config.EnsureOktaGateway).
type harnessRecord struct {
	Sandbox string `json:"sandbox"`
	// Kit is the --kit reference; KitDigest the sha256 of its spec.yaml when
	// it is a local kit directory (empty for a published ref).
	Kit       string    `json:"kit"`
	KitDigest string    `json:"kit_digest,omitempty"`
	Image     string    `json:"image,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// harnessStateDir is where harness records live: a "sandboxes" directory
// beside the sessions root (like mcpgateway.StateDir), since a kept sandbox
// outlives its session directory.
func harnessStateDir(sessionsRoot string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(sessionsRoot)), "sandboxes")
}

func (c Config) harnessPath(sandbox string) string {
	return filepath.Join(harnessStateDir(c.SessionsRoot), sandbox+".json")
}

// kitDigest is the sha256 of a local kit's spec.yaml — the file whose install
// step writes claude-acp-client — or "" when kit is not a local directory.
func kitDigest(kit string) string {
	data, err := os.ReadFile(filepath.Join(kit, "spec.yaml"))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// currentHarness is the harness a sandbox created now would get.
func (c Config) currentHarness(sandbox string) harnessRecord {
	return harnessRecord{
		Sandbox:   sandbox,
		Kit:       c.KitPath,
		KitDigest: kitDigest(c.KitPath),
		Image:     strings.TrimSpace(c.TemplateImage),
	}
}

func (c Config) loadHarness(sandbox string) (harnessRecord, bool) {
	data, err := os.ReadFile(c.harnessPath(sandbox))
	if err != nil {
		return harnessRecord{}, false
	}
	var rec harnessRecord
	if json.Unmarshal(data, &rec) != nil {
		return harnessRecord{}, false
	}
	return rec, true
}

// saveHarness records what sandbox was just created from. Best-effort: a
// missing record only makes a later EnsureOktaGateway launch recreate it.
func (c Config) saveHarness(sandbox string) {
	if c.SessionsRoot == "" {
		return // not normalized (tests calling createSandbox directly)
	}
	rec := c.currentHarness(sandbox)
	rec.CreatedAt = time.Now()
	data, err := json.MarshalIndent(rec, "", "  ")
	if err == nil {
		if err = os.MkdirAll(harnessStateDir(c.SessionsRoot), 0o755); err == nil {
			err = os.WriteFile(c.harnessPath(sandbox), append(data, '\n'), 0o644)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "acp-wrapper: session %s: WARNING could not record harness of sandbox %q: %v\n", c.SessionID, sandbox, err)
	}
}

func (c Config) removeHarness(sandbox string) {
	if c.SessionsRoot == "" {
		return
	}
	if err := os.Remove(c.harnessPath(sandbox)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "acp-wrapper: session %s: WARNING %v\n", c.SessionID, err)
	}
}

// harnessDrift says why an existing sandbox was not built from the current
// kit and image, or "" when it was. A sandbox with no record predates this
// bookkeeping (or was made by hand), so its kit is unknown and it counts as
// drifted.
func (c Config) harnessDrift(sandbox string) string {
	rec, ok := c.loadHarness(sandbox)
	if !ok {
		return "no record of the kit/image it was created from"
	}
	cur := c.currentHarness(sandbox)
	switch {
	case rec.Kit != cur.Kit:
		return fmt.Sprintf("created with kit %q, now %q", rec.Kit, cur.Kit)
	case rec.KitDigest != cur.KitDigest:
		return fmt.Sprintf("kit %q changed since the sandbox was created (spec.yaml %s -> %s)", cur.Kit, short(rec.KitDigest), short(cur.KitDigest))
	case rec.Image != cur.Image:
		return fmt.Sprintf("created from image %q, now %q", orStock(rec.Image), orStock(cur.Image))
	}
	return ""
}

func short(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	if digest == "" {
		return "none"
	}
	return digest
}

func orStock(img string) string {
	if img == "" {
		return StockTemplate
	}
	return img
}

// entrypointMarker is a string only a claude-acp-client new enough to register
// okta-gateway, remove `mcp-gateway` and support register-only mode contains.
const entrypointMarker = "CLAUDE_ACP_REGISTER_ONLY"

// oktaGatewayPreflight checks, before the agent starts, that the sandbox will
// see `okta-gateway` at c.mcpURL and not the stock `mcp-gateway`: the
// installed claude-acp-client must be one that removes mcp-gateway (a sandbox
// created by an older kit keeps an older one), and running its register-only
// mode — in the agent's working directory, so local and project scopes match
// what the agent sees — must leave okta-gateway at MCP_URL and mcp-gateway
// gone. It returns "" when all holds, otherwise what is wrong; recreate is
// true when a fresh sandbox can fix it (a stale entrypoint, a failed
// registration), false when it cannot (no MCP_URL, or an mcp-gateway entry
// from a project's .mcp.json, which claude-acp-client never edits).
func oktaGatewayPreflight(ctx context.Context, run commandFunc, c Config) (problem string, recreate bool) {
	if c.mcpURL == "" {
		return "no MCP_URL for this sandbox, so claude-acp-client leaves the stock mcp-gateway server in place", false
	}
	exec := func(env []string, argv ...string) ([]byte, error) {
		args := []string{"exec"}
		for _, kv := range env {
			args = append(args, "-e", kv)
		}
		if cwd := c.primaryWorkspace(); cwd != "" {
			args = append(args, "-w", cwd)
		}
		args = append(append(args, c.SandboxName, "--"), argv...)
		return run(ctx, c.SbxPath, args...)
	}
	check := `grep -q ` + entrypointMarker + ` "$(command -v claude-acp-client)"`
	if _, err := exec(nil, "sh", "-c", check); err != nil {
		return "the sandbox's claude-acp-client is from a kit too old to register okta-gateway and remove mcp-gateway", true
	}
	if out, err := exec([]string{mcpgateway.EnvVar + "=" + c.mcpURL, "CLAUDE_ACP_REGISTER_ONLY=1"}, "claude-acp-client"); err != nil {
		return fmt.Sprintf("claude-acp-client register-only failed: %v: %s", err, strings.TrimSpace(string(out))), true
	}
	out, err := exec(nil, "claude", "mcp", "get", "okta-gateway")
	if err != nil || !strings.Contains(string(out), c.mcpURL) {
		return fmt.Sprintf("okta-gateway is not registered at %s: %s", c.mcpURL, strings.TrimSpace(string(out))), true
	}
	if _, err := exec(nil, "claude", "mcp", "get", "mcp-gateway"); err == nil {
		return "the stock mcp-gateway MCP server is still configured next to okta-gateway (a project-scope .mcp.json in the workspace?)", false
	}
	return "", false
}

// requireOktaGateway is Run's RequireOktaGateway check, once the sandbox and
// its gateway exist. A problem a fresh sandbox can fix removes the sandbox
// (with its gateway) and fails the launch, so the daemon's retry creates it
// from the current kit and image; one it cannot fix is only warned about, so
// the agent is not stuck in a recreate loop. No MCP_URL at all fails the
// launch without touching the sandbox.
func requireOktaGateway(ctx context.Context, run commandFunc, c Config) error {
	if c.mcpURL == "" {
		// The gateway was configured but could not be minted (see
		// ensureMCPGateway's warning). The sandbox is fine; the retry may not be.
		return fmt.Errorf("acpwrapper: okta-gateway required but no MCP gateway could be minted for sandbox %q; not starting the agent with the stock mcp-gateway server", c.SandboxName)
	}
	problem, recreate := oktaGatewayPreflight(ctx, run, c)
	if problem == "" {
		fmt.Fprintf(os.Stderr, "acp-wrapper: session %s: okta-gateway preflight OK (kit %s, template %s, MCP_URL %s)\n",
			c.SessionID, c.KitPath, c.Harness(), c.mcpURL)
		return nil
	}
	if !recreate {
		fmt.Fprintf(os.Stderr, "acp-wrapper: session %s: WARNING okta-gateway preflight: %s\n", c.SessionID, problem)
		return nil
	}
	if out, err := run(ctx, c.SbxPath, "rm", "--force", c.SandboxName); err != nil {
		fmt.Fprintf(os.Stderr, "acp-wrapper: session %s: WARNING sbx rm --force %q: %v: %s\n",
			c.SessionID, c.SandboxName, err, strings.TrimSpace(string(out)))
	} else {
		c.MCPGateway.Release(ctx, c.SandboxName)
		c.removeHarness(c.SandboxName)
	}
	return fmt.Errorf("acpwrapper: okta-gateway preflight FAILED for sandbox %q (kit %s, template %s): %s; removed the sandbox so the next launch recreates it",
		c.SandboxName, c.KitPath, c.Harness(), problem)
}

// AbsKit returns kit as an absolute path when it names an existing local
// directory, and unchanged otherwise (a published OCI/git reference).
func AbsKit(kit string) string {
	kit = strings.TrimSpace(kit)
	if kit == "" || filepath.IsAbs(kit) {
		return kit
	}
	if info, err := os.Stat(kit); err == nil && info.IsDir() {
		if abs, err := filepath.Abs(kit); err == nil {
			return abs
		}
	}
	return kit
}
