package sbx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	warnLine    = "WARN: docker hub refresh lock held by another process: context deadline exceeded"
	jsonWarnLog = `{"time":"2026-11-22T23:53:01Z","level":"WARN","msg":"docker hub refresh lock held by another process: context deadline exceeded"}`
)

// DecodeList skips WARN and JSON log lines before and after the list, and
// never mistakes a log object for the {"sandboxes": [...]} shape.
func TestDecodeListSkipsNoise(t *testing.T) {
	const bare = `[{"name":"a","workspaces":["/x"]},{"name":"b"}]`
	const wrapped = `{"sandboxes":[{"name":"a","workspaces":["/x"]},{"name":"b"}]}`
	for _, tc := range []struct{ name, out string }{
		{"warn after bare", bare + "\n" + warnLine + "\n"},
		{"warn after bare, same line", bare + warnLine},
		{"warn after wrapped", wrapped + "\n" + warnLine},
		{"warn before", warnLine + "\n" + bare},
		{"json log before", jsonWarnLog + "\n" + wrapped},
		{"json log after", bare + "\n" + jsonWarnLog + "\n"},
		{"json log before and after", jsonWarnLog + "\n" + warnLine + "\n" + wrapped + "\n" + jsonWarnLog},
		{"pretty printed", warnLine + "\n[\n  {\"name\": \"a\", \"workspaces\": [\"/x\"]},\n  {\"name\": \"b\"}\n]\n" + warnLine},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list, err := DecodeList([]byte(tc.out))
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 2 || list[0].Name != "a" || list[0].Workspaces[0] != "/x" || list[1].Name != "b" {
				t.Errorf("DecodeList = %+v, want a and b", list)
			}
		})
	}
	for _, bad := range []string{"", warnLine, jsonWarnLog, jsonWarnLog + "\n" + warnLine, `{}`, warnLine + "\n" + `[{"name":`} {
		if list, err := DecodeList([]byte(bad)); err == nil {
			t.Errorf("DecodeList(%q) = %+v, want an error", bad, list)
		}
	}
}

// DecodeSecretList likewise ignores what follows the document.
func TestDecodeSecretListIgnoresTrailingNoise(t *testing.T) {
	for _, out := range []string{
		globalSecret + "\n" + warnLine,
		globalSecret + warnLine,
		jsonWarnLog + "\n" + globalSecret + "\n" + jsonWarnLog,
	} {
		data, err := DecodeSecretList([]byte(out))
		if err != nil {
			t.Fatalf("DecodeSecretList(%q): %v", out, err)
		}
		if global, _ := gatewayScopes(data, "s"); !global {
			t.Errorf("DecodeSecretList(%q): no global secret", out)
		}
	}
}

func TestClientListRetriesThenSucceeds(t *testing.T) {
	var calls int32
	var slept []time.Duration
	var logs []string
	var argv []string
	inner := script(&calls,
		result{stderr: warnLine, err: errors.New("exit status 1")},
		result{stdout: warnLine + "\n"},
		result{stdout: `[{"name":"a"}]` + "\n" + warnLine},
	)
	q := Client{
		Run: func(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
			argv = append([]string{name}, args...)
			return inner(ctx, name, args...)
		},
		SbxPath: "/bin/sbx",
		Sleep:   func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil },
		Jitter:  func(d time.Duration) time.Duration { return d },
		Logf:    func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}
	list, err := q.List(context.Background())
	if err != nil || len(list) != 1 || list[0].Name != "a" {
		t.Fatalf("List = %+v, %v; want [a]", list, err)
	}
	if strings.Join(argv, " ") != "/bin/sbx ls --json" {
		t.Errorf("ran %v, want /bin/sbx ls --json", argv)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
	if want := "[200ms 400ms]"; fmt.Sprint(slept) != want {
		t.Errorf("slept %v, want %s", slept, want)
	}
	if len(logs) != 2 || !strings.Contains(logs[0], "sbx ls --json attempt 1/5") || !strings.Contains(logs[0], "docker hub refresh lock") {
		t.Errorf("logs = %q, want one per retry naming the command and quoting the failure", logs)
	}
}

func TestClientListExhaustion(t *testing.T) {
	var calls int32
	q := Client{
		Run:   script(&calls, result{stdout: jsonWarnLog + "\n" + warnLine}),
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	_, err := q.List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sbx ls --json failed after 5 attempts") || !strings.Contains(err.Error(), "docker hub refresh lock") {
		t.Fatalf("err = %v, want the exhaustion error naming sbx ls --json and quoting its output", err)
	}
	if calls != 5 {
		t.Errorf("calls = %d, want 5", calls)
	}
}

// A single-attempt Client still names the command, without "after 1 attempts".
func TestClientSingleAttemptError(t *testing.T) {
	q := Client{Attempts: 1, Run: script(new(int32), result{stderr: "boom", err: errors.New("exit status 1")})}
	_, err := q.List(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "sbx ls --json: exit status 1") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestDefaultLockPath(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	if got, want := DefaultLockPath(), "/home/u/.workingman/locks/sbx.lock"; got != want {
		t.Errorf("DefaultLockPath = %q, want %q", got, want)
	}
	if got, want := LockPath("/state/sessions"), "/state/locks/sbx.lock"; got != want {
		t.Errorf("LockPath = %q, want %q", got, want)
	}
}
