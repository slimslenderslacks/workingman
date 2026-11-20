package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/channels/slack"
)

// TestSetupChannelsBuildsSlack checks the slack factory is registered and a
// slack-only config enables channels, with the post-office route installed.
// Nothing is started, so no gateway is contacted.
func TestSetupChannelsBuildsSlack(t *testing.T) {
	base := t.TempDir()
	cfgPath := filepath.Join(base, "channels.yaml")
	cfg := `
channels:
  slack:
    type: slack
    options:
      gateway_url: http://127.0.0.1:1
      channel: U0CC1K1MFNX
      allow_from: [U0CC1K1MFNX]
routes:
  - {topic: post-office, channel: slack, chat: U0CC1K1MFNX}
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	dc, err := setupChannels(cfgPath, base, filepath.Join(base, "state"), filepath.Join(base, "post-office"), audit.New(&lockedBuffer{}))
	if err != nil {
		t.Fatalf("setupChannels: %v", err)
	}
	t.Cleanup(func() { _ = dc.reg.Close() })
	if !dc.enabled() || !dc.cfg.HasInboundChannel() {
		t.Fatal("slack config did not enable an inbound channel")
	}
	ch, ok := dc.reg.Get("slack")
	if !ok {
		t.Fatal("no slack channel in the registry")
	}
	if _, ok := ch.(*slack.Channel); !ok {
		t.Fatalf("channel is %T, want *slack.Channel", ch)
	}
	if rs := dc.reg.RoutesFor("post-office"); len(rs) != 1 || rs[0].Channel != "slack" {
		t.Errorf("post-office routes = %v", rs)
	}
}

func TestSlackDepsPaths(t *testing.T) {
	d := slackDeps("/h/.workingman/state", nil, audit.New(&lockedBuffer{}))
	if d.StateDir != "/h/.workingman/slack" || d.GatewayStore.Dir != "/h/.workingman/gateways/daemon" {
		t.Errorf("deps = %+v", d)
	}
}
