package channels

import (
	"encoding/json"
	"log/slog"
)

// Secret is a credential value that never prints itself. fmt verbs, JSON,
// and slog all render it as "[REDACTED]", so a token cannot leak into logs
// or the audit trail by accident; call Reveal at the point of use.
type Secret struct{ v string }

const redacted = "[REDACTED]"

// NewSecret wraps v.
func NewSecret(v string) Secret { return Secret{v: v} }

// Reveal returns the underlying value.
func (s Secret) Reveal() string { return s.v }

// Empty reports whether the secret holds no value.
func (s Secret) Empty() bool { return s.v == "" }

func (Secret) String() string               { return redacted }
func (Secret) GoString() string             { return redacted }
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }
func (Secret) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }
func (Secret) LogValue() slog.Value         { return slog.StringValue(redacted) }

// Credentials maps a credential name (as given under `credentials:` in the
// config, e.g. "access_token") to its resolved secret.
type Credentials map[string]Secret
