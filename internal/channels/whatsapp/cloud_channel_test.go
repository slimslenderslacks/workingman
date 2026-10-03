package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/channels"
)

func newTestChannel(t *testing.T, g *fakeGraph) *Channel {
	t.Helper()
	return NewChannel("whatsapp", newTestClient(t, g), nil)
}

func TestChannel_SatisfiesInterfaceAndSends(t *testing.T) {
	g := newFakeGraph(t)
	var ch channels.Channel = newTestChannel(t, g)
	if ch.Name() != "whatsapp" {
		t.Errorf("Name = %q", ch.Name())
	}
	if err := ch.Start(context.Background(), func(context.Context, channels.InboundMessage) {}); err != nil {
		t.Fatal(err)
	}
	id, err := ch.Send(context.Background(), channels.OutboundMessage{ChatID: "15551234567", Text: "wolf is up", Topic: "wolf", ReplyToID: "wamid.Q"})
	if err != nil {
		t.Fatal(err)
	}
	if id != "wamid.1" {
		t.Errorf("id = %q", id)
	}
	body := g.requests()[0].Body
	if got := body["text"].(map[string]any)["body"]; got != "[wolf] wolf is up" {
		t.Errorf("body = %q; the topic should prefix the text", got)
	}
	if body["context"].(map[string]any)["message_id"] != "wamid.Q" {
		t.Errorf("context = %v", body["context"])
	}
}

func TestChannel_SendReturnsLastChunkID(t *testing.T) {
	g := newFakeGraph(t)
	ch := newTestChannel(t, g)
	id, err := ch.Send(context.Background(), channels.OutboundMessage{ChatID: "15551234567", Text: strings.Repeat("word ", 3000)})
	if err != nil {
		t.Fatal(err)
	}
	if want := "wamid." + string(rune('0'+len(g.requests()))); id != want {
		t.Errorf("id = %q, want the last chunk's %q", id, want)
	}
}

