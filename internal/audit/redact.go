package audit

import "regexp"

// Redacted replaces any secret-looking substring Redact finds.
const Redacted = "[REDACTED]"

// redactors are applied in order. They err on the side of over-redacting
// well-known credential shapes, and deliberately do NOT blanket-redact long
// hex/base64 runs: those would swallow the git commit SHAs that audit lines
// and task records legitimately carry.
var redactors = []struct {
	re   *regexp.Regexp
	repl string
}{
	// PEM private key blocks (possibly multi-line, possibly with escaped \n).
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`), Redacted},
	// name=value / name: value where the name says it is a credential.
	{regexp.MustCompile(`(?i)\b([A-Za-z0-9_.-]*(?:token|secret|passw(?:or)?d|passwd|api[_-]?key|access[_-]?key|private[_-]?key|authorization|credential)[A-Za-z0-9_.-]*)(["']?\s*[=:]\s*)(?:(?:bearer|basic)\s+)?("[^"]*"|'[^']*'|[^\s,;"']+)`), "${1}${2}" + Redacted},
	// Authorization-style bearer/basic credentials.
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "${1} " + Redacted},
	// user:password@ in URLs.
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^\s/@:]+:[^\s/@]+@`), "${1}" + Redacted + "@"},
	// Well-known token formats.
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`), Redacted},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`), Redacted},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`), Redacted},
	{regexp.MustCompile(`\bxox[abeprs]-[A-Za-z0-9-]{10,}`), Redacted},
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), Redacted},
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}\b`), Redacted},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*`), Redacted},
}

// Redact masks anything in s that looks like a credential — tokens, API keys,
// passwords, bearer headers, URL userinfo, private-key blocks — so audit lines
// and free-text fields can be published (e.g. in the daemon's state snapshot)
// to readers that must not see secrets. It is idempotent.
func Redact(s string) string {
	for _, r := range redactors {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}
