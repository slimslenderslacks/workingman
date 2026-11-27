package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slimslenderslacks/work/internal/api"
)

// TestAPIIntakeSourceNotAPRFix: a task carrying an intake back-reference is
// served with its source as written, and is never counted among a PR's fix
// tasks — even when a confused planner also wrote the PR's number on it.
func TestAPIIntakeSourceNotAPRFix(t *testing.T) {
	f := newAPIFixture(t)
	path := filepath.Join(filepath.Dir(f.alpha), "tasks", "ship.yaml")
	body := "name: ship\ndescription: ship it\ndepends_on: [build]\nstatus: ready\n" +
		"source: {kind: intake, ref: add-svg, repo: acme/widgets, pr: 7}\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	var one TaskResource
	f.get(t, api.Prefix+"/projects/"+alphaID+"/tasks/ship", &one)
	if one.Source == nil || one.Source.Kind != "intake" || one.Source.Ref != "add-svg" {
		t.Errorf("ship source = %+v, want intake add-svg", one.Source)
	}

	var l listOf[PullRequest]
	f.get(t, api.Prefix+"/projects/"+alphaID+"/pulls", &l)
	if len(l.Items) != 1 {
		t.Fatalf("pulls = %+v", l.Items)
	}
	if got := strings.Join(l.Items[0].FixTasks, ","); got != "build" {
		t.Errorf("fix_tasks = %q, want only the pr-comment task build", got)
	}
}
