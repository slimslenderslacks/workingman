package acpwrapper

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/slimslenderslacks/work/internal/sbx"
)

// directAPIURL is where Claude Code goes when ANTHROPIC_BASE_URL is unset.
const directAPIURL = "https://api.anthropic.com"

// curlCertFailure is curl's exit status for "the peer's certificate could not
// be authenticated" (CURLE_PEER_FAILED_VERIFICATION).
const curlCertFailure = "60"

// llmURL is the API endpoint the agent will talk to: the LLM gateway when sbx
// can authenticate it there (c.useGateway), otherwise the default API.
func (c Config) llmURL() string {
	if c.useGateway {
		return "https://" + gatewayHost
	}
	return directAPIURL
}

// llmTLSPreflight checks, from inside the sandbox and through its proxy, that
// the agent's API endpoint presents a certificate chain the sandbox trusts.
// Without it a chain the sandbox cannot verify only surfaces as every agent
// reply reading "Unable to connect to API: Self-signed certificate detected".
// That is what happens when the agent goes to api.anthropic.com directly from
// a network whose TLS-inspecting proxy re-signs it with a CA the sandbox does
// not hold (the sbx proxy trusts it upstream, but passes the corporate chain
// through), which is the case whenever no sbx secret targets the LLM gateway.
//
// curl reads the same CA bundle Claude Code does (SSL_CERT_FILE /
// NODE_EXTRA_CA_CERTS point at it in sbx sandboxes), so a curl verification
// failure is the agent's failure. Only that failure is an error: a sandbox
// without curl, an HTTP error status, or a network problem is left for the
// agent to report, since none of them is a certificate the sandbox rejects.
func llmTLSPreflight(ctx context.Context, run commandFunc, c Config) error {
	url := c.llmURL()
	probe := `command -v curl >/dev/null 2>&1 || { echo "curl=missing"; exit 0; }; ` +
		`curl -sS -o /dev/null --max-time 20 "$1" 2>&1; echo "curl=$?"`
	out, err := run(ctx, c.SbxPath, "exec", c.SandboxName, "--", "sh", "-c", probe, "sh", url)
	text := strings.TrimSpace(string(out))
	if err != nil {
		fmt.Fprintf(os.Stderr, "acp-wrapper: session %s: WARNING could not run the LLM TLS preflight against %s: %v: %s\n", c.SessionID, url, err, text)
		return nil
	}
	status, detail := probeStatus(text, "curl")
	switch status {
	case "0":
		fmt.Fprintf(os.Stderr, "acp-wrapper: session %s: LLM TLS preflight OK (%s)\n", c.SessionID, url)
		return nil
	case "missing":
		fmt.Fprintf(os.Stderr, "acp-wrapper: session %s: LLM TLS preflight skipped: no curl in sandbox %q\n", c.SessionID, c.SandboxName)
		return nil
	case curlCertFailure:
		return fmt.Errorf("acpwrapper: LLM TLS preflight FAILED for sandbox %q: %s presents a certificate chain the sandbox does not trust (%s), so the agent would only answer \"Unable to connect to API: Self-signed certificate detected\"; %s",
			c.SandboxName, url, detail, c.llmTLSHint())
	default:
		fmt.Fprintf(os.Stderr, "acp-wrapper: session %s: WARNING LLM TLS preflight against %s: curl exit %s: %s\n", c.SessionID, url, status, detail)
		return nil
	}
}

// llmTLSHint is the remediation for a failed llmTLSPreflight.
func (c Config) llmTLSHint() string {
	if c.useGateway {
		return "the sbx proxy should re-sign " + gatewayHost + " with its own CA; check `sbx policy log` and the sandbox's CA bundle (/etc/ssl/certs/ca-certificates.crt)"
	}
	hint := "no sbx custom secret targeting " + gatewayHost + " applies to this sandbox, so ANTHROPIC_BASE_URL was left unset and the agent goes to " + directAPIURL +
		" through the network's TLS-inspecting proxy. Configure the LLM gateway secret globally (`sbx secret ls --json` must list a custom secret with scope \"global\" and target " + gatewayHost +
		"); a secret scoped to one sandbox is deleted by sbx when that sandbox is removed, including when acp-wrapper recreates it"
	if c.ExtraCACerts == "" {
		return hint + ". Alternatively, install the network's CA into every sandbox: export the current corporate root (docs/sandbox-custom-ca.md §1.4) and set --extra-ca-certs / " + sbx.ExtraCAEnv
	}
	return hint + ". The configured extra CA (" + c.extraCADescription() + ") does not validate it either: it is the wrong root or expired; re-export it (docs/sandbox-custom-ca.md §1.4) and run ./check-ca.sh " + c.ExtraCACerts
}

// gitProbeURL is the repository gitTLSPreflight runs `git ls-remote` against.
// A variable so tests can override it.
var gitProbeURL = "https://github.com/slimslenderslacks/workingman"

