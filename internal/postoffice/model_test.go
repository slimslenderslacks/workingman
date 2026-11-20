package postoffice

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 20, 10, 30, 0, 0, time.UTC)

func TestNewIDFormatAndOrder(t *testing.T) {
	var ids []string
	for i := range 1000 {
		// Same millisecond for most, to exercise the monotonic path.
		ids = append(ids, NewID(t0.Add(time.Duration(i/100)*time.Millisecond)))
	}
	seen := map[string]bool{}
	for i, id := range ids {
		if !ValidID(id) {
			t.Fatalf("invalid id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
		if i > 0 && id <= ids[i-1] {
			t.Fatalf("ids not increasing: %q then %q", ids[i-1], id)
		}
		if got, ok := IDTime(id); !ok || !got.Equal(t0.Add(time.Duration(i/100)*time.Millisecond)) {
			t.Fatalf("IDTime(%q) = %v, %v", id, got, ok)
		}
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatal("ids do not sort by creation")
	}
}

func TestValidID(t *testing.T) {
	for _, id := range []string{"", "msg_", "01JD3Z8Q4X7V2K9P0M5N6R1T8W", "msg_01JD3Z8Q4X7V2K9P0M5N6R1T8", "msg_01JD3Z8Q4X7V2K9P0M5N6R1T8WX",
		"msg_01JD3Z8Q4X7V2K9P0M5N6R1T8U", "msg_81JD3Z8Q4X7V2K9P0M5N6R1T8W", "msg_../../../../etc/passwdxx", "msg_01jd3z8q4x7v2k9p0m5n6r1t8w"} {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true", id)
		}
	}
	if !ValidID("msg_01JD3Z8Q4X7V2K9P0M5N6R1T8W") {
		t.Error("design-doc example id rejected")
	}
}

func TestShortID(t *testing.T) {
	if got := ShortID("msg_01JD3Z8Q4X7V2K9P0M5N6R1T8W"); got != "6r1t8w" {
		t.Fatalf("ShortID = %q", got)
	}
	a := Message{ID: "msg_01JD3Z8Q4X7V2K9P0M5N6R1T8W"}
	b := Message{ID: "msg_01JD3Z8Q4X7V2K9P0M5N6ABCDE"}
	c := Message{ID: "msg_01JD3Z8Q4X7V2K9P0M4N6R1T8W"} // same short id as a
	if m, ok, err := MatchShortID([]Message{a, b}, "#6R1T8W"); err != nil || !ok || m.ID != a.ID {
		t.Fatalf("MatchShortID = %v %v %v", m.ID, ok, err)
	}
	if _, ok, err := MatchShortID([]Message{a, b}, "zzzzzz"); ok || err != nil {
		t.Fatalf("no match: ok=%v err=%v", ok, err)
	}
	if _, ok, err := MatchShortID([]Message{a, b, c}, "6r1t8w"); ok || !errors.Is(err, ErrAmbiguousShortID) {
		t.Fatalf("collision: ok=%v err=%v", ok, err)
	}
}

func TestMatchOption(t *testing.T) {
	opts := []string{"staging", "Prod", "approve"}
	for answer, want := range map[string]string{
		"staging":            "staging",
		"  STAGING \n":       "staging",
		"prod":               "Prod",
		"1":                  "staging",
		" 3 ":                "approve",
		"0":                  "",
		"4":                  "",
		"-1":                 "",
		"use staging please": "",
		"":                   "",
	} {
		if got := MatchOption(opts, answer); got != want {
			t.Errorf("MatchOption(%q) = %q, want %q", answer, got, want)
		}
	}
	if got := MatchOption(nil, "1"); got != "" {
		t.Errorf("no options: %q", got)
	}
	// An option that is itself a number wins over the index.
	if got := MatchOption([]string{"2", "1"}, "1"); got != "1" {
		t.Errorf("numeric option: %q", got)
	}
}

func TestTruncateResponse(t *testing.T) {
	short := "hello"
	if TruncateResponse(short) != short {
		t.Fatal("short answer changed")
	}
	long := strings.Repeat("é", MaxResponse) // 2 bytes per rune
	got := TruncateResponse(long)
	if len(got) > MaxResponse {
		t.Fatalf("len %d > %d", len(got), MaxResponse)
	}
	if !strings.HasSuffix(got, truncatedMark) {
		t.Fatal("no truncation marker")
	}
	if !json.Valid([]byte(`"` + strings.TrimSuffix(got, truncatedMark) + `"`)) {
		t.Fatal("cut inside a rune")
	}
}

func TestNewMessageDefaults(t *testing.T) {
	from := Sender{Agent: "task", Sandbox: "wm-x-fix-ci", Project: "/o/workingman-messaging/project.yaml", Task: "fix-ci"}
	m, err := NewMessage(Draft{Kind: KindRequest, From: from, Recipient: " Operator ", Body: "\n\n  Which bucket?  \nmore", Options: []string{" staging ", "prod"}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if m.Recipient != RecipientOperator || m.Subject != "Which bucket?" || m.Version != Version {
		t.Fatalf("got %+v", m)
	}
	if !m.ExpiresAt.Equal(t0.Add(DefaultRequestExpiry)) {
		t.Fatalf("expires %v", m.ExpiresAt)
	}
	if m.Options[0] != "staging" {
		t.Fatalf("options not trimmed: %q", m.Options)
	}
	if got := from.Display(); got != "task fix-ci (workingman-messaging)" {
		t.Fatalf("Display = %q", got)
	}

	n, err := NewMessage(Draft{Kind: KindNotification, From: from, Recipient: "Step-Up", Subject: "FYI", Body: "x", ExpiresIn: time.Second}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if n.Recipient != "step-up" || !n.ExpiresAt.Equal(t0.Add(MinExpiry)) {
		t.Fatalf("got %+v", n)
	}
	n2, _ := NewMessage(Draft{Kind: KindNotification, From: from, Body: "x"}, t0)
	if !n2.ExpiresAt.Equal(t0.Add(DefaultNotificationExpiry)) {
		t.Fatalf("notification expiry %v", n2.ExpiresAt)
	}
	n3, _ := NewMessage(Draft{Kind: KindRequest, From: from, Body: "x", ExpiresIn: 30 * 24 * time.Hour}, t0)
	if !n3.ExpiresAt.Equal(t0.Add(MaxExpiry)) {
		t.Fatalf("expiry not clamped: %v", n3.ExpiresAt)
	}
	long, _ := NewMessage(Draft{Kind: KindRequest, From: from, Body: strings.Repeat("x", 500)}, t0)
	if r := []rune(long.Subject); len(r) != MaxSubject || r[len(r)-1] != '…' {
		t.Fatalf("subject not cut: %d runes", len(r))
	}
}

func TestNewMessageRejects(t *testing.T) {
	from := Sender{Agent: "task", Sandbox: "sb"}
	cases := map[string]Draft{
		"no body":             {Kind: KindRequest, From: from, Body: "  "},
		"no sandbox":          {Kind: KindRequest, Body: "x"},
		"bad kind":            {Kind: "memo", From: from, Body: "x"},
		"long body":           {Kind: KindRequest, From: from, Body: strings.Repeat("x", MaxBody+1)},
		"long subject":        {Kind: KindRequest, From: from, Body: "x", Subject: strings.Repeat("s", MaxSubject+1)},
		"too many options":    {Kind: KindRequest, From: from, Body: "x", Options: make([]string, MaxOptions+1)},
		"empty option":        {Kind: KindRequest, From: from, Body: "x", Options: []string{"a", " "}},
		"long option":         {Kind: KindRequest, From: from, Body: "x", Options: []string{strings.Repeat("o", MaxOption+1)}},
		"notification option": {Kind: KindNotification, From: from, Body: "x", Options: []string{"a"}},
	}
	for name, d := range cases {
		if _, err := NewMessage(d, t0); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestKindAllows(t *testing.T) {
	for _, c := range []struct {
		k  Kind
		o  Outcome
		ok bool
	}{
		{KindRequest, OutcomeAnswered, true},
		{KindRequest, OutcomeCancelled, true},
		{KindRequest, OutcomeExpired, true},
		{KindRequest, OutcomeDismissed, false},
		{KindNotification, OutcomeDismissed, true},
		{KindNotification, OutcomeExpired, true},
		{KindNotification, OutcomeAnswered, false},
		{KindNotification, OutcomeCancelled, false},
	} {
		if got := c.k.Allows(c.o); got != c.ok {
			t.Errorf("%s.Allows(%s) = %v", c.k, c.o, got)
		}
	}
}

func TestSenderSame(t *testing.T) {
	a := Sender{Sandbox: "sb", Task: "t1"}
	if !a.Same(Sender{Sandbox: "sb", Task: "t1", SessionID: "other"}) {
		t.Error("same sandbox and task")
	}
	if a.Same(Sender{Sandbox: "sb", Task: "t2"}) || a.Same(Sender{Sandbox: "sb"}) || a.Same(Sender{Sandbox: "x", Task: "t1"}) {
		t.Error("different callers matched")
	}
	if (Sender{}).Same(Sender{}) {
		t.Error("empty senders matched")
	}
}

func TestMessageJSONShape(t *testing.T) {
	m, err := NewMessage(Draft{Kind: KindRequest, From: Sender{Agent: "task", Sandbox: "sb"}, Body: "q"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(m)
	var raw map[string]any
	_ = json.Unmarshal(data, &raw)
	for _, k := range []string{"version", "id", "kind", "from", "recipient", "subject", "body", "created_at", "expires_at", "received_at"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing %q in %s", k, data)
		}
	}
	if raw["received_at"] != nil {
		t.Errorf("received_at = %v, want null", raw["received_at"])
	}
}
