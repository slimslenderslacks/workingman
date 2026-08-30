package gitsign

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestConfigEnabled(t *testing.T) {
	cases := []struct {
		name string
		c    Config
		want bool
	}{
		{"fully configured", Config{SigningKey: "ssh-ed25519 AAA", Format: "ssh", GpgSign: "true"}, true},
		{"format case-insensitive", Config{SigningKey: "k", Format: "SSH", GpgSign: "TRUE"}, true},
		{"no key", Config{Format: "ssh", GpgSign: "true"}, false},
		{"gpg not ssh", Config{SigningKey: "k", Format: "openpgp", GpgSign: "true"}, false},
		{"gpgsign off", Config{SigningKey: "k", Format: "ssh", GpgSign: "false"}, false},
		{"gpgsign unset", Config{SigningKey: "k", Format: "ssh"}, false},
		{"empty", Config{}, false},
	}
	for _, tc := range cases {
		if got := tc.c.Enabled(); got != tc.want {
			t.Errorf("%s: Enabled() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestConfigKey(t *testing.T) {
	// Key() returns the signing key only when fully enabled, so a half-configured
	// host never injects a lone signingkey that would leave commits unsigned or
	// make git commit fail.
	enabled := Config{SigningKey: "ssh-ed25519 AAA", Format: "ssh", GpgSign: "true"}
	if got := enabled.Key(); got != "ssh-ed25519 AAA" {
		t.Errorf("enabled Key() = %q, want the signing key", got)
	}
	half := Config{SigningKey: "ssh-ed25519 AAA", Format: "openpgp", GpgSign: "true"}
	if got := half.Key(); got != "" {
		t.Errorf("half-configured Key() = %q, want empty", got)
	}
}

func TestIdentityEnvArgs(t *testing.T) {
	want := []string{
		"-e", "GIT_AUTHOR_NAME=Jim Clark",
		"-e", "GIT_AUTHOR_EMAIL=jim@example.com",
		"-e", "GIT_COMMITTER_NAME=Jim Clark",
		"-e", "GIT_COMMITTER_EMAIL=jim@example.com",
	}
	if got := IdentityEnvArgs("Jim Clark", "jim@example.com"); !reflect.DeepEqual(got, want) {
		t.Errorf("IdentityEnvArgs() = %v, want %v", got, want)
	}
	// A half-set identity is worse than the sandbox default: either field empty
	// yields no env at all.
	if got := IdentityEnvArgs("Jim Clark", ""); got != nil {
		t.Errorf("IdentityEnvArgs with no email = %v, want nil", got)
	}
	if got := IdentityEnvArgs("", "jim@example.com"); got != nil {
		t.Errorf("IdentityEnvArgs with no name = %v, want nil", got)
	}
}

func TestSigningEnvArgs(t *testing.T) {
	want := []string{
		"-e", "GIT_CONFIG_COUNT=4",
		"-e", "GIT_CONFIG_KEY_0=user.signingkey",
		"-e", "GIT_CONFIG_VALUE_0=ssh-ed25519 AAAAKEY",
		"-e", "GIT_CONFIG_KEY_1=gpg.format",
		"-e", "GIT_CONFIG_VALUE_1=ssh",
		"-e", "GIT_CONFIG_KEY_2=gpg.ssh.program",
		"-e", "GIT_CONFIG_VALUE_2=ssh-keygen",
		"-e", "GIT_CONFIG_KEY_3=commit.gpgsign",
		"-e", "GIT_CONFIG_VALUE_3=true",
	}
	if got := SigningEnvArgs("ssh-ed25519 AAAAKEY"); !reflect.DeepEqual(got, want) {
		t.Errorf("SigningEnvArgs() = %v, want %v", got, want)
	}
	// No key → no signing env at all (commits land unsigned, never half-configured).
	if got := SigningEnvArgs(""); got != nil {
		t.Errorf("SigningEnvArgs(\"\") = %v, want nil", got)
	}
}

func TestPreflight(t *testing.T) {
	// Not configured → not checked, so no `sbx exec` runs.
	ran := false
	run := func(context.Context, string, ...string) ([]byte, error) { ran = true; return nil, nil }
	if checked, ok := Preflight(context.Background(), run, "sbx", "s", ""); checked || ok {
		t.Errorf("no signing key: checked=%v ok=%v, want false false", checked, ok)
	}
	if ran {
		t.Error("Preflight ran a command despite no signing key configured")
	}

	// Agent holds a key → checked and ok.
	okRun := func(_ context.Context, name string, args ...string) ([]byte, error) {
		return []byte("256 SHA256:abc123 key (ED25519)\n"), nil
	}
	if checked, ok := Preflight(context.Background(), okRun, "sbx", "s", "k"); !checked || !ok {
		t.Errorf("agent with key: checked=%v ok=%v, want true true", checked, ok)
	}

	// `ssh-add -l` exits non-zero when the agent has no identities → checked but not ok.
	emptyRun := func(context.Context, string, ...string) ([]byte, error) {
		return []byte("The agent has no identities."), errors.New("exit status 1")
	}
	if checked, ok := Preflight(context.Background(), emptyRun, "sbx", "s", "k"); !checked || ok {
		t.Errorf("empty agent: checked=%v ok=%v, want true false", checked, ok)
	}
}
