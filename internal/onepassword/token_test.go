package onepassword

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	sdk "github.com/1password/onepassword-sdk-go"
)

const testToken = "ops_eyJzZWNyZXQiOiJ0ZXN0LXRva2VuLWRvLW5vdC1sZWFrIn0"

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// recordingKeychain returns a KeychainLookup answering with out/err and
// recording each item it was asked for.
func recordingKeychain(out []byte, err error, got *[]KeychainItem) KeychainLookup {
	return func(_ context.Context, item KeychainItem) ([]byte, error) {
		*got = append(*got, item)
		return out, err
	}
}

func TestResolveEnvWins(t *testing.T) {
	var asked []KeychainItem
	s := TokenSource{
		Getenv:   envMap(map[string]string{EnvToken: testToken}),
		Keychain: recordingKeychain([]byte("from-keychain"), nil, &asked),
	}
	tok, err := s.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok != testToken {
		t.Errorf("token = %q, want the env token", tok)
	}
	if len(asked) != 0 {
		t.Errorf("keychain consulted although %s is set: %v", EnvToken, asked)
	}
}

func TestResolveEnvTrimmed(t *testing.T) {
	s := TokenSource{
		Getenv:   envMap(map[string]string{EnvToken: "  " + testToken + "\n"}),
		Keychain: func(context.Context, KeychainItem) ([]byte, error) { t.Fatal("keychain consulted"); return nil, nil },
	}
	tok, err := s.Resolve(context.Background())
	if err != nil || tok != testToken {
		t.Fatalf("Resolve = %q, %v; want %q", tok, err, testToken)
	}
}

func TestResolveBlankEnvFallsBack(t *testing.T) {
	var asked []KeychainItem
	s := TokenSource{
		Getenv:   envMap(map[string]string{EnvToken: " \n"}),
		Keychain: recordingKeychain([]byte(testToken+"\n"), nil, &asked),
	}
	tok, err := s.Resolve(context.Background())
	if err != nil || tok != testToken {
		t.Fatalf("Resolve = %q, %v; want keychain token", tok, err)
	}
	if len(asked) != 1 {
		t.Fatalf("keychain asked %d times, want 1", len(asked))
	}
}

func TestResolveKeychainFallbackDefaults(t *testing.T) {
	var asked []KeychainItem
	s := TokenSource{
		Getenv:   envMap(nil),
		Keychain: recordingKeychain([]byte(testToken+"\n"), nil, &asked),
	}
	tok, err := s.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok != testToken {
		t.Errorf("token = %q, want trailing newline trimmed", tok)
	}
	want := KeychainItem{Service: "workingman:service_account", Account: "jim.clark"}
	if len(asked) != 1 || asked[0] != want {
		t.Errorf("keychain asked for %v, want [%v]", asked, want)
	}
}

func TestResolveKeychainOverrides(t *testing.T) {
	var asked []KeychainItem
	s := TokenSource{
		Getenv: envMap(map[string]string{
			EnvKeychainService: "svc",
			EnvKeychainAccount: "acct",
			EnvKeychain:        "jim.clark.keychain-db",
		}),
		Keychain: recordingKeychain([]byte(testToken), nil, &asked),
	}
	if _, err := s.Resolve(context.Background()); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := KeychainItem{Service: "svc", Account: "acct", Keychain: "jim.clark.keychain-db"}
	if len(asked) != 1 || asked[0] != want {
		t.Errorf("keychain asked for %v, want [%v]", asked, want)
	}
}

