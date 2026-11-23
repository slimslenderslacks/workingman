package taskgraph

import (
	"testing"
	"time"

	"github.com/slimslenderslacks/work/internal/task"
)

func at(min int) *time.Time {
	t := time.Date(2026, 11, 19, 10, min, 0, 0, time.UTC)
	return &t
}

// The queue holds non-push tasks at success and push tasks at ready with every
// dependency committed, ordered by succeeded_at with unstamped tasks first.
func TestQueuedMembershipAndOrder(t *testing.T) {
	push := &task.Source{Kind: task.SourceKindPush}
	tasks := []*task.Task{
		{Name: "a", Status: task.StatusCommitted},
		{Name: "late", Status: task.StatusSuccess, SucceededAt: at(9)},
		{Name: "early", Status: task.StatusSuccess, SucceededAt: at(1)},
		{Name: "legacy", Status: task.StatusSuccess},
		{Name: "push-ready", Status: task.StatusReady, Source: push, DependsOn: []string{"a"}},
		{Name: "push-waiting", Status: task.StatusReady, Source: push, DependsOn: []string{"late"}},
		{Name: "push-success", Status: task.StatusSuccess, Source: push},
		{Name: "plain-ready", Status: task.StatusReady},
		{Name: "running", Status: task.StatusRunning},
	}
	got := names(Queued(tasks))
	want := []string{"legacy", "push-ready", "early", "late"}
	if len(got) != len(want) {
		t.Fatalf("Queued = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Queued = %v, want %v", got, want)
		}
	}
}

func TestWaitingOnIntegration(t *testing.T) {
	tasks := []*task.Task{
		{Name: "a", Status: task.StatusSuccess},
		{Name: "b", Status: task.StatusCommitted},
		{Name: "c", Status: task.StatusSuccess},
	}
	d := &task.Task{Name: "d", Status: task.StatusReady, DependsOn: []string{"c", "b", "a", "gone"}}
	got := WaitingOnIntegration(d, tasks)
	if len(got) != 2 || got[0] != "c" || got[1] != "a" {
		t.Errorf("WaitingOnIntegration = %v, want [c a]", got)
	}
	if got := WaitingOnIntegration(&task.Task{Name: "e"}, tasks); got != nil {
		t.Errorf("no deps: %v", got)
	}
}
