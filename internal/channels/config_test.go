package channels

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validYAML = `
channels:
  whatsapp:
    type: whatsapp
    credentials:
      access_token: {env: WA_TOKEN}
      app_secret: {file: secrets.yaml, key: wa_secret}
    options:
      phone_number_id: "123"
  slack:
    type: slack
    enabled: false
routes:
  - {topic: wolf, channel: whatsapp, chat: "15551234567"}
  - {topic: "*", channel: whatsapp, chat: "15551234567"}
  - {topic: wolf, channel: slack, chat: C1}
`

func TestParseValid(t *testing.T) {
	cfg, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.ChannelNames(); len(got) != 2 || got[0] != "slack" || got[1] != "whatsapp" {
		t.Errorf("ChannelNames = %v", got)
	}
	if !cfg.Channels["whatsapp"].IsEnabled() || cfg.Channels["slack"].IsEnabled() {
		t.Errorf("enabled flags wrong: %+v", cfg.Channels)
	}
	if got := cfg.Channels["whatsapp"].Options["phone_number_id"]; got != "123" {
		t.Errorf("option = %v", got)
	}
	active := cfg.ActiveRoutes()
	if len(active) != 2 {
		t.Errorf("ActiveRoutes = %v, want the 2 whatsapp routes (slack disabled)", active)
	}
}

func TestParseEmptyIsValid(t *testing.T) {
	for _, in := range []string{"", "# nothing\n", "channels: {}\nroutes: []\n"} {
		cfg, err := Parse([]byte(in))
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)
		} else if len(cfg.Channels) != 0 || len(cfg.Routes) != 0 {
			t.Errorf("Parse(%q) = %+v, want empty", in, cfg)
		}
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want []string // substrings that must all appear
	}{
		{"missing type", "channels:\n  wa: {}\n", []string{"channels.wa.type: required"}},
		{"bad channel name", "channels:\n  Bad Name:\n    type: x\n", []string{"invalid channel name"}},
		{"inline token rejected", "channels:\n  wa:\n    type: whatsapp\n    credentials:\n      access_token: s3cr3t-inline\n", []string{"parse:"}},
		{"inline value key rejected", "channels:\n  wa:\n    type: whatsapp\n    credentials:\n      t: {value: s3cr3t-inline}\n", []string{"parse:", "value"}},
		{"empty credential ref", "channels:\n  wa:\n    type: x\n    credentials:\n      t: {}\n", []string{"channels.wa.credentials.t", "env or file+key"}},
		{"env and file", "channels:\n  wa:\n    type: x\n    credentials:\n      t: {env: A, file: f, key: k}\n", []string{"not both"}},
		{"file without key", "channels:\n  wa:\n    type: x\n    credentials:\n      t: {file: f}\n", []string{"file requires key"}},
		{"key with env", "channels:\n  wa:\n    type: x\n    credentials:\n      t: {env: A, key: k}\n", []string{"key is only valid with file"}},
		{"bad env name", "channels:\n  wa:\n    type: x\n    credentials:\n      t: {env: \"A B\"}\n", []string{"not a valid environment variable name"}},
		{"route missing fields", "channels:\n  wa: {type: x}\nroutes:\n  - {}\n", []string{"routes[0].topic: required", "routes[0].channel: required", "routes[0].chat: required"}},
		{"route unknown channel", "channels:\n  wa: {type: x}\nroutes:\n  - {topic: wolf, channel: nope, chat: c}\n", []string{`routes[0].channel: "nope" is not defined`}},
		{"route bad topic", "channels:\n  wa: {type: x}\nroutes:\n  - {topic: \"two words\", channel: wa, chat: c}\n", []string{"routes[0].topic"}},
		{"duplicate route", "channels:\n  wa: {type: x}\nroutes:\n  - {topic: wolf, channel: wa, chat: c}\n  - {topic: wolf, channel: wa, chat: c}\n", []string{"routes[1]: duplicate of routes[0]"}},
		{"unknown field", "channels:\n  wa: {type: x, bogus: 1}\n", []string{"parse:", "bogus"}},
		{"malformed yaml", "channels: [\n", []string{"parse:"}},
		{"all errors reported together", "channels:\n  a: {}\n  b: {}\n", []string{"channels.a.type", "channels.b.type"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatal("expected error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q missing %q", err, w)
				}
			}
			if strings.Contains(err.Error(), "s3cr3t-inline") {
				t.Errorf("error leaks inline secret: %v", err)
			}
		})
	}
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // defeat umask
		t.Fatal(err)
	}
}

