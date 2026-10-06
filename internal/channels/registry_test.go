package channels_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/channels"
	"github.com/slimslenderslacks/work/internal/channels/channeltest"
	"github.com/slimslenderslacks/work/internal/notify"
)

func newReg(t *testing.T, chs ...channels.Channel) *channels.Registry {
	t.Helper()
	r := channels.NewRegistry()
	for _, ch := range chs {
		if err := r.Add(ch); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestAddValidation(t *testing.T) {
	r := newReg(t, channeltest.New("a"))
	if err := r.Add(channeltest.New("a")); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate add err = %v", err)
	}
	if err := r.Add(channeltest.New("")); err == nil {
		t.Error("empty name should error")
	}
	if err := r.Add(nil); err == nil {
		t.Error("nil channel should error")
	}
	if got := r.Names(); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("Names = %v", got)
	}
}

func TestLifecycle(t *testing.T) {
	var order []string
	a := channeltest.New("a").RecordCloseOrderIn(&order)
	b := channeltest.New("b").RecordCloseOrderIn(&order)
	r := newReg(t, a, b)

	if _, err := r.Send(context.Background(), "a", channels.OutboundMessage{}); !errors.Is(err, channels.ErrNotRunning) {
		t.Errorf("send before Start err = %v, want ErrNotRunning", err)
	}

	if err := r.Start(context.Background(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if a.StartCount() != 1 || b.StartCount() != 1 {
		t.Errorf("start counts = %d,%d", a.StartCount(), b.StartCount())
	}
	// Starting again does not restart running channels.
	if err := r.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if a.StartCount() != 1 {
		t.Errorf("restart: StartCount = %d", a.StartCount())
	}

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !reflect.DeepEqual(order, []string{"b", "a"}) {
		t.Errorf("close order = %v, want reverse registration", order)
	}
	if err := r.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if a.CloseCount() != 1 {
		t.Errorf("CloseCount = %d, want 1 (idempotent)", a.CloseCount())
	}
	if _, err := r.Send(context.Background(), "a", channels.OutboundMessage{}); !errors.Is(err, channels.ErrNotRunning) {
		t.Errorf("send after Close err = %v", err)
	}
	if err := r.Start(context.Background(), nil); err == nil {
		t.Error("Start after Close should error")
	}
	if err := r.Add(channeltest.New("c")); err == nil {
		t.Error("Add after Close should error")
	}
}

func TestStartFailureIsolation(t *testing.T) {
	bad := channeltest.New("bad")
	bad.StartErr = errors.New("no network")
	good := channeltest.New("good")
	r := newReg(t, bad, good)

	err := r.Start(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "start bad") || !strings.Contains(err.Error(), "no network") {
		t.Fatalf("Start err = %v", err)
	}
	if good.StartCount() != 1 {
		t.Error("healthy channel must still start")
	}
	if _, err := r.Send(context.Background(), "good", channels.OutboundMessage{Text: "hi"}); err != nil {
		t.Errorf("send via healthy channel: %v", err)
	}
	if _, err := r.Send(context.Background(), "bad", channels.OutboundMessage{}); !errors.Is(err, channels.ErrNotRunning) {
		t.Errorf("send via failed channel err = %v, want ErrNotRunning", err)
	}
	_ = r.Close()
	if good.CloseCount() != 1 {
		t.Errorf("good CloseCount = %d", good.CloseCount())
	}
}

func TestCloseErrorIsolation(t *testing.T) {
	a, b := channeltest.New("a"), channeltest.New("b")
	b.CloseErr = errors.New("close boom") // closed first (reverse order)
	r := newReg(t, a, b)
	if err := r.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	err := r.Close()
	if err == nil || !strings.Contains(err.Error(), "close b") {
		t.Errorf("Close err = %v", err)
	}
	if a.CloseCount() != 1 {
		t.Error("a must still be closed after b's Close failed")
	}
}

func TestContextCancelClosesChannels(t *testing.T) {
	a := channeltest.New("a")
	r := newReg(t, a)
	ctx, cancel := context.WithCancel(context.Background())
	if err := r.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if a.CloseCount() != 0 {
		t.Fatal("closed too early")
	}
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for a.CloseCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if a.CloseCount() != 1 {
		t.Errorf("CloseCount = %d after ctx cancel, want 1", a.CloseCount())
	}
}

func TestInboundDelivery(t *testing.T) {
	a := channeltest.New("a")
	r := newReg(t, a)
	var (
		mu  sync.Mutex
		got []channels.InboundMessage
	)
	h := func(_ context.Context, m channels.InboundMessage) {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
		if m.Text == "panic" {
			panic("handler bug")
		}
	}
	if err := r.Start(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := a.Inject(channels.InboundMessage{ChatID: "c1", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Inject(channels.InboundMessage{Channel: "explicit", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Inject(channels.InboundMessage{Text: "panic"}); err != nil { // must not propagate
		t.Fatal(err)
	}
	if err := a.Inject(channels.InboundMessage{Text: "after panic"}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0].Channel != "a" || got[1].Channel != "explicit" || got[3].Text != "after panic" {
		t.Errorf("got = %+v", got)
	}
}

func TestRoutesFor(t *testing.T) {
	r := channels.NewRegistry()
	r.SetRoutes([]channels.Route{
		{Topic: "wolf", Channel: "wa", Chat: "1"},
		{Topic: "wolf", Channel: "tg", Chat: "2"},
		{Topic: "*", Channel: "wa", Chat: "9"},
	})
	tests := []struct {
		topic string
		want  []channels.Route
	}{
		{"wolf", []channels.Route{{Topic: "wolf", Channel: "wa", Chat: "1"}, {Topic: "wolf", Channel: "tg", Chat: "2"}}},
		{"daemon", []channels.Route{{Topic: "*", Channel: "wa", Chat: "9"}}},
		{"", []channels.Route{{Topic: "*", Channel: "wa", Chat: "9"}}},
	}
	for _, tt := range tests {
		if got := r.RoutesFor(tt.topic); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("RoutesFor(%q) = %v, want %v", tt.topic, got, tt.want)
		}
	}
	r.SetRoutes(nil)
	if got := r.RoutesFor("wolf"); len(got) != 0 {
		t.Errorf("no routes: %v", got)
	}
}

func TestSendTopicFanOutErrorIsolation(t *testing.T) {
	ok1, ok2 := channeltest.New("ok1"), channeltest.New("ok2")
	failing := channeltest.New("failing")
	failing.SendErr = errors.New("rate limited")
	panicky := channeltest.New("panicky")
	panicky.PanicOnSend = true
	stopped := channeltest.New("stopped")
	stopped.StartErr = errors.New("never up")

	r := newReg(t, failing, panicky, ok1, stopped, ok2)
	r.SetRoutes([]channels.Route{
		{Topic: "wolf", Channel: "failing", Chat: "f"},
		{Topic: "wolf", Channel: "panicky", Chat: "p"},
		{Topic: "wolf", Channel: "ok1", Chat: "c1"},
		{Topic: "wolf", Channel: "stopped", Chat: "s"},
		{Topic: "wolf", Channel: "ok2", Chat: "c2"},
		{Topic: "other", Channel: "ok1", Chat: "zzz"},
	})
	_ = r.Start(context.Background(), nil)
	defer r.Close()

	results, err := r.SendTopic(context.Background(), "wolf", "wolf is up")
	if len(results) != 5 {
		t.Fatalf("results = %d, want 5 (one per wolf route)", len(results))
	}
	if err == nil || !strings.Contains(err.Error(), "rate limited") || !strings.Contains(err.Error(), "panic") ||
		!errors.Is(err, channels.ErrNotRunning) {
		t.Errorf("joined err = %v", err)
	}
	wantErr := map[string]bool{"failing": true, "panicky": true, "stopped": true}
	for _, res := range results {
		if wantErr[res.Route.Channel] != (res.Err != nil) {
			t.Errorf("route %s: err = %v", res.Route.Channel, res.Err)
		}
	}
	for _, c := range []*channeltest.Fake{ok1, ok2} {
		sent := c.Sent()
		if len(sent) != 1 || sent[0].Text != "wolf is up" || sent[0].Topic != "wolf" {
			t.Errorf("%s sent = %+v", c.Name(), sent)
		}
	}
	if got := ok1.Sent()[0].ChatID; got != "c1" {
		t.Errorf("ok1 chat = %q", got)
	}

	// Unrouted topic: no results, no error.
	if res, err := r.SendTopic(context.Background(), "nobody", "x"); err != nil || len(res) != 0 {
		t.Errorf("unrouted = %v, %v", res, err)
	}
}

func TestSendUnknownChannel(t *testing.T) {
	if _, err := channels.NewRegistry().Send(context.Background(), "ghost", channels.OutboundMessage{}); err == nil {
		t.Error("expected error")
	}
}

func TestFormatTopic(t *testing.T) {
	tests := []struct{ topic, text, want string }{
		{"wolf", "hello", "[wolf] hello"},
		{"", "hello", "hello"},
		{"  ", "hello", "hello"},
	}
	for _, tt := range tests {
		if got := channels.FormatTopic(tt.topic, tt.text); got != tt.want {
			t.Errorf("FormatTopic(%q,%q) = %q, want %q", tt.topic, tt.text, got, tt.want)
		}
	}
}

func TestBuildFromConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "channels.yaml")
	secrets := filepath.Join(dir, "secrets.yaml")
	if err := os.WriteFile(secrets, []byte("tok: file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(secrets, 0o600)
	yaml := `
channels:
  fake:
    type: fake
    credentials:
      token: {file: secrets.yaml, key: tok}
    options: {greeting: hi}
  off:
    type: unregistered-but-disabled
    enabled: false
routes:
  - {topic: wolf, channel: fake, chat: "42"}
  - {topic: wolf, channel: off, chat: "99"}
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	var fake *channeltest.Fake
	var gotCreds channels.Credentials
	var gotOpts map[string]any
	factories := map[string]channels.Factory{
		"fake": func(name string, spec channels.ChannelConfig, creds channels.Credentials) (channels.Channel, error) {
			fake = channeltest.New(name)
			gotCreds, gotOpts = creds, spec.Options
			return fake, nil
		},
	}
	reg, err := channels.BuildFromFile(cfgPath, factories)
	if err != nil {
		t.Fatalf("BuildFromFile: %v", err)
	}
	if gotCreds["token"].Reveal() != "file-token" || gotOpts["greeting"] != "hi" {
		t.Errorf("factory got creds=%v opts=%v", gotCreds, gotOpts)
	}
	if got := reg.Names(); !reflect.DeepEqual(got, []string{"fake"}) {
		t.Errorf("Names = %v (disabled channel must be skipped)", got)
	}
	if got := reg.RoutesFor("wolf"); len(got) != 1 || got[0].Chat != "42" {
		t.Errorf("routes = %v (disabled channel's route must be dropped)", got)
	}
	if err := reg.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	if _, err := reg.SendTopic(context.Background(), "wolf", "hi"); err != nil || len(fake.Sent()) != 1 {
		t.Errorf("send via built registry: %v, sent=%v", err, fake.Sent())
	}
}

func TestBuildErrors(t *testing.T) {
	t.Setenv("PRESENT_TOKEN", "x")
	parse := func(y string) *channels.Config {
		cfg, err := channels.Parse([]byte(y))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	boom := func(string, channels.ChannelConfig, channels.Credentials) (channels.Channel, error) {
		return nil, errors.New("factory exploded")
	}
	wrongName := func(string, channels.ChannelConfig, channels.Credentials) (channels.Channel, error) {
		return channeltest.New("other"), nil
	}
	ok := func(name string, _ channels.ChannelConfig, _ channels.Credentials) (channels.Channel, error) {
		return channeltest.New(name), nil
	}
	tests := []struct {
		name      string
		yaml      string
		factories map[string]channels.Factory
		want      []string
	}{
		{"unknown type", "channels:\n  a: {type: nope}\n", nil, []string{"channels.a.type", `"nope"`}},
		{"missing env", "channels:\n  a:\n    type: t\n    credentials:\n      k: {env: ABSENT_TOKEN_VAR}\n", map[string]channels.Factory{"t": ok}, []string{"ABSENT_TOKEN_VAR"}},
		{"factory error", "channels:\n  a: {type: t}\n", map[string]channels.Factory{"t": boom}, []string{"channels.a", "factory exploded"}},
		{"name mismatch", "channels:\n  a: {type: t}\n", map[string]channels.Factory{"t": wrongName}, []string{"channels.a", "name"}},
		{"collects all", "channels:\n  a: {type: nope}\n  b: {type: t}\n", map[string]channels.Factory{"t": boom}, []string{"channels.a", "channels.b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := channels.Build(parse(tt.yaml), t.TempDir(), tt.factories)
			if err == nil {
				t.Fatal("expected error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q missing %q", err, w)
				}
			}
		})
	}
}

func TestBuildFromFileMissingDefaultIsEmpty(t *testing.T) {
	t.Setenv(channels.ConfigPathEnv, filepath.Join(t.TempDir(), "absent.yaml"))
	reg, err := channels.BuildFromFile("", nil)
	if err != nil || len(reg.Names()) != 0 {
		t.Fatalf("BuildFromFile = %v, %v", reg, err)
	}
	if err := reg.Start(context.Background(), nil); err != nil {
		t.Errorf("empty Start: %v", err)
	}
	_ = reg.Close()
}

func TestNotifierAdapter(t *testing.T) {
	wa, other := channeltest.New("wa"), channeltest.New("other")
	failing := channeltest.New("failing")
	failing.SendErr = errors.New("down")
	r := newReg(t, wa, other, failing)
	r.SetRoutes([]channels.Route{
		{Topic: "wolf", Channel: "wa", Chat: "me"},
		{Topic: "wolf", Channel: "failing", Chat: "x"},
		{Topic: "*", Channel: "other", Chat: "catchall"},
	})
	if err := r.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	n := channels.NewNotifier(r).WithTimeout(time.Second)
	osa := &notify.Recorder{} // stands in for the macOS sender
	multi := notify.NewMulti(osa, n)

	// Topic-aware: reaches both wolf routes; the failing one is reported but
	// the mac sender and healthy channel still got it.
	err := notify.SendTopic(multi, "wolf", "Wolf started", "project p")
	if err == nil || !strings.Contains(err.Error(), "down") {
		t.Errorf("err = %v, want failing channel surfaced", err)
	}
	if got := osa.Calls(); len(got) != 1 || got[0].Topic != "wolf" {
		t.Errorf("mac calls = %+v", got)
	}
	sent := wa.Sent()
	if len(sent) != 1 || sent[0].Text != "Wolf started\nproject p" || sent[0].Topic != "wolf" || sent[0].ChatID != "me" {
		t.Errorf("wa sent = %+v", sent)
	}
	if len(other.Sent()) != 0 {
		t.Errorf("wildcard route must not fire when an exact route exists: %+v", other.Sent())
	}

	// Plain Send (existing callers, e.g. "Project blocked") has no topic and
	// reaches wildcard routes only.
	if err := multi.Send("Project blocked", "needs input"); err != nil {
		t.Errorf("plain send: %v", err)
	}
	if got := other.Sent(); len(got) != 1 || got[0].Text != "Project blocked\nneeds input" || got[0].Topic != "" {
		t.Errorf("other sent = %+v", got)
	}
	if len(wa.Sent()) != 1 {
		t.Errorf("wa should not receive topic-less send")
	}
}
