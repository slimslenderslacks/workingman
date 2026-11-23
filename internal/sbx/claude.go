package sbx

import (
	"context"
)

// RunFunc runs one command and returns its output. Production callers pass a
// real exec; tests substitute a fake so nothing shells out to sbx.
type RunFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// LLMGatewayHost is the LLM gateway a sandboxed Claude Code agent is pointed
// at (ANTHROPIC_BASE_URL) when sbx holds a secret for it (see
// GatewaySecretConfigured).
const LLMGatewayHost = "llm.es.atko.services"

// ClaudeEnv is the KEY=VALUE environment every sandboxed Claude Code agent
// gets: the opus/sonnet/haiku aliases resolve to the gateway's model ids, and
// the nonessential traffic and experimental betas the gateway doesn't support
// are switched off. ANTHROPIC_BASE_URL is not here: it is added only when
// GatewaySecretConfigured says sbx can authenticate to LLMGatewayHost.
var ClaudeEnv = []string{
	"ANTHROPIC_DEFAULT_OPUS_MODEL=claude-opus-5-5[1m]",
	"ANTHROPIC_DEFAULT_SONNET_MODEL=claude-sonnet-5-5[1m]",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL=claude-haiku-5-5",
	"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1",
}

// GatewayBaseURLEnv is the ANTHROPIC_BASE_URL entry that routes the agent
// through LLMGatewayHost.
const GatewayBaseURLEnv = "ANTHROPIC_BASE_URL=https://" + LLMGatewayHost

// GatewaySecretConfigured reports whether `sbx secret ls --json` lists a
// custom secret targeting LLMGatewayHost that applies to sandbox — one scoped
// globally or to that sandbox. sbx's proxy swaps the real credential in only
// for requests to that host, so without one ANTHROPIC_BASE_URL must stay unset.
func GatewaySecretConfigured(ctx context.Context, run RunFunc, sbxPath, sandbox string) (bool, error) {
	global, scoped, err := GatewaySecretScopes(ctx, run, sbxPath, sandbox)
	return global || scoped, err
}

// GatewaySecretScopes reports which custom secrets targeting LLMGatewayHost
// apply to sandbox: a global one, and one scoped to sandbox itself. The
// difference matters because sbx deletes a sandbox-scoped secret when that
// sandbox is removed, so recreating the sandbox loses it. It runs `sbx secret
// ls --json` once, without Client's retries or lock, but decodes as
// tolerantly (DecodeSecretList).
func GatewaySecretScopes(ctx context.Context, run RunFunc, sbxPath, sandbox string) (global, scoped bool, err error) {
	return Client{Run: Combined(run), SbxPath: sbxPath, Attempts: 1}.GatewayScopes(ctx, sandbox)
}