func TestChannel_SendSurfacesWindowError(t *testing.T) {
	g := newFakeGraph(t, graphReply{Status: 400, Body: graphErrBody(131047, "Re-engagement message")})
	_, err := newTestChannel(t, g).Send(context.Background(), channels.OutboundMessage{ChatID: "15551234567", Text: "hi"})
	if !IsOutsideServiceWindow(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestChannel_EmptyTextIsAnError(t *testing.T) {
	g := newFakeGraph(t)
	_, err := newTestChannel(t, g).Send(context.Background(), channels.OutboundMessage{ChatID: "15551234567", Text: "  "})
	if !errors.Is(err, ErrEmptyMessage) {
		t.Fatalf("err = %v", err)
	}
}

func TestChannel_ClosedRefusesAndCloseIsIdempotent(t *testing.T) {
	g := newFakeGraph(t)
	ch := newTestChannel(t, g)
	if err := ch.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ch.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ch.Send(context.Background(), channels.OutboundMessage{ChatID: "15551234567", Text: "hi"}); !errors.Is(err, channels.ErrNotRunning) {
		t.Errorf("Send after Close: %v", err)
	}
	if err := ch.Start(context.Background(), nil); !errors.Is(err, channels.ErrNotRunning) {
		t.Errorf("Start after Close: %v", err)
	}
	if n := len(g.requests()); n != 0 {
		t.Errorf("%d requests after Close", n)
	}
}

func TestChannel_TypingUsesLatestInboundWamid(t *testing.T) {
	g := newFakeGraph(t, graphReply{Body: `{"success":true}`})
	ch := newTestChannel(t, g)
	ctx := context.Background()

	// Nothing received yet: no request, no error.
	if err := ch.Typing(ctx, "15551234567"); err != nil {
		t.Fatal(err)
	}
	if n := len(g.requests()); n != 0 {
		t.Fatalf("%d requests with no inbound message to attach to", n)
	}

	ch.NoteInbound("15551234567", "wamid.OLD")
	ch.NoteInbound("+1 555 123 4567", "wamid.NEW") // same chat, other spelling
	ch.NoteInbound("15559999999", "wamid.OTHER")
	if err := ch.Typing(ctx, "15551234567@s.whatsapp.net"); err != nil {
		t.Fatal(err)
	}
	reqs := g.requests()
	if len(reqs) != 1 || reqs[0].Body["message_id"] != "wamid.NEW" {
		t.Fatalf("requests = %+v", reqs)
	}
	if _, ok := reqs[0].Body["typing_indicator"]; !ok {
		t.Error("no typing_indicator in the payload")
	}
}

func TestChannel_InboundCacheIsBounded(t *testing.T) {
	ch := newTestChannel(t, newFakeGraph(t))
	for i := range lastInboundCap + 50 {
		ch.NoteInbound("1555"+pad7(i), "wamid.x")
	}
	if n := len(ch.inbound); n != lastInboundCap || ch.order.Len() != lastInboundCap {
		t.Errorf("cache holds %d/%d entries, want %d", n, ch.order.Len(), lastInboundCap)
	}
	if ch.lastInbound("1555"+pad7(0)) != "" {
		t.Error("the oldest entry should have been evicted")
	}
	if ch.lastInbound("1555"+pad7(lastInboundCap+49)) == "" {
		t.Error("the newest entry was evicted")
	}
}

func pad7(i int) string { return fmt.Sprintf("%07d", i) }

func TestChannel_MarkRead(t *testing.T) {
	g := newFakeGraph(t, graphReply{Body: `{"success":true}`})
	ch := newTestChannel(t, g)
	if err := ch.MarkRead(context.Background(), "wamid.IN"); err != nil {
		t.Fatal(err)
	}
	if b := g.requests()[0].Body; b["status"] != "read" || b["message_id"] != "wamid.IN" {
		t.Errorf("body = %v", b)
	}
}

func TestCloudFactory_BuildsFromSpec(t *testing.T) {
	g := newFakeGraph(t)
	spec := channels.ChannelConfig{
		Type: "whatsapp",
		Options: map[string]any{
			"phone_number_id": 1098765, // an unquoted YAML number
			"waba_id":         "555",
			"api_version":     "v21.0",
			"graph_base_url":  g.srv.URL,
			"mode":            "cloud",
			"access":          map[string]any{"allow_from": []any{"15551234567"}},
		},
	}
	creds := channels.Credentials{
		CredAccessToken: channels.NewSecret(testToken),
		CredAppSecret:   channels.NewSecret("app-secret"),
		CredVerifyToken: channels.NewSecret("verify"),
	}
	ch, err := CloudFactory(nil)("whatsapp", spec, creds)
	if err != nil {
		t.Fatal(err)
	}
	cc, ok := ch.(*Channel)
	if !ok {
		t.Fatalf("factory returned %T", ch)
	}
	cfg := cc.Client().Config()
	if cfg.PhoneNumberID != "1098765" || cfg.WABAID != "555" || cfg.APIVersion != "v21.0" ||
		cfg.AppSecret.Reveal() != "app-secret" || cfg.VerifyToken.Reveal() != "verify" {
		t.Errorf("config = %+v", cfg)
	}
	if _, err := ch.Send(context.Background(), channels.OutboundMessage{ChatID: "15551234567", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if p := g.requests()[0].Path; p != "/v21.0/1098765/messages" {
		t.Errorf("path = %q", p)
	}
}

func TestCloudFactory_RejectsBadConfig(t *testing.T) {
	good := channels.Credentials{CredAccessToken: channels.NewSecret(testToken)}
	cases := []struct {
		name  string
		spec  channels.ChannelConfig
		creds channels.Credentials
		want  string
	}{
		{"no phone id", channels.ChannelConfig{Type: "whatsapp"}, good, "phone_number_id"},
		{"no token", channels.ChannelConfig{Type: "whatsapp", Options: map[string]any{"phone_number_id": "1"}}, nil, "access_token"},
		{"wrong option type", channels.ChannelConfig{Type: "whatsapp", Options: map[string]any{"phone_number_id": []any{"1"}}}, good, "options.phone_number_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CloudFactory(nil)("whatsapp", tc.spec, tc.creds)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			assertNoSecret(t, "factory error", err.Error())
		})
	}
}

func TestFactory_DefaultModeBuildsCloudChannel(t *testing.T) {
	f := Factory(FactoryDeps{Cloud: CloudFactory(nil)})
	spec := channels.ChannelConfig{Type: "whatsapp", Options: map[string]any{"phone_number_id": "1"}}
	ch, err := f("whatsapp", spec, channels.Credentials{CredAccessToken: channels.NewSecret(testToken)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ch.(*Channel); !ok {
		t.Errorf("got %T", ch)
	}
}