// gitTLSPreflight checks, from inside the sandbox and through its proxy, that
// git over https can verify github.com's certificate chain. On a laptop whose
// VPN inspects TLS, github.com is re-signed with a corporate CA the sandbox
// does not hold unless --extra-ca-certs installed it (docs/sandbox-custom-ca.md
// §1.2), and every clone, fetch and push over https then fails mid-task.
//
// Only a certificate failure is an error, with a hint that depends on whether
// an extra CA is configured. A sandbox without git, a network the sandbox's
// policy denies, DNS or a timeout only warn, like llmTLSPreflight.
func gitTLSPreflight(ctx context.Context, run commandFunc, c Config) error {
	url := gitProbeURL
	probe := `command -v git >/dev/null 2>&1 || { echo "git=missing"; exit 0; }; ` +
		`if command -v timeout >/dev/null 2>&1; then t="timeout 20"; else t=; fi; ` +
		`GIT_TERMINAL_PROMPT=0 $t git ls-remote "$1" HEAD 2>&1 >/dev/null; echo "git=$?"`
	out, err := run(ctx, c.SbxPath, "exec", c.SandboxName, "--", "sh", "-c", probe, "sh", url)
	text := strings.TrimSpace(string(out))
	if err != nil {
		fmt.Fprintf(os.Stderr, "acp-wrapper: session %s: WARNING could not run the git TLS preflight against %s: %v: %s\n", c.SessionID, url, err, text)
		return nil
	}
	status, detail := probeStatus(text, "git")
	switch {
	case status == "0":
		fmt.Fprintf(os.Stderr, "acp-wrapper: session %s: git TLS preflight OK (%s)\n", c.SessionID, url)
		return nil
	case status == "missing":
		fmt.Fprintf(os.Stderr, "acp-wrapper: session %s: git TLS preflight skipped: no git in sandbox %q\n", c.SessionID, c.SandboxName)
		return nil
	// gnutls builds print "SSL certificate verification failed: ...",
	// OpenSSL builds "SSL certificate problem: ...".
	case status != "" && strings.Contains(strings.ToLower(detail), "certificate"):
		return fmt.Errorf("acpwrapper: git TLS preflight FAILED for sandbox %q: https://github.com presents a certificate chain the sandbox does not trust (%s), so git over https would fail; %s",
			c.SandboxName, detail, c.gitTLSHint(detail))
	default:
		fmt.Fprintf(os.Stderr, "acp-wrapper: session %s: WARNING git TLS preflight against %s: git exit %s: %s\n", c.SessionID, url, status, detail)
		return nil
	}
}

// gitTLSHint is the remediation for a failed gitTLSPreflight; detail is what
// git printed.
func (c Config) gitTLSHint(detail string) string {
	if c.ExtraCACerts == "" {
		return "the network re-signs github.com with a CA the sandbox does not hold (VPN TLS inspection). Export the current corporate root (docs/sandbox-custom-ca.md §1.4) and set --extra-ca-certs / " + sbx.ExtraCAEnv
	}
	hint := "the configured extra CA (" + c.extraCADescription() + ") does not validate github.com's chain: it is the wrong root or EXPIRED. Re-export it (docs/sandbox-custom-ca.md §1.4) and run ./check-ca.sh " + c.ExtraCACerts
	if strings.Contains(strings.ToLower(detail), "expired") || c.extraCAExpired(time.Now()) {
		hint = "a certificate has EXPIRED; " + hint
	}
	return hint
}

// extraCADescription names the loaded extra CA for a hint: its path, then
// each certificate's subject and notAfter.
func (c Config) extraCADescription() string {
	parts := []string{c.ExtraCACerts}
	for _, cert := range c.extraCA.Certs {
		parts = append(parts, fmt.Sprintf("%s, notAfter %s", cert.Subject, cert.NotAfter.UTC().Format(time.RFC3339)))
	}
	return strings.Join(parts, ": ")
}

// extraCAExpired reports whether every loaded extra-CA certificate has
// expired by now (false when none is loaded).
func (c Config) extraCAExpired(now time.Time) bool {
	if len(c.extraCA.Certs) == 0 {
		return false
	}
	for _, cert := range c.extraCA.Certs {
		if now.Before(cert.NotAfter) {
			return false
		}
	}
	return true
}

// probeStatus splits a preflight probe's output into the probed command's
// exit status (the last "<key>=<n>" line) and whatever it printed before it.
func probeStatus(out, key string) (status, detail string) {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), key+"="); ok {
			return s, strings.TrimSpace(strings.Join(lines[:i], " "))
		}
	}
	return "", out
}

// warnScopedLLMSecret warns, before a sandbox is removed to be recreated,
// when the LLM gateway secret that lets its agent use ANTHROPIC_BASE_URL is
// scoped to that sandbox only: sbx deletes such a secret with the sandbox, so
// the recreated one falls back to the default API.
func warnScopedLLMSecret(ctx context.Context, run commandFunc, c Config) {
	global, scoped, err := sbx.GatewaySecretScopes(ctx, sbx.RunFunc(run), c.SbxPath, c.SandboxName)
	if err != nil || global || !scoped {
		return
	}
	fmt.Fprintf(os.Stderr, "acp-wrapper: session %s: WARNING the %s secret is scoped to sandbox %q, and sbx deletes it when the sandbox is removed: the recreated sandbox will not use the LLM gateway. Make the secret global.\n",
		c.SessionID, gatewayHost, c.SandboxName)
}
