package channels

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileConversationIndexRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "conv.json")
	x, err := NewFileConversationIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	want := ConversationTarget{Kind: ConversationKindWolf, WorkStream: "ws", ProjectPath: "/r/ws/.project.yaml", SessionKey: "/r/ws/.project.yaml#wolf", SessionID: "wolf-1"}
	if err := x.Register("whatsapp", "wamid.1", want); err != nil {
		t.Fatal(err)
	}
	if got, ok := x.Lookup("whatsapp", "wamid.1"); !ok || got != want {
		t.Fatalf("Lookup = %+v, %v", got, ok)
	}
	if _, ok := x.Lookup("other", "wamid.1"); ok {
		t.Error("lookup must be scoped by channel")
	}
	if _, ok := x.Lookup("whatsapp", "nope"); ok {
		t.Error("unknown id found")
	}

	reloaded, err := NewFileConversationIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reloaded.Lookup("whatsapp", "wamid.1"); !ok || got != want {
		t.Fatalf("after reload Lookup = %+v, %v", got, ok)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("index file mode = %v (%v), want 0600", info.Mode(), err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp")); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

func TestFileConversationIndexPrunesByTTLAndSize(t *testing.T) {
	x, _ := NewFileConversationIndex("")
	now := time.Now()
	x.now = func() time.Time { return now }
	x.maxEntries = 3
	x.ttl = time.Hour

	_ = x.Register("c", "old", ConversationTarget{SessionKey: "old"})
	now = now.Add(2 * time.Hour)
	for i := 0; i < 5; i++ {
		now = now.Add(time.Second)
		_ = x.Register("c", fmt.Sprintf("m%d", i), ConversationTarget{SessionKey: "k"})
	}
	if _, ok := x.Lookup("c", "old"); ok {
		t.Error("expired entry survived")
	}
	if x.Len() != 3 {
		t.Fatalf("Len = %d, want 3", x.Len())
	}
	if _, ok := x.Lookup("c", "m0"); ok {
		t.Error("oldest entry should have been evicted")
	}
	if _, ok := x.Lookup("c", "m4"); !ok {
		t.Error("newest entry evicted")
	}
}

func TestFileConversationIndexRejectsBadInputAndCorruptFile(t *testing.T) {
	x, _ := NewFileConversationIndex("")
	if err := x.Register("", "id", ConversationTarget{}); err == nil {
		t.Error("empty channel accepted")
	}
	path := filepath.Join(t.TempDir(), "conv.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileConversationIndex(path); err == nil {
		t.Error("corrupt file should be an error")
	}
}

func TestNotifyConfigWolfStartInterval(t *testing.T) {
	cases := []struct {
		yaml string
		want time.Duration
		bad  bool
	}{
		{"", DefaultWolfStartInterval, false},
		{"notify: {wolf_start_interval: 30m}", 30 * time.Minute, false},
		{"notify: {wolf_start_interval: 0s}", 0, false},
		{"notify: {wolf_start_interval: soon}", 0, true},
		{"notify: {wolf_start_interval: -5m}", 0, true},
	}
	for _, c := range cases {
		cfg, err := Parse([]byte(c.yaml))
		if c.bad {
			if err == nil {
				t.Errorf("%q: want error", c.yaml)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.yaml, err)
			continue
		}
		if got := cfg.Notify.WolfStartIntervalDuration(); got != c.want {
			t.Errorf("%q: interval = %v, want %v", c.yaml, got, c.want)
		}
	}
}
