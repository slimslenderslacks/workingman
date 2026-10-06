package signal

import (
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/channels"
)

func TestParseOptions(t *testing.T) {
	tests := []struct {
		name    string
		opts    map[string]any
		creds   channels.Credentials
		wantErr []string // substrings, all required
		check   func(t *testing.T, c Config)
	}{
		{
			name: "defaults",
			opts: map[string]any{"account": "+1 (555) 000-1111"},
			check: func(t *testing.T, c Config) {
				if c.HTTPURL != DefaultHTTPURL || c.Account != testAccount || c.PlainText {
					t.Errorf("got %+v", c)
				}
				if len(c.Access.AllowFrom) != 0 || c.Access.AllowAll {
					t.Errorf("default access is not closed: %+v", c.Access)
				}
			},
		},
		{
			name: "full",
			opts: map[string]any{
				"http_url": "http://signal.internal:9000/", "account": testAccount, "plain_text": true,
				"access": map[string]any{
					"allow_from": []any{"+1 555 765 4321", "AAAAAAAA-0000-0000-0000-000000000001"},
					"groups":     true, "group_allow_from": []any{"group:R3JvdXA=", "Zm9v"}, "note_to_self": true,
				},
			},
			check: func(t *testing.T, c Config) {
				if c.HTTPURL != "http://signal.internal:9000" || !c.PlainText || !c.Access.Groups || !c.Access.NoteToSelf {
					t.Errorf("got %+v", c)
				}
				if got := strings.Join(c.Access.AllowFrom, ","); got != "+15557654321,aaaaaaaa-0000-0000-0000-000000000001" {
					t.Errorf("allow_from = %s", got)
				}
				if got := strings.Join(c.Access.GroupAllowFrom, ","); got != "R3JvdXA=,Zm9v" {
					t.Errorf("group_allow_from = %s", got)
				}
			},
		},
		{
			name:  "account from credentials",
			opts:  nil,
			creds: channels.Credentials{"account": channels.NewSecret("+15550001111")},
			check: func(t *testing.T, c Config) {
				if c.Account != testAccount {
					t.Errorf("account = %q", c.Account)
				}
			},
		},
		{
			name:    "account in both places",
			opts:    map[string]any{"account": testAccount},
			creds:   channels.Credentials{"account": channels.NewSecret("+15550001111")},
			wantErr: []string{"options.account", "use one"},
		},
		{name: "account required", opts: map[string]any{}, wantErr: []string{"options.account: required"}},
		{name: "account not a number", opts: map[string]any{"account": "bob"}, wantErr: []string{"options.account", "E.164"}},
		{
			name:    "bad credential value is not echoed",
			opts:    nil,
			creds:   channels.Credentials{"account": channels.NewSecret("hunter2-not-a-number")},
			wantErr: []string{"E.164"},
		},
		{name: "unknown credential", opts: map[string]any{"account": testAccount}, creds: channels.Credentials{"token": channels.NewSecret("x")}, wantErr: []string{"credentials.token"}},
		{name: "bad url scheme", opts: map[string]any{"account": testAccount, "http_url": "ftp://x"}, wantErr: []string{"options.http_url"}},
		{name: "bad url", opts: map[string]any{"account": testAccount, "http_url": "127.0.0.1:8080"}, wantErr: []string{"options.http_url"}},
		{name: "unknown option key", opts: map[string]any{"account": testAccount, "acount": "x"}, wantErr: []string{"acount"}},
		{name: "unknown access key", opts: map[string]any{"account": testAccount, "access": map[string]any{"allowfrom": []any{"+15557654321"}}}, wantErr: []string{"allowfrom"}},
		{
			name:    "wildcard allow_from",
			opts:    map[string]any{"account": testAccount, "access": map[string]any{"allow_from": []any{"*"}}},
			wantErr: []string{"allow_from[0]", "allow_all"},
		},
		{
			name:    "junk allow_from entry",
			opts:    map[string]any{"account": testAccount, "access": map[string]any{"allow_from": []any{"+15557654321", "pat"}}},
			wantErr: []string{"allow_from[1]", `"pat"`},
		},
		{
			name:    "groups without list",
			opts:    map[string]any{"account": testAccount, "access": map[string]any{"groups": true}},
			wantErr: []string{"groups: enabled but group_allow_from is empty"},
		},
		{
			name:    "group list without opt-in",
			opts:    map[string]any{"account": testAccount, "access": map[string]any{"group_allow_from": []any{"Zm9v"}}},
			wantErr: []string{"group_allow_from: given but groups is not enabled"},
		},
		{
			name:    "group wildcard",
			opts:    map[string]any{"account": testAccount, "access": map[string]any{"groups": true, "group_allow_from": []any{"*"}}},
			wantErr: []string{"group_allow_from[0]: wildcard"},
		},
		{
			name:    "all problems reported together",
			opts:    map[string]any{"http_url": "nope", "access": map[string]any{"allow_from": []any{"x"}}},
			wantErr: []string{"http_url", "account: required", "allow_from[0]"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := ParseOptions(tt.opts, tt.creds)
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("no error, want %v", tt.wantErr)
				}
				for _, w := range tt.wantErr {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error %q missing %q", err, w)
					}
				}
				if strings.Contains(err.Error(), "hunter2") {
					t.Errorf("error leaks credential value: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tt.check(t, c)
		})
	}
}

// TestLoadableFromChannelsYAML proves `type: signal` works end to end through
// the shared config loader and Registry builder.
func TestLoadableFromChannelsYAML(t *testing.T) {
	cfg, err := channels.Parse([]byte(`
channels:
  signal:
    type: signal
    credentials:
      account: {env: SIGNAL_TEST_ACCOUNT}
    options:
      http_url: http://127.0.0.1:8080
      access:
        allow_from: ["+15557654321"]
routes:
  - {topic: wolf, channel: signal, chat: "+15557654321"}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !cfg.Channels["signal"].InboundCapable() || !cfg.HasInboundChannel() {
		t.Error("signal should be inbound capable")
	}
	t.Setenv("SIGNAL_TEST_ACCOUNT", testAccount)
	reg, err := channels.Build(cfg, t.TempDir(), map[string]channels.Factory{Type: Factory(quietLogger())})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ch, ok := reg.Get("signal")
	if !ok || ch.Name() != "signal" {
		t.Fatalf("channel not registered: %v", reg.Names())
	}

	t.Setenv("SIGNAL_TEST_ACCOUNT", "garbage")
	if _, err := channels.Build(cfg, t.TempDir(), map[string]channels.Factory{Type: Factory(quietLogger())}); err == nil ||
		!strings.Contains(err.Error(), "channels.signal") || strings.Contains(err.Error(), "garbage") {
		t.Errorf("Build with bad account: err = %v", err)
	}
}
