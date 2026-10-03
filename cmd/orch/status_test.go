package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/daemon"
)

func writeStatusFixture(t *testing.T) (root, auditLog string) {
	t.Helper()
	root = t.TempDir()
	proj := filepath.Join(root, "demo")
	if err := os.MkdirAll(filepath.Join(proj, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	must := func(p, body string) {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(proj, ".project.yaml"), "description: demo\nbranch: demo-branch\nstatus: blocked\nblocked_reason: \"task one failed; api_key=sk-abcdefghijklmnopqrstuv\"\nupdated_by: daemon\n")
	must(filepath.Join(proj, "tasks", "one.yaml"), "name: one\ndescription: d\ndepends_on: []\nstatus: failed\nattempts: 3\nfailure_reason: boom\n")
	must(filepath.Join(proj, "tasks", "two.yaml"), "name: two\ndescription: d\ndepends_on: [one]\nstatus: ready\nattempts: 0\n")
	auditLog = filepath.Join(t.TempDir(), "audit.log")
	must(auditLog, "2026-10-02T10:00:00Z daemon_start pid=1\n2026-10-02T10:00:05Z wolf_dispatch path=/x reason=\"task failed\"\n")
	return root, auditLog
}

func TestStatusOfflineText(t *testing.T) {
	root, auditLog := writeStatusFixture(t)
	var out, errb bytes.Buffer
	code := runStatus([]string{"--root", root, "--audit-log", auditLog, "--sessions-root", t.TempDir()}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{
		"source: offline", "unavailable", "demo  [blocked]", "branch=demo-branch",
		"1 ready, 1 failed", "one", "failed", "attempts=3", "deps=one",
		"daemon_start", "wolf_dispatch", `reason="task failed"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "sk-abcdefghijklmnopqrstuv") {
		t.Errorf("secret leaked:\n%s", got)
	}
}

func TestStatusOfflineJSON(t *testing.T) {
	root, auditLog := writeStatusFixture(t)
	var out, errb bytes.Buffer
	if code := runStatus([]string{"--json", "--root", root, "--audit-log", auditLog, "--sessions-root", t.TempDir(), "--audit-events", "1"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	var s daemon.Snapshot
	if err := json.Unmarshal(out.Bytes(), &s); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out.String())
	}
	if s.Version != daemon.SnapshotVersion || s.Source != "offline" || s.LiveStateAvailable || len(s.Notes) == 0 {
		t.Errorf("header = %+v", s)
	}
	if len(s.Projects) != 1 || s.Projects[0].WorkStream != "demo" || s.Projects[0].TaskCounts["failed"] != 1 {
		t.Errorf("projects = %+v", s.Projects)
	}
	if len(s.AuditEvents) != 1 || s.AuditEvents[0].Event != "wolf_dispatch" {
		t.Errorf("--audit-events 1 should keep only the latest: %+v", s.AuditEvents)
	}
	// Raw JSON keys are the documented, stable ones.
	var raw map[string]any
	_ = json.Unmarshal(out.Bytes(), &raw)
	for _, k := range []string{"version", "generated_at", "source", "live_state_available", "roots", "projects", "sessions", "audit_events"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("JSON missing top-level key %q", k)
		}
	}
	if strings.Contains(out.String(), "sk-abcdefghijklmnopqrstuv") {
		t.Error("secret leaked in JSON")
	}
}

func TestStatusReadsDaemonSnapshotFile(t *testing.T) {
	root, auditLog := writeStatusFixture(t)
	off := daemon.BuildOfflineSnapshot(daemon.OfflineOptions{Roots: []string{root}, AuditLog: auditLog, SessionsRoot: t.TempDir()})
	// Present it as what a live daemon would publish.
	off.Source = daemon.SnapshotSourceDaemon
	off.LiveStateAvailable = true
	off.Notes = nil
	off.Daemon = &daemon.DaemonInfo{State: daemon.DaemonStateRunning, PID: 4242, StartedAt: time.Now().Add(-time.Hour).UTC(), WorkspaceManager: "wsp", Roots: []string{root}}
	off.Projects[0].Live = &daemon.ProjectLive{Sessions: []string{"wolf"}, WolfInFlight: true, PlanningFailures: 2}

	stateFile := filepath.Join(t.TempDir(), "state", "snapshot.json")
	var buf bytes.Buffer
	if err := off.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteFileAtomic(stateFile, buf.Bytes()); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := runStatus([]string{"--state-file", stateFile}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{"source: daemon", "daemon: running pid=4242", "live sessions (0)", "wolf-in-flight", "planning_failures=2"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "unavailable") {
		t.Errorf("a daemon snapshot must not claim live data is unavailable:\n%s", got)
	}
}

func TestStatusWarnsOnStaleDaemonSnapshot(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "snapshot.json")
	s := &daemon.Snapshot{
		Version: daemon.SnapshotVersion, GeneratedAt: time.Now().Add(-10 * time.Minute).UTC(),
		Source: daemon.SnapshotSourceDaemon, LiveStateAvailable: true,
		Daemon: &daemon.DaemonInfo{State: daemon.DaemonStateRunning, PID: 1},
	}
	var buf bytes.Buffer
	_ = s.WriteJSON(&buf)
	if err := os.WriteFile(stateFile, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := runStatus([]string{"--state-file", stateFile}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "old") || !strings.Contains(out.String(), "may have stopped") {
		t.Errorf("no staleness warning:\n%s", out.String())
	}
}

func TestStatusErrors(t *testing.T) {
	var out, errb bytes.Buffer
	missing := filepath.Join(t.TempDir(), "nope.json")
	if code := runStatus([]string{"--state-file", missing}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "no daemon snapshot") || !strings.Contains(errb.String(), "--root") {
		t.Errorf("missing snapshot: code=%d stderr=%q", code, errb.String())
	}
	errb.Reset()
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte("{not json"), 0o644)
	if code := runStatus([]string{"--state-file", bad}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "parse snapshot") {
		t.Errorf("corrupt snapshot: code=%d stderr=%q", code, errb.String())
	}
	errb.Reset()
	if code := runStatus([]string{"--bogus"}, &out, &errb); code != 2 {
		t.Errorf("bad flag: code=%d", code)
	}
}

func TestResolveStateFile(t *testing.T) {
	if p, err := resolveStateFile("off", ""); err != nil || p != "" {
		t.Errorf("off -> %q, %v", p, err)
	}
	if p, err := resolveStateFile("", "/h/.workingman/sessions"); err != nil || p != "/h/.workingman/state/snapshot.json" {
		t.Errorf("default -> %q, %v", p, err)
	}
	if p, err := resolveStateFile("/tmp/x/s.json", "/ignored"); err != nil || p != "/tmp/x/s.json" {
		t.Errorf("explicit -> %q, %v", p, err)
	}
}