func TestResolveMissingToken(t *testing.T) {
	lookupErr := errors.New("The specified item could not be found in the keychain.")
	var asked []KeychainItem
	s := TokenSource{Getenv: envMap(nil), Keychain: recordingKeychain(nil, lookupErr, &asked)}
	_, err := s.Resolve(context.Background())
	if err == nil {
		t.Fatal("Resolve succeeded with no token anywhere")
	}
	if !errors.Is(err, lookupErr) {
		t.Errorf("error does not wrap the keychain error: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{EnvToken, "workingman:service_account", "jim.clark", "security add-generic-password -U -a jim.clark -s workingman:service_account -w"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

func TestResolveEmptyKeychainItem(t *testing.T) {
	var asked []KeychainItem
	s := TokenSource{Getenv: envMap(nil), Keychain: recordingKeychain([]byte("\n"), nil, &asked)}
	_, err := s.Resolve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("Resolve error = %v, want an empty-item error", err)
	}
}

func TestResolveUnsupportedKeychain(t *testing.T) {
	s := TokenSource{
		Getenv:   envMap(nil),
		Keychain: func(context.Context, KeychainItem) ([]byte, error) { return nil, ErrKeychainUnsupported },
	}
	_, err := s.Resolve(context.Background())
	if !errors.Is(err, ErrKeychainUnsupported) || !strings.Contains(err.Error(), EnvToken) {
		t.Fatalf("Resolve error = %v, want ErrKeychainUnsupported and a hint to set %s", err, EnvToken)
	}
}

// TestTokenNeverInErrors drives every error path with the token available to
// the code and checks it never surfaces.
func TestTokenNeverInErrors(t *testing.T) {
	// A keychain that fails after "reading" the token (e.g. a lookup that
	// errors with partial output) must not let it into the error.
	s := TokenSource{
		Getenv:   envMap(nil),
		Keychain: func(context.Context, KeychainItem) ([]byte, error) { return []byte(testToken), errors.New("boom") },
	}
	if _, err := s.Resolve(context.Background()); err == nil || strings.Contains(err.Error(), testToken) {
		t.Errorf("Resolve error = %v; want an error without the token", err)
	}

	// An SDK error that echoes the token is redacted.
	restore := newClient
	t.Cleanup(func() { newClient = restore })
	sdkErr := errors.New("invalid service account token " + testToken)
	newClient = func(context.Context, ...sdk.ClientOption) (*sdk.Client, error) { return nil, sdkErr }
	_, err := NewWithToken(context.Background(), testToken)
	if err == nil {
		t.Fatal("NewWithToken succeeded")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("NewWithToken error leaks the token: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") || !strings.Contains(err.Error(), "service account") {
		t.Errorf("NewWithToken error = %v; want a redacted, actionable message", err)
	}
	if !errors.Is(err, sdkErr) {
		t.Errorf("NewWithToken error does not wrap the SDK error")
	}
}

func TestNewPassesTokenAndIntegration(t *testing.T) {
	t.Setenv(EnvToken, testToken+"\n")
	restore := newClient
	t.Cleanup(func() { newClient = restore })
	var nopts int
	newClient = func(_ context.Context, opts ...sdk.ClientOption) (*sdk.Client, error) {
		nopts = len(opts)
		return &sdk.Client{}, nil
	}
	c, err := New(context.Background())
	if err != nil || c == nil {
		t.Fatalf("New = %v, %v", c, err)
	}
	if nopts != 2 {
		t.Errorf("NewClient got %d options, want token + integration info", nopts)
	}
}

func TestNewMissingToken(t *testing.T) {
	t.Setenv(EnvToken, "")
	if runtime.GOOS == "darwin" {
		t.Skip("would read the real keychain")
	}
	restore := newClient
	t.Cleanup(func() { newClient = restore })
	newClient = func(context.Context, ...sdk.ClientOption) (*sdk.Client, error) {
		t.Fatal("NewClient called without a token")
		return nil, nil
	}
	_, err := New(context.Background())
	if !errors.Is(err, ErrKeychainUnsupported) {
		t.Fatalf("New error = %v, want ErrKeychainUnsupported", err)
	}
}

func TestVersion(t *testing.T) {
	if Version() == "" {
		t.Error("Version is empty")
	}
}
