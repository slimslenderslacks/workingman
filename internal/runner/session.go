package runner

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/slimslenderslacks/work/internal/mcpgateway"
	"github.com/slimslenderslacks/work/internal/sbx"
)

// The `:session` sandbox follows sbx-create.sh step for step, so a session
// opened from the TUI and one made by hand are the same thing:
//
//  1. mint a per-sandbox MCP gateway (dynamic, over SessionMCPServers — none
//     by default, so the agent attaches what it needs with mcp-add);
//  2. `sbx create claude --kit <Kit> -t <Image> --pull missing -e MCP_URL=…
//     -e <claude env> <workspaces…>`, then allow the gateway's port and record
//     the gateway;
//  3. attach the tab with `sbx run --name <sandbox> -e …`, so the pty runs the
//     sandbox's own interactive claude. With acp-kit's image that `claude` is
//     its shim, which registers MCP_URL as the `okta-gateway` MCP server.
//
// An existing sandbox with the same mounts is reused (its gateway reused or
// replaced); one with different mounts is removed and recreated.

// sessionGateway is the gateway manager for a `:session` sandbox: dynamic
// unless MCPGatewayStatic, over SessionMCPServers.
func (r *Runner) sessionGateway() (mcpgateway.Manager, error) {
	return r.mcpGateway(nonBlank(r.SessionMCPServers), !r.MCPGatewayStatic)
}

func (r *Runner) sbxBin() string {
	if r.SbxPath != "" {
		return r.SbxPath
	}
	return "sbx"
}

func (r *Runner) sbxRunner() sbx.RunFunc {
	if r.sbxRun != nil {
		return r.sbxRun
	}
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
}

// sessionEnv is the `-e` flags shared by `sbx create` and `sbx run`: MCP_URL
// (when there is a gateway), the claude env, and ANTHROPIC_BASE_URL when sbx
// can authenticate to the LLM gateway.
func sessionEnv(mcpURL string, useGateway bool) []string {
	var args []string
	if mcpURL != "" {
		args = append(args, "-e", mcpgateway.EnvVar+"="+mcpURL)
	}
	for _, kv := range sbx.ClaudeEnv {
		args = append(args, "-e", kv)
	}
	if useGateway {
		args = append(args, "-e", sbx.GatewayBaseURLEnv)
	}
	return args
}

// ensureSessionSandbox makes the `:session` sandbox exist with exactly
// workspaces mounted and returns the env (`-e` flags) to attach with.
func (r *Runner) ensureSessionSandbox(ctx context.Context, name string, workspaces []string, gw mcpgateway.Manager) ([]string, error) {
	run, bin := r.sbxRunner(), r.sbxBin()
	useGateway, err := sbx.GatewaySecretConfigured(ctx, run, bin, name)
	if err != nil {
		r.audit("session_llm_gateway_unknown", "sandbox", name, "err", err.Error())
	}
	gwRules, err := gw.PolicyRules()
	if err != nil {
		return nil, fmt.Errorf("mcp gateway policy: %w", err)
	}

	out, err := run(ctx, bin, "ls", "--json")
	if err != nil {
		return nil, fmt.Errorf("sbx ls: %w: %s", err, strings.TrimSpace(string(out)))
	}
	list, err := sbx.DecodeList(out)
	if err != nil {
		return nil, fmt.Errorf("decode sbx ls output: %w", err)
	}
	var existing []string
	found := false
	for _, s := range list {
		if s.Name == name {
			existing, found = s.Workspaces, true
		}
	}
	if found && !sameWorkspaceSet(existing, workspaces) {
		if out, err := run(ctx, bin, "rm", "--force", name); err != nil {
			return nil, fmt.Errorf("sbx rm %s: %w: %s", name, err, strings.TrimSpace(string(out)))
		}
		gw.Release(ctx, name)
		found = false
	}

	if !found {
		// Mint before the sandbox exists so MCP_URL is baked into it; record
		// once the sandbox and its policies are in place, DELETE on failure.
		rec := gw.Prepare(ctx, name)
		fail := func(err error) ([]string, error) {
			gw.Discard(ctx, rec)
			return nil, err
		}
		args := []string{"create", "claude", "--name", name}
		args = append(args, sessionEnv(rec.SandboxURL, useGateway)...)
		if kit := strings.TrimSpace(r.Kit); kit != "" {
			args = append(args, "--kit", kit)
		}
		if img := strings.TrimSpace(r.Image); img != "" {
			args = append(args, "-t", img, "--pull", "missing")
		}
		args = append(args, workspaces...)
		if out, err := run(ctx, bin, args...); err != nil {
			return fail(fmt.Errorf("sbx create %s: %w: %s", name, err, strings.TrimSpace(string(out))))
		}
		for _, rule := range gwRules {
			if out, err := run(ctx, bin, rule.CLIArgs(name)...); err != nil {
				return fail(fmt.Errorf("sbx policy %s %s %s %s: %w: %s",
					rule.Action, rule.Kind, name, rule.Resource, err, strings.TrimSpace(string(out))))
			}
		}
		if err := gw.Save(rec); err != nil {
			gw.Discard(ctx, rec)
		}
	} else {
		// Best effort, as acp-wrapper does: a sandbox created before the gateway
		// was configured can still reach it.
		for _, rule := range gwRules {
			_, _ = run(ctx, bin, rule.CLIArgs(name)...)
		}
	}

	// Reuse the recorded gateway, or replace a gone one. MCP_URL is passed on
	// every attach, and also written to the sandbox's persistent env file so a
	// shell opened in it sees the gateway the shim registers.
	mcpURL := ""
	if gw.Enabled() {
		rec, err := gw.Ensure(ctx, name)
		if err != nil {
			r.audit("mcp_gateway_failed", "sandbox", name, "error", err.Error())
		} else {
			mcpURL = rec.SandboxURL
			if out, err := run(ctx, bin, mcpgateway.PersistEnvArgs(name, mcpURL)...); err != nil {
				r.audit("mcp_gateway_persist_failed", "sandbox", name, "error", strings.TrimSpace(err.Error()+": "+string(out)))
			}
		}
	}
	return sessionEnv(mcpURL, useGateway), nil
}

// sessionAgentArgs are the claude arguments that open (or resume) the stable
// conversation id. `claude --session-id <id>` only CREATES a conversation and
// fails if one exists, so the transcript is probed first: the sandbox's
// ~/.claude/projects is a persistent volume, so an existing transcript means
// --resume. The id is a v5 UUID (hex and hyphens), safe to interpolate.
func (r *Runner) sessionAgentArgs(ctx context.Context, name, id string) []string {
	if id == "" {
		return nil
	}
	probe := fmt.Sprintf(`ls ~/.claude/projects/*/%s.jsonl >/dev/null 2>&1`, id)
	if _, err := r.sbxRunner()(ctx, r.sbxBin(), "exec", name, "--", "sh", "-c", probe); err == nil {
		return []string{"--resume", id}
	}
	return []string{"--session-id", id}
}

func (r *Runner) audit(event string, kv ...string) {
	if r.Audit != nil {
		r.Audit.Log(event, kv...)
	}
}