func TestResolveCredentials(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "secrets.yaml"), "wa_secret: file-secret\nnum: 12345\n", 0o600)
	writeFile(t, filepath.Join(dir, "loose.yaml"), "k: v\n", 0o644)
	writeFile(t, filepath.Join(dir, "notamap.yaml"), "- a\n- b\n", 0o600)
	t.Setenv("WA_TOKEN", "env-token")
	t.Setenv("EMPTY_VAR", "")

	cfgFor := func(refs string) *Config {
		cfg, err := Parse([]byte("channels:\n  wa:\n    type: x\n    credentials:\n" + refs))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	t.Run("env and file", func(t *testing.T) {
		cfg := cfgFor("      tok: {env: WA_TOKEN}\n      sec: {file: secrets.yaml, key: wa_secret}\n      n: {file: secrets.yaml, key: num}\n")
		creds, err := cfg.ResolveCredentials("wa", dir)
		if err != nil {
			t.Fatal(err)
		}
		if creds["tok"].Reveal() != "env-token" || creds["sec"].Reveal() != "file-secret" || creds["n"].Reveal() != "12345" {
			t.Errorf("creds = %v / reveal mismatch", creds)
		}
	})

	t.Run("absolute file path", func(t *testing.T) {
		cfg := cfgFor(fmt.Sprintf("      sec: {file: %s, key: wa_secret}\n", filepath.Join(dir, "secrets.yaml")))
		if _, err := cfg.ResolveCredentials("wa", "/nonexistent"); err != nil {
			t.Fatal(err)
		}
	})

	errTests := []struct {
		name, refs, want string
	}{
		{"unset env", "      t: {env: DEFINITELY_UNSET_VAR_XYZ}\n", "DEFINITELY_UNSET_VAR_XYZ is not set"},
		{"empty env", "      t: {env: EMPTY_VAR}\n", "EMPTY_VAR is not set or empty"},
		{"loose perms", "      t: {file: loose.yaml, key: k}\n", "chmod 600"},
		{"missing key", "      t: {file: secrets.yaml, key: nope}\n", `key "nope" not found`},
		{"missing file", "      t: {file: absent.yaml, key: k}\n", "open secrets file"},
		{"not a map", "      t: {file: notamap.yaml, key: k}\n", "not a YAML map"},
	}
	for _, tt := range errTests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := cfgFor(tt.refs).ResolveCredentials("wa", dir)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), "channels.wa.credentials.t") {
				t.Errorf("error lacks config path: %v", err)
			}
		})
	}

	if _, err := cfgFor("      t: {env: WA_TOKEN}\n").ResolveCredentials("ghost", dir); err == nil {
		t.Error("unknown channel should error")
	}
}

func TestSecretNeverPrints(t *testing.T) {
	s := NewSecret("hunter2")
	creds := Credentials{"tok": s}
	for _, out := range []string{
		fmt.Sprint(s), fmt.Sprintf("%v", s), fmt.Sprintf("%+v", creds), fmt.Sprintf("%#v", creds),
		fmt.Sprintf("%s", s), fmt.Sprintf("%q", s), fmt.Sprintf("%v", &s),
	} {
		if strings.Contains(out, "hunter2") {
			t.Errorf("secret leaked: %q", out)
		}
	}
	if s.Reveal() != "hunter2" || s.Empty() {
		t.Error("Reveal/Empty broken")
	}
	if !NewSecret("").Empty() {
		t.Error("empty secret should be Empty")
	}
}

