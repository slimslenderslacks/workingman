package setup

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/slimslenderslacks/work/internal/channels/whatsapp"
)

var (
	digitsRE = regexp.MustCompile(`^[0-9]+$`)
	hexRE    = regexp.MustCompile(`^[0-9a-fA-F]+$`)
)

// ValidatePhoneNumberID checks the Meta phone number id (not the phone number).
// Pasting the actual number is the most common setup mistake, so a value that
// is phone-number-sized gets a pointed message, as in hermes.
func ValidatePhoneNumberID(v string) error {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return errors.New("Phone Number ID is required")
	case !digitsRE.MatchString(v):
		return errors.New("Phone Number ID must be numeric (no '+', spaces, or dashes)")
	case len(v) >= 10 && len(v) <= 12:
		return errors.New("that looks like a phone number, but this field needs the Phone Number ID " +
			"(Meta's internal id, 15-17 digits). It is shown just below the 'From' dropdown in " +
			"App Dashboard > WhatsApp > API Setup")
	case len(v) < 13:
		return errors.New("Phone Number ID looks too short (expected 13-18 digits)")
	case len(v) > 20:
		return errors.New("Phone Number ID looks too long (expected 13-18 digits)")
	}
	return nil
}

// ValidateWABAID checks the optional WhatsApp Business Account id.
func ValidateWABAID(v string) error {
	v = strings.TrimSpace(v)
	switch {
	case !digitsRE.MatchString(v):
		return errors.New("WABA ID must be numeric")
	case len(v) < 10 || len(v) > 25:
		return errors.New("WABA ID looks wrong (expected 10-25 digits)")
	}
	return nil
}

// foreignTokens are common paste mistakes for the access-token field.
var foreignTokens = []struct {
	prefixes []string
	what     string
}{
	{[]string{"sk-"}, "that's an OpenAI key (starts with 'sk-')"},
	{[]string{"xoxb-", "xoxp-"}, "that's a Slack token"},
	{[]string{"ghp_", "gho_", "github_pat_"}, "that's a GitHub token"},
}

// ValidateAccessToken checks the shape of a Meta access token (starts "EAA",
// 100+ characters). The value is never included in the error.
func ValidateAccessToken(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("Access Token is required")
	}
	if !strings.HasPrefix(v, "EAA") {
		for _, f := range foreignTokens {
			for _, p := range f.prefixes {
				if strings.HasPrefix(v, p) {
					return fmt.Errorf("%s, not a Meta WhatsApp access token (those start with 'EAA')", f.what)
				}
			}
		}
		return errors.New("Meta WhatsApp access tokens start with 'EAA'; copy one from App Dashboard > " +
			"WhatsApp > API Setup > 'Generate access token', or a System User token from Business Settings")
	}
	if strings.ContainsAny(v, " \t\r\n") {
		return errors.New("Access Token must not contain whitespace")
	}
	if len(v) < 100 {
		return fmt.Errorf("Access Token looks too short (%d characters, expected 100+)", len(v))
	}
	return nil
}

// ValidateAppSecret checks the app secret: 32 hex characters.
func ValidateAppSecret(v string) error {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return errors.New("App Secret is required")
	case !hexRE.MatchString(v):
		return errors.New("App Secret should be a hex string (digits 0-9, letters a-f); copy the " +
			"'App secret' from App Dashboard > Settings > Basic, not some other token")
	case len(v) != 32:
		return fmt.Errorf("App Secret should be exactly 32 hex characters (got %d)", len(v))
	}
	return nil
}

// ValidateVerifyToken checks a user-chosen webhook verify token.
func ValidateVerifyToken(v string) error {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return errors.New("Verify Token is required")
	case len(v) < 8:
		return errors.New("Verify Token is too short (use at least 8 characters, or let setup generate one)")
	case strings.ContainsAny(v, " \t\r\n&=#?%"):
		return errors.New("Verify Token must not contain whitespace or URL-special characters (& = # ? %)")
	}
	return nil
}

// GenerateVerifyToken returns 32 random bytes, base64url-encoded.
func GenerateVerifyToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate verify token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NormalizeAllowFrom turns owner numbers ("+1 (555) 123-4567", "1555...@lid")
// into the bare ids stored in access.allow_from. Entries may come comma- or
// whitespace-separated. "*" is refused: opening the channel to everyone is
// `access.allow_all`, which setup never writes. Duplicates are dropped.
func NormalizeAllowFrom(entries []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, e := range entries {
		for _, part := range strings.Split(e, ",") {
			raw := strings.TrimSpace(part)
			if raw == "" {
				continue
			}
			if raw == "*" {
				return nil, errors.New("'*' is not allowed; list the owner phone numbers (access.allow_all is a separate, deliberate opt-in)")
			}
			id := raw
			if !strings.Contains(raw, "@") {
				id = digitsOnly(raw) // "+1 (555) 123-4567"
			} else {
				id = whatsapp.NormalizeID(raw)
			}
			if !digitsRE.MatchString(id) {
				return nil, fmt.Errorf("%q is not a phone number (use digits with country code, e.g. 15551234567)", raw)
			}
			if len(id) < 7 || len(id) > 20 {
				return nil, fmt.Errorf("%q does not look like a full phone number with country code", raw)
			}
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	// Let the real policy compiler have the last word.
	if _, err := whatsapp.NewAccessPolicy(whatsapp.AccessConfig{AllowFrom: out}); err != nil {
		return nil, err
	}
	return out, nil
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Mask renders a secret for display without revealing it: nothing of a short
// value, only the last four characters of a long one, plus its length.
func Mask(v string) string {
	switch n := len(v); {
	case n == 0:
		return "(not set)"
	case n < 16:
		return "****"
	default:
		return fmt.Sprintf("****%s (%d chars)", v[n-4:], n)
	}
}

// MaskID shortens an id (phone number id, phone number) to its last four
// digits: "***4567". Used for the allowlist, which is personal data.
func MaskID(id string) string { return whatsapp.RedactID(id) }
