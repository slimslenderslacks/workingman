package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/agent"
)

// writeTaskFile drops a minimal valid task YAML into <dir>/tasks/<name>.yaml.
func writeTaskFile(t *testing.T, projectDir, name, status string) {
	t.Helper()
	tasksDir := filepath.Join(projectDir, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatalf("mkdir tasks: %v", err)
	}
	body := "name: " + name + "\n" +
		"description: x\n" +
		"depends_on: []\n" +
		"status: " + status + "\n" +
		"attempts: 0\n" +
		"failure_reason: \"\"\n" +
		"blocked_reason: \"\"\n" +
		"model: default\n"
	if err := os.WriteFile(filepath.Join(tasksDir, name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write task: %v", err)
	}
}

func TestStrandedReasonTaskStages(t *testing.T) {
	d, _ := newTestDaemon(t) // runner nil → sessionIdle measures from startedAt

	cases := []struct {
		name       string
		kind       agent.Kind
		taskStatus string
		startedAgo time.Duration
		wantReap   bool
		wantCause  string // only checked when wantReap
	}{
		// Task agent that recorded success but whose wrapper lingers past the
		// done-grace is the canonical zombie — reap it.
		{"success past grace", agent.TaskAgent, "success", 2 * sessionDoneGrace, true, reapCauseStageComplete},
		// Same status but still inside the grace window: give the normal exit a
		// chance first.
		{"success within grace", agent.TaskAgent, "success", sessionDoneGrace / 2, false, ""},
		// failed is also terminal for a task agent.
		{"failed past grace", agent.TaskAgent, "failed", 2 * sessionDoneGrace, true, reapCauseStageComplete},
		// A still-running task that keeps streaming (recent activity) is healthy.
		{"running recent", agent.TaskAgent, "running", time.Second, false, ""},
		// A running task gone silent past the idle bound is a mid-turn hang.
		{"running idle", agent.TaskAgent, "running", defaultSessionIdleTimeout + time.Minute, true, reapCauseIdleTimeout},
		// Commit agent: terminal only at committed.
		{"commit committed past grace", agent.CommitAgent, "committed", 2 * sessionDoneGrace, true, reapCauseStageComplete},
		{"commit success not terminal", agent.CommitAgent, "success", sessionDoneGrace * 2, false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projectDir := t.TempDir()
			key := filepath.Join(projectDir, ".project.yaml")
			writeTaskFile(t, projectDir, "t1", tc.taskStatus)

			e := sessionEntry{
				sess:      newStubSession("task-t1"),
				kind:      tc.kind,
				startedAt: time.Now().Add(-tc.startedAgo),
				taskName:  "t1",
			}
			v := d.strandedVerdict(key, e)
			if v.reap != tc.wantReap {
				t.Fatalf("strandedVerdict reap=%v (cause=%q), want reap=%v", v.reap, v.cause, tc.wantReap)
			}
			if tc.wantReap && v.cause != tc.wantCause {
				t.Fatalf("strandedVerdict cause=%q, want %q", v.cause, tc.wantCause)
			}
		})
	}
}

// TestReapStrandedSessionRunsOnEnd verifies the full path: a tracked session the
// reaper deems stranded is Closed, which unblocks trackSession's wait goroutine,
// removes it from the map, and runs the onEnd callback (where the daemon would
// dispatch the next stage).
func TestReapStrandedSessionRunsOnEnd(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.sessionIdleTimeout = time.Millisecond // make the idle path fire immediately

	projectDir := t.TempDir()
	key := filepath.Join(projectDir, ".project.yaml")
	// No tasks dir → stageComplete is false, so reaping is driven purely by the
	// idle bound.

	ended := make(chan struct{})
	sess := newStubSession("task-zombie")
	if !d.trackSession(key, sess, agent.TaskAgent, "zombie", func(error) { close(ended) }) {
		t.Fatal("trackSession returned false")
	}

	time.Sleep(5 * time.Millisecond) // exceed the 1ms idle bound
	d.reapStrandedSessions()

	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("onEnd not invoked — reaper did not close the stranded session")
	}
	if d.hasSession(key) {
		t.Fatal("session still tracked after reap")
	}
}
