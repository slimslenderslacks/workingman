package audit

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRedact(t *testing.T) {
	secrets := []string{
		"ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"github_pat_11ABCDEFG0abcdefghijklmnop_qrstuvwxyz",
		"sk-ant-api03-abcdefghijklmnopqrstuvwx",
		"xoxb-1234567890-abcdefghij",
		"AKIAABCDEFGHIJKLMNOP",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijk",
		"hunter2hunter2",
	}
	cases := []string{
		"clone failed token=%s for repo",
		"msg=\"api_key: %s\" next=1",
		"Authorization: Bearer %s",
		"GITHUB_TOKEN=%s",
		"url=https://user:%s@github.com/x/y.git",
		"password=%s",
		"found %s in output",
	}
	for _, sec := range secrets {
		for _, c := range cases {
			in := fmt.Sprintf(c, sec)
			if strings.Contains(c, "found") && !strings.Contains(sec, "_") && !strings.Contains(sec, "-") && !strings.HasPrefix(sec, "AKIA") && !strings.HasPrefix(sec, "eyJ") {
				continue // a bare arbitrary word isn't a recognisable secret
			}
			got := Redact(in)
			if strings.Contains(got, sec) {
				t.Errorf("Redact(%q) = %q still contains secret", in, got)
			}
		}
	}
}

func TestRedactKeepsOrdinaryText(t *testing.T) {
	for _, in := range []string{
		"2026-10-02T10:00:00Z session_ended key=/x/.project.yaml name=task-1",
		"commit 9315099abcdef0123456789abcdef0123456789a pushed",
		"status=blocked reason=\"task failed 3 times\"",
		"https://github.com/NousResearch/hermes-agent/pull/12",
	} {
		if got := Redact(in); got != in {
			t.Errorf("Redact(%q) = %q, want unchanged", in, got)
		}
	}
	if got := Redact("token=abc"); got != "token="+Redacted {
		t.Errorf("got %q", got)
	}
	if got := Redact(Redact("token=abc")); got != "token="+Redacted {
		t.Errorf("not idempotent: %q", got)
	}
}

func TestRedactPrivateKeyBlock(t *testing.T) {
	in := "before -----BEGIN OPENSSH PRIVATE KEY-----\nAAAAB3Nza\nC1rZXk=\n-----END OPENSSH PRIVATE KEY----- after"
	got := Redact(in)
	if strings.Contains(got, "AAAAB3Nza") || !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Errorf("got %q", got)
	}
}

func TestParseLineRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	l.now = func() time.Time { return time.Date(2026, 5, 12, 18, 4, 0, 0, time.UTC) }
	l.Log("session_ended", "key", "/a b/.project.yaml", "err", `exit "1" now`, "empty", "", "plain", "x=y")
	ev, ok := ParseLine(buf.String())
	if !ok {
		t.Fatalf("not parsed: %q", buf.String())
	}
	if ev.Name != "session_ended" || !ev.Time.Equal(time.Date(2026, 5, 12, 18, 4, 0, 0, time.UTC)) {
		t.Errorf("ev = %+v", ev)
	}
	want := map[string]string{"key": "/a b/.project.yaml", "err": `exit "1" now`, "empty": "", "plain": "x=y"}
	for k, v := range want {
		if ev.Fields[k] != v {
			t.Errorf("field %s = %q, want %q", k, ev.Fields[k], v)
		}
	}
}

func TestParseLineRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "nonsense", "notatime event k=v", "2026-05-12T18:04:00Z"} {
		if _, ok := ParseLine(in); ok {
			t.Errorf("ParseLine(%q) ok, want false", in)
		}
	}
	ev, ok := ParseLine("2026-05-12T18:04:00Z daemon_stop")
	if !ok || ev.Name != "daemon_stop" || len(ev.Fields) != 0 {
		t.Errorf("bare event: %+v ok=%v", ev, ok)
	}
}

func TestRecentSeedAndOnLog(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	l.Seed([]string{"2026-01-01T00:00:00Z old a=1"})
	calls := 0
	l.OnLog(func() { calls++ })
	l.Log("new", "b", "2")
	if calls != 1 {
		t.Errorf("OnLog calls = %d", calls)
	}
	got := l.Recent(10)
	if len(got) != 2 || !strings.HasSuffix(got[0], "old a=1") || !strings.HasSuffix(got[1], "new b=2") {
		t.Errorf("Recent = %q", got)
	}
	if strings.Contains(buf.String(), "old") {
		t.Error("Seed must not write to the underlying writer")
	}
	if got := l.Recent(1); len(got) != 1 || !strings.HasSuffix(got[0], "new b=2") {
		t.Errorf("Recent(1) = %q", got)
	}
	for i := 0; i < recentCap+10; i++ {
		l.Log("e", "i", fmt.Sprint(i))
	}
	if n := len(l.Recent(10000)); n != recentCap {
		t.Errorf("ring size = %d, want %d", n, recentCap)
	}
}

func TestTailFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	if lines, err := TailFile(path, 5); err != nil || lines != nil {
		t.Fatalf("missing file: %v %v", lines, err)
	}
	var b strings.Builder
	for i := 0; i < 5000; i++ { // > one tailChunk
		fmt.Fprintf(&b, "2026-01-01T00:00:00Z evt i=%d pad=%s\n", i, strings.Repeat("x", 40))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := TailFile(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || !strings.Contains(lines[0], "i=4997 ") || !strings.Contains(lines[2], "i=4999 ") {
		t.Errorf("tail = %q", lines)
	}
	all, _ := TailFile(path, 100000)
	if len(all) != 5000 || !strings.Contains(all[0], "i=0 ") {
		t.Errorf("full tail len=%d first=%q", len(all), all[0])
	}
}
