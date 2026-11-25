package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/channels"
)

const me = "U0CC1K1MFNX"

func TestSaveCreatesThenMergesAndKeepsTheRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "channels.yaml")
	s, err := NewStore(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if cur, err := s.Load(); err != nil || cur.Configured {
		t.Fatalf("Load before = %+v, %v", cur, err)
	}
	res, err := s.Save(Update{Channel: me, AllowFrom: []string{me}, RouteTopics: DefaultRouteTopics, RouteChat: me})
	if err != nil {
		t.Fatal(err)
	}
	if !res.CreatedConfig || len(res.RoutesAdded) != 1 || res.RoutesAdded[0] != (channels.Route{Topic: "post-office", Channel: "slack", Chat: me}) {
		t.Errorf("res = %+v", res)
	}
	cur, err := s.Load()
	if err != nil || !cur.Configured || cur.OptionErr != nil || cur.Config.Channel != me || cur.GatewayURL != "http://localhost:3333" || len(cur.Routes) != 1 {
		t.Fatalf("Load = %+v, %v", cur, err)
	}

	// Comments, other channels and routes, and unmanaged options survive; a
	// second save with the same route adds nothing.
	data, _ := os.ReadFile(path)
	text := "# top comment\n" + strings.Replace(string(data), "allow_from:", "watch_ttl: 48h # mine\n      allow_from:", 1) +
		"  - {topic: wolf, channel: slack, chat: " + me + "}\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = s.Save(Update{PollInterval: "30s", RouteTopics: DefaultRouteTopics, RouteChat: me})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RoutesAdded) != 0 || res.CreatedConfig {
		t.Errorf("second res = %+v", res)
	}
	out, _ := os.ReadFile(path)
	for _, want := range []string{"# top comment", "watch_ttl: 48h # mine", "poll_interval: 30s", "allow_from: [" + me + "]", "topic: wolf"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("config lost %q:\n%s", want, out)
		}
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want the file's own 0600", fi.Mode().Perm())
	}
	if cur, _ := s.Load(); cur.Config.WatchTTL.Hours() != 48 || cur.Config.PollInterval.Seconds() != 30 {
		t.Errorf("config = %+v", cur.Config)
	}
}

func TestSaveRefusesAnInvalidSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "channels.yaml")
	orig := "channels:\n  slack:\n    type: slack\n    options:\n      typo: 1\n"
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	s, _ := NewStore(path, "slack")
	cur, err := s.Load()
	if err != nil || !cur.Configured || cur.OptionErr == nil {
		t.Fatalf("Load = %+v, %v", cur, err)
	}
	if _, err := s.Save(Update{Channel: me}); err == nil || !strings.Contains(err.Error(), "invalid slack section") {
		t.Fatalf("Save err = %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != orig {
		t.Errorf("file changed:\n%s", got)
	}
}

func TestNamedInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "channels.yaml")
	s, _ := NewStore(path, "work-slack")
	if _, err := s.Save(Update{Channel: "C0123ABCD", RouteTopics: []string{"post-office", "wolf"}, RouteChat: "C0123ABCD"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := channels.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Channels["work-slack"].Type != "slack" || len(cfg.Routes) != 2 || cfg.Routes[1].Channel != "work-slack" {
		t.Errorf("cfg = %+v", cfg)
	}
}
