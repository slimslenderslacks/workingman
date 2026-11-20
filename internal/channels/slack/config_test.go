package slack

import (
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
)

func TestParseOptionsDefaults(t *testing.T) {
	cfg, err := ParseOptions(map[string]any{"channel": "U0CC1K1MFNX"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GatewayURL != DefaultGatewayURL || cfg.PollInterval != DefaultPollInterval || cfg.WatchTTL != DefaultWatchTTL || len(cfg.AllowFrom) != 0 {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.Allowed("U0CC1K1MFNX") {
		t.Error("empty allow_from admitted someone")
	}
}

func TestParseOptionsFull(t *testing.T) {
	cfg, err := ParseOptions(map[string]any{
		"gateway_url":   "http://localhost:3333/",
		"channel":       "C0123ABCD",
		"allow_from":    []any{"U0CC1K1MFNX"},
		"poll_interval": "30s",
		"watch_ttl":     "48h",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GatewayURL != "http://localhost:3333" || cfg.Channel != "C0123ABCD" || cfg.PollInterval != 30*time.Second || cfg.WatchTTL != 48*time.Hour {
		t.Errorf("cfg = %+v", cfg)
	}
	if !cfg.Allowed("U0CC1K1MFNX") || cfg.Allowed("U0OTHER") {
		t.Error("allow_from not applied")
	}
}

func TestParseOptionsRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		opts  map[string]any
		creds channels.Credentials
		want  []string
	}{
		"unknown key": {map[string]any{"channel": "U0CC1K1MFNX", "chanel": "x"}, nil, []string{"chanel"}},
		"no channel":  {map[string]any{}, nil, []string{"options.channel: required"}},
		"bad channel": {map[string]any{"channel": "#general"}, nil, []string{"not a Slack channel or user id"}},
		"bad url":     {map[string]any{"channel": "U0CC1K1MFNX", "gateway_url": "ftp://x"}, nil, []string{"gateway_url"}},
		"wildcard":    {map[string]any{"channel": "U0CC1K1MFNX", "allow_from": []any{"*"}}, nil, []string{"wildcard"}},
		"bad user":    {map[string]any{"channel": "U0CC1K1MFNX", "allow_from": []any{"C0123ABCD"}}, nil, []string{"not a Slack user id"}},
		"fast poll":   {map[string]any{"channel": "U0CC1K1MFNX", "poll_interval": "1s"}, nil, []string{"below the minimum"}},
		"bad poll":    {map[string]any{"channel": "U0CC1K1MFNX", "poll_interval": "soon"}, nil, []string{"not a duration"}},
		"bad ttl":     {map[string]any{"channel": "U0CC1K1MFNX", "watch_ttl": "-1h"}, nil, []string{"must be positive"}},
		"credentials": {map[string]any{"channel": "U0CC1K1MFNX"}, channels.Credentials{"token": channels.Secret{}}, []string{"takes no credentials"}},
		"all at once": {map[string]any{"gateway_url": "x", "poll_interval": "1s"}, nil, []string{"gateway_url", "channel: required", "poll_interval"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseOptions(tc.opts, tc.creds)
			if err == nil {
				t.Fatal("no error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

func TestFactoryBuildsAChannel(t *testing.T) {
	f := Factory(FactoryDeps{})
	ch, err := f("slack", channels.ChannelConfig{Type: Type, Options: map[string]any{"channel": "U0CC1K1MFNX"}}, nil)
	if err != nil || ch.Name() != "slack" {
		t.Fatalf("Factory = %v, %v", ch, err)
	}
	if _, err := f("slack", channels.ChannelConfig{Type: Type}, nil); err == nil {
		t.Fatal("Factory accepted a config with no channel")
	}
	// Close before Start must not touch the network.
	if err := ch.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInboundCapable(t *testing.T) {
	cfg, err := channels.Parse([]byte("channels:\n  slack:\n    type: slack\n    options: {channel: U0CC1K1MFNX}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasInboundChannel() {
		t.Error("a slack channel is not inbound-capable")
	}
}

func TestTSHelpers(t *testing.T) {
	if !tsLess("1795141398.745179", "1795141406.120249") || tsLess("2.000001", "2.000001") || !tsLess("2.5", "2.600000") {
		t.Error("tsLess")
	}
	if got := tsBefore("1795141398.745179"); got != "1795141398.745178" {
		t.Errorf("tsBefore = %s", got)
	}
	if got := tsBefore("10.000000"); got != "9.999999" {
		t.Errorf("tsBefore = %s", got)
	}
	if got := tsOf(time.Unix(1795141398, 745179000)); got != "1795141398.745179" {
		t.Errorf("tsOf = %s", got)
	}
	if ch, ts := SplitMessageID("D0CBT6QV8NT:1795141398.745179"); ch != "D0CBT6QV8NT" || ts != "1795141398.745179" {
		t.Errorf("SplitMessageID = %s, %s", ch, ts)
	}
	if ch, ts := SplitMessageID("1795141398.745179"); ch != "" || ts != "1795141398.745179" {
		t.Errorf("SplitMessageID(bare) = %s, %s", ch, ts)
	}
}
