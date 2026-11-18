package onepassword

import (
	"context"
	"encoding/pem"
	"fmt"
	"net/url"
	"runtime/debug"
	"strings"

	sdk "github.com/1password/onepassword-sdk-go"
	"golang.org/x/crypto/ssh"
)

// IntegrationName is the integration name the SDK client reports to
// 1Password.
const IntegrationName = "workingman"

// Resolver resolves an op:// secret reference. It is the one SDK call Read
// needs: (*onepassword.Client).Secrets() satisfies it.
type Resolver interface {
	Resolve(ctx context.Context, secretReference string) (string, error)
}

// newClient is sdk.NewClient, replaceable in tests.
var newClient = sdk.NewClient

// New resolves the service account token (ResolveToken) and returns an SDK
// client authenticated with it.
func New(ctx context.Context) (*sdk.Client, error) {
	tok, err := ResolveToken(ctx)
	if err != nil {
		return nil, err
	}
	return NewWithToken(ctx, tok)
}

// NewWithToken returns an SDK client authenticated with the service account
// token tok.
func NewWithToken(ctx context.Context, tok string) (*sdk.Client, error) {
	c, err := newClient(ctx,
		sdk.WithServiceAccountToken(tok),
		sdk.WithIntegrationInfo(IntegrationName, Version()),
	)
	if err != nil {
		return nil, &authError{err: err, msg: redact(err.Error(), tok)}
	}
	return c, nil
}

// authError is a failure to build an SDK client. Its message is the SDK's
// with the token redacted; Unwrap still reaches the SDK error.
type authError struct {
	err error
	msg string
}

func (e *authError) Error() string {
	return fmt.Sprintf("onepassword: authenticating to 1Password as a service account: %s; check that the token (from %s or the keychain) is a current service account token that has not been revoked, and that the service account can reach 1Password", e.msg, EnvToken)
}

func (e *authError) Unwrap() error { return e.err }

// redact removes tok from s, in case an SDK error ever echoes it.
func redact(s, tok string) string {
	if tok == "" {
		return s
	}
	return strings.ReplaceAll(s, tok, "[REDACTED]")
}

// Read resolves the secret reference ref with r. When ref asks for
// `ssh-format=openssh`, the result is guaranteed to be an OpenSSH private
// key: a PEM key in another format is converted (see the package doc).
// Errors name the reference, never the secret.
func Read(ctx context.Context, r Resolver, ref string) (string, error) {
	v, err := r.Resolve(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("onepassword: resolving %s: %w; the service account must have read access to the vault in the reference (it cannot see a Personal vault)", ref, err)
	}
	if wantsOpenSSH(ref) {
		if v, err = toOpenSSH(v); err != nil {
			return "", fmt.Errorf("onepassword: %s: %w", ref, err)
		}
	}
	return v, nil
}

// wantsOpenSSH reports whether ref carries the ssh-format=openssh query.
func wantsOpenSSH(ref string) bool {
	_, query, ok := strings.Cut(ref, "?")
	if !ok {
		return false
	}
	q, err := url.ParseQuery(query)
	return err == nil && strings.EqualFold(q.Get("ssh-format"), "openssh")
}

const openSSHBlock = "OPENSSH PRIVATE KEY"

// toOpenSSH returns key as an OpenSSH-format private key PEM, converting a
// PKCS#1, PKCS#8 or SEC 1 PEM key. Its errors never include key material.
func toOpenSSH(key string) (string, error) {
	block, _ := pem.Decode([]byte(key))
	if block == nil {
		return "", fmt.Errorf("value is not a PEM private key; point the reference at an SSH key's private key field")
	}
	if block.Type == openSSHBlock {
		if !strings.HasSuffix(key, "\n") {
			key += "\n"
		}
		return key, nil
	}
	raw, err := ssh.ParseRawPrivateKey([]byte(key))
	if err != nil {
		return "", fmt.Errorf("parsing %s PEM block as a private key failed", block.Type)
	}
	out, err := ssh.MarshalPrivateKey(raw, "")
	if err != nil {
		return "", fmt.Errorf("converting %s private key to OpenSSH format failed", block.Type)
	}
	return string(pem.EncodeToMemory(out)), nil
}

// Version is the version reported to 1Password as the integration version:
// the main module's version from the build info, made acceptable to the SDK
// (see sdkVersion), or "dev" when there is none (a plain `go build` outside a
// VCS checkout).
func Version() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		return sdkVersion(bi.Main.Version)
	}
	return "dev"
}

// maxVersionLen is the SDK's limit on the integration version's length.
const maxVersionLen = 20

// sdkVersion maps a Go module version to one the SDK accepts: at most
// maxVersionLen characters and no '+'. Since Go 1.24 `go build` stamps
// VCS pseudo-versions such as v0.0.0-20251117232700-0123456789ab+dirty, which
// the SDK rejects ("integration version was too long", "unallowed char: '+'")
// and so fails to build a client at all. Build metadata is dropped and an
// over-long version is cut to its release part (v0.0.0); anything still
// unusable becomes "dev".
func sdkVersion(v string) string {
	if v == "" || v == "(devel)" {
		return "dev"
	}
	v, _, _ = strings.Cut(v, "+")
	if len(v) > maxVersionLen {
		v, _, _ = strings.Cut(v, "-")
	}
	if v == "" || len(v) > maxVersionLen || strings.Trim(v, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ.-") != "" {
		return "dev"
	}
	return v
}
