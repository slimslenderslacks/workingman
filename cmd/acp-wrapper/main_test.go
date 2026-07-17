package main

import "testing"

func TestGitSigningConfigEnabled(t *testing.T) {
	cases := []struct {
		name string
		c    gitSigningConfig
		want bool
	}{
		{"fully configured", gitSigningConfig{signingKey: "ssh-ed25519 AAA", format: "ssh", gpgSign: "true"}, true},
		{"format case-insensitive", gitSigningConfig{signingKey: "k", format: "SSH", gpgSign: "TRUE"}, true},
		{"no key", gitSigningConfig{format: "ssh", gpgSign: "true"}, false},
		{"gpg not ssh", gitSigningConfig{signingKey: "k", format: "openpgp", gpgSign: "true"}, false},
		{"gpgsign off", gitSigningConfig{signingKey: "k", format: "ssh", gpgSign: "false"}, false},
		{"gpgsign unset", gitSigningConfig{signingKey: "k", format: "ssh"}, false},
		{"empty", gitSigningConfig{}, false},
	}
	for _, tc := range cases {
		if got := tc.c.enabled(); got != tc.want {
			t.Errorf("%s: enabled() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestGitSigningConfigKey(t *testing.T) {
	// key() returns the signing key only when fully enabled, so a half-configured
	// host never injects a lone signingkey that would leave commits unsigned or
	// make git commit fail.
	enabled := gitSigningConfig{signingKey: "ssh-ed25519 AAA", format: "ssh", gpgSign: "true"}
	if got := enabled.key(); got != "ssh-ed25519 AAA" {
		t.Errorf("enabled key() = %q, want the signing key", got)
	}
	half := gitSigningConfig{signingKey: "ssh-ed25519 AAA", format: "openpgp", gpgSign: "true"}
	if got := half.key(); got != "" {
		t.Errorf("half-configured key() = %q, want empty", got)
	}
}
