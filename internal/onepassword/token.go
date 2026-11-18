package onepassword

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Environment variables consulted by ResolveToken.
const (
	EnvToken           = "OP_SERVICE_ACCOUNT_TOKEN"
	EnvKeychainService = "OP_SA_KEYCHAIN_SERVICE"
	EnvKeychainAccount = "OP_SA_KEYCHAIN_ACCOUNT"
	EnvKeychain        = "OP_SA_KEYCHAIN"
)

// Defaults for the keychain item holding the token.
const (
	DefaultKeychainService = "workingman:service_account"
	DefaultKeychainAccount = "jim.clark"
)

// KeychainItem identifies a generic-password item in the macOS keychain.
type KeychainItem struct {
	Service string
	Account string
	// Keychain is the keychain to search (name or path); "" searches the
	// user's default keychain search list.
	Keychain string
}

func (k KeychainItem) String() string {
	s := fmt.Sprintf("service %q, account %q", k.Service, k.Account)
	if k.Keychain != "" {
		s += fmt.Sprintf(", keychain %q", k.Keychain)
	} else {
		s += ", default keychain search list"
	}
	return s
}

// KeychainLookup returns the password of the keychain item. Implementations
// must not include the password in a returned error.
type KeychainLookup func(ctx context.Context, item KeychainItem) ([]byte, error)

// TokenSource resolves the service account token. The zero value is not
// usable; DefaultTokenSource returns the one ResolveToken uses.
type TokenSource struct {
	// Getenv reads an environment variable (os.Getenv).
	Getenv func(string) string
	// Keychain reads a keychain item (SecurityKeychainLookup).
	Keychain KeychainLookup
}

// DefaultTokenSource reads the process environment and the macOS keychain
// via the `security` tool.
func DefaultTokenSource() TokenSource {
	return TokenSource{Getenv: os.Getenv, Keychain: SecurityKeychainLookup}
}

// ResolveToken resolves the service account token with DefaultTokenSource.
func ResolveToken(ctx context.Context) (string, error) {
	return DefaultTokenSource().Resolve(ctx)
}

// KeychainItem returns the keychain item to read, after environment
// overrides.
func (s TokenSource) KeychainItem() KeychainItem {
	item := KeychainItem{
		Service:  strings.TrimSpace(s.Getenv(EnvKeychainService)),
		Account:  strings.TrimSpace(s.Getenv(EnvKeychainAccount)),
		Keychain: strings.TrimSpace(s.Getenv(EnvKeychain)),
	}
	if item.Service == "" {
		item.Service = DefaultKeychainService
	}
	if item.Account == "" {
		item.Account = DefaultKeychainAccount
	}
	return item
}

// Resolve returns the service account token: from EnvToken if set, else from
// the keychain. See the package doc.
func (s TokenSource) Resolve(ctx context.Context) (string, error) {
	if tok := strings.TrimSpace(s.Getenv(EnvToken)); tok != "" {
		return tok, nil
	}
	item := s.KeychainItem()
	out, err := s.Keychain(ctx, item)
	if err != nil {
		return "", fmt.Errorf("onepassword: no 1Password service account token: %s is not set and reading it from the keychain (%s) failed: %w; %s",
			EnvToken, item, err, seedHint(item))
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return "", fmt.Errorf("onepassword: no 1Password service account token: %s is not set and the keychain item (%s) is empty; %s",
			EnvToken, item, seedHint(item))
	}
	return tok, nil
}

func seedHint(item KeychainItem) string {
	cmd := fmt.Sprintf("security add-generic-password -U -a %s -s %s -w", item.Account, item.Service)
	if item.Keychain != "" {
		// A keychain argument cannot follow the trailing -w that makes
		// security prompt; with the keychain as the default one it need not.
		cmd = fmt.Sprintf("security default-keychain -s %s && %s", item.Keychain, cmd)
	}
	return fmt.Sprintf("set %s, or store the token in the macOS keychain with `%s` (it prompts for the token)", EnvToken, cmd)
}

// ErrKeychainUnsupported is returned by SecurityKeychainLookup off macOS.
var ErrKeychainUnsupported = errors.New("the keychain is only available on macOS")

// securityBin is the macOS keychain tool SecurityKeychainLookup runs.
var securityBin = "/usr/bin/security"

// SecurityKeychainLookup reads a generic password with `security
// find-generic-password -w`. The password is read from its stdout; it is
// never on the command line. Off macOS it returns ErrKeychainUnsupported.
func SecurityKeychainLookup(ctx context.Context, item KeychainItem) ([]byte, error) {
	if runtime.GOOS != "darwin" {
		return nil, ErrKeychainUnsupported
	}
	args := []string{"find-generic-password", "-a", item.Account, "-s", item.Service, "-w"}
	if item.Keychain != "" {
		args = append(args, item.Keychain)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, securityBin, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// stderr holds security's diagnostic ("The specified item could not
		// be found in the keychain."); the password only ever goes to stdout,
		// which is dropped here.
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s: %w", msg, err)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}