func TestLoadAndOptional(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "channels.yaml")
	writeFile(t, path, validYAML, 0o600)

	cfg, err := Load(path)
	if err != nil || len(cfg.Channels) != 2 {
		t.Fatalf("Load = %+v, %v", cfg, err)
	}

	missing := filepath.Join(dir, "missing.yaml")
	if _, err := Load(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load(missing) err = %v, want ErrNotExist", err)
	}
	if cfg, err := LoadOptional(missing); err != nil || len(cfg.Channels) != 0 {
		t.Errorf("LoadOptional(missing) = %+v, %v", cfg, err)
	}

	bad := filepath.Join(dir, "bad.yaml")
	writeFile(t, bad, "channels:\n  wa: {}\n", 0o600)
	_, err = LoadOptional(bad)
	if err == nil || !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), "channels.wa.type") {
		t.Errorf("LoadOptional(bad) err = %v, want path + validation detail", err)
	}
}

func TestDefaultConfigPath(t *testing.T) {
	t.Setenv(ConfigPathEnv, "")
	t.Setenv("HOME", "/home/test")
	got, err := DefaultConfigPath()
	if err != nil || got != "/home/test/.workingman/channels.yaml" {
		t.Errorf("default = %q, %v", got, err)
	}

	t.Setenv(ConfigPathEnv, "/custom/c.yaml")
	if got, _ := DefaultConfigPath(); got != "/custom/c.yaml" {
		t.Errorf("env override = %q", got)
	}
	t.Setenv(ConfigPathEnv, "~/x.yaml")
	if got, _ := DefaultConfigPath(); got != "/home/test/x.yaml" {
		t.Errorf("tilde override = %q", got)
	}

	// Load("") honors the env override.
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	writeFile(t, p, "channels:\n  a: {type: x}\n", 0o600)
	t.Setenv(ConfigPathEnv, p)
	if cfg, err := Load(""); err != nil || len(cfg.Channels) != 1 {
		t.Errorf("Load(\"\") = %+v, %v", cfg, err)
	}
}

func TestHasInboundChannel(t *testing.T) {
	parse := func(y string) *Config {
		t.Helper()
		cfg, err := Parse([]byte(y))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		return cfg
	}
	if (*Config)(nil).HasInboundChannel() {
		t.Error("nil config has no inbound channel")
	}
	if parse("channels: {}\n").HasInboundChannel() {
		t.Error("empty config has no inbound channel")
	}
	if !parse("channels:\n  wa:\n    type: whatsapp\n").HasInboundChannel() {
		t.Error("an enabled whatsapp channel can receive")
	}
	if parse("channels:\n  wa:\n    type: whatsapp\n    enabled: false\n").HasInboundChannel() {
		t.Error("a disabled channel is not inbound-capable")
	}
	if parse("channels:\n  x:\n    type: sendonly\n").HasInboundChannel() {
		t.Error("an unknown (send-only) type is not inbound-capable")
	}
}

func TestRouterConfig(t *testing.T) {
	cfg, err := Parse([]byte("router:\n  permission_timeout: 90s\n  turn_timeout: 10m\n  thinking_after: 5s\n  queue_size: 3\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	r := cfg.Router
	if r.PermissionTimeoutDuration().Seconds() != 90 || r.TurnTimeoutDuration().Minutes() != 10 || r.ThinkingAfterDuration().Seconds() != 5 || r.QueueSize != 3 {
		t.Errorf("router = %+v", r)
	}

	empty, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Router.PermissionTimeoutDuration() != 0 || empty.Router.TurnTimeoutDuration() != 0 || empty.Router.ThinkingAfterDuration() != 0 {
		t.Errorf("unset router durations must be 0 (use the router default): %+v", empty.Router)
	}

	for _, bad := range []string{
		"router: {permission_timeout: soon}",
		"router: {turn_timeout: -1m}",
		"router: {thinking_after: 0s}",
		"router: {queue_size: -2}",
		"router: {nope: 1}",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", bad)
		}
	}
}
