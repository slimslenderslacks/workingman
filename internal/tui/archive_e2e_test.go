package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/slimslenderslacks/work/internal/agent"
	"github.com/slimslenderslacks/work/internal/audit"
	"github.com/slimslenderslacks/work/internal/daemon"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/runner"
	"github.com/slimslenderslacks/work/internal/workspace"
)

// archiveE2ELauncher is the e2e fake launcher with a memory: it records every
// Spec the runner hands it so the test can assert what the daemon dispatched,
// and it can end one session on demand, standing in for the archive agent's
// claude process exiting.
type archiveE2ELauncher struct {
	mu       sync.Mutex
	specs    []agent.Spec
	sessions []*e2eFakeSession
}

func (l *archiveE2ELauncher) Launch(_ context.Context, spec agent.Spec) (agent.Session, error) {
	// Mirror the real TmuxLauncher's "session:window" name shape.
	s := newE2EFakeSession(agent.DefaultUmbrellaSession + ":" + spec.Name)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.specs = append(l.specs, spec)
	l.sessions = append(l.sessions, s)
	return s, nil
}

func (l *archiveE2ELauncher) launched() []agent.Spec {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]agent.Spec(nil), l.specs...)
}

func (l *archiveE2ELauncher) countKind(k agent.Kind) int {
	n := 0
	for _, s := range l.launched() {
		if s.Kind == k {
			n++
		}
	}
	return n
}

// firstSpecOfKind returns the first Spec launched for kind k, or false if the
// runner never launched one.
func (l *archiveE2ELauncher) firstSpecOfKind(k agent.Kind) (agent.Spec, bool) {
	for _, s := range l.launched() {
		if s.Kind == k {
			return s, true
		}
	}
	return agent.Spec{}, false
}

// closeKind ends the first still-open session of kind k — the agent process
// exiting, which is what triggers the daemon's session-end bookkeeping.
func (l *archiveE2ELauncher) closeKind(t *testing.T, k agent.Kind) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, spec := range l.specs {
		if spec.Kind == k {
			_ = l.sessions[i].Close()
			return
		}
	}
	t.Fatalf("no %s session was launched", k)
}

func (l *archiveE2ELauncher) shutdown() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.sessions {
		_ = s.Close()
	}
}

// cleanupHarness wires the pieces of the cleanup/archive loop together at the
// seams the rest of the package's tests already use: a live daemon with
// fsnotify, a stub wsp manager (real directories, no `wsp` binary), a fake
// launcher instead of tmux+claude, and a headless TUI model whose projects come
// from a real ScanProjects pass over the orch root. Nothing here shells out or
// touches the network.
//
// The same stub workspace manager is both the daemon's provisioner and the
// model's wspRemover, so the workspace `:archive` tears down is exactly the one
// the daemon created for the archive agent.
type cleanupHarness struct {
	root     string // orch root the daemon watches
	stubRoot string // where stub wsp workspaces live
	name     string // work stream (project directory) name
	branch   string
	projPath string
	wspDir   string
	audit    *e2eSafeBuf
	launcher *archiveE2ELauncher
	wsp      *workspace.StubManager
	model    model
}

func newCleanupHarness(t *testing.T, name, branch string) *cleanupHarness {
	t.Helper()
	h := &cleanupHarness{
		name:     name,
		branch:   branch,
		root:     t.TempDir(),
		stubRoot: t.TempDir(),
		audit:    &e2eSafeBuf{},
		launcher: &archiveE2ELauncher{},
	}
	h.wsp = workspace.NewStub(h.stubRoot)
	h.wspDir = filepath.Join(h.stubRoot, branch)
	h.projPath = filepath.Join(h.root, name, ".project.yaml")

	a := audit.New(h.audit)
	d, err := daemon.New([]string{h.root}, a, daemon.WithRunner(&runner.Runner{
		Workspaces: h.wsp,
		Launcher:   h.launcher,
		Audit:      a,
		// The fake launcher ignores the command; return something harmless.
		Command: func(agent.Kind, string) []string { return []string{"true"} },
	}))
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = d.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		h.launcher.shutdown()
		<-done
	})
	if err := waitForString(h.audit, "watch_root", 3*time.Second); err != nil {
		t.Fatalf("daemon never started watching:\n%s", h.audit.String())
	}

	// A finished work stream, written by an agent the way the real ones write
	// it. status:done is terminal, so nothing but the cleanup request can
	// dispatch an agent for it — any launch the assertions see came from
	// `:cleanup`.
	if err := os.MkdirAll(filepath.Dir(h.projPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := project.SaveAs(h.projPath, &project.Project{
		Description: "ship the " + name,
		Branch:      branch,
		Status:      project.StatusDone,
		Repos:       []project.Repo{{Org: "octo", Name: "widget"}},
	}, project.WriterAgent); err != nil {
		t.Fatalf("save project: %v", err)
	}

	h.model = newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	h.model.projectRoot = h.root
	h.model.wspRemover = h.wsp
	step, _ := h.model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	h.model = step.(model)
	h.refresh(t)
	h.model.projSel = h.projPath
	h.model = focusProjectsPane(t, h.model)
	return h
}

// refresh feeds the model the same projectsMsg the WatchProjects goroutine
// delivers in production, built from a real scan of the orch root — so what the
// gallery renders is whatever is on disk right now.
func (h *cleanupHarness) refresh(t *testing.T) {
	t.Helper()
	views, err := ScanProjects([]string{h.root})
	if err != nil {
		t.Fatalf("ScanProjects: %v", err)
	}
	step, _ := h.model.Update(projectsMsg{views: views})
	h.model = step.(model)
}

// selectedView returns the gallery's view of the harness's work stream.
func (h *cleanupHarness) selectedView(t *testing.T) ProjectView {
	t.Helper()
	for _, v := range h.model.projects {
		if v.Path == h.projPath {
			return v
		}
	}
	t.Fatalf("work stream %s missing from the gallery: %+v", h.projPath, h.model.projects)
	return ProjectView{}
}

func (h *cleanupHarness) loadProject(t *testing.T) *project.Project {
	t.Helper()
	p, err := project.Load(h.projPath)
	if err != nil {
		t.Fatalf("load project: %v", err)
	}
	return p
}

// run executes one `:` command against the harness's model.
func (h *cleanupHarness) run(t *testing.T, cmd string) {
	t.Helper()
	m, _ := runProjectCommand(t, h.model, cmd)
	h.model = m
}

// TestCleanupToArchiveEndToEnd walks the whole loop the cleanup/archive
// workflow was built for, one seam at a time:
//
//	:cleanup writes the request flag → the daemon dispatches an archive agent
//	for that project → the agent's `archive: true` lands in .project.yaml →
//	the gallery renders the card with the blue border → `:archive` proceeds,
//	removes the wsp workspace, and moves the project into the backup root.
func TestCleanupToArchiveEndToEnd(t *testing.T) {
	h := newCleanupHarness(t, "weather-tui", "feat/weather")

	// 1. `:cleanup` — the request flag, written as the agent so the daemon
	// doesn't filter its own marker out of the fsnotify event.
	h.run(t, "cleanup")
	if h.model.mode != modeNormal {
		t.Errorf("`cleanup` should act immediately; mode = %v", h.model.mode)
	}
	if !strings.Contains(h.model.statusMsg, "requested cleanup") {
		t.Errorf("statusMsg = %q, want the cleanup request confirmed", h.model.statusMsg)
	}
	requested := h.loadProject(t)
	if !requested.Cleanup {
		t.Fatalf("cleanup flag not written: %+v", requested)
	}
	if requested.UpdatedBy != project.WriterAgent {
		t.Errorf("request written by %q, want agent (the daemon drops its own writes)", requested.UpdatedBy)
	}
	if requested.Archive {
		t.Errorf("`:cleanup` set archive: true itself; that is the agent's job")
	}

	// 2. The daemon picks the request up and dispatches the archive agent into
	// the project's wsp workspace.
	if err := waitForString(h.audit, "archive_dispatch", 4*time.Second); err != nil {
		t.Fatalf("cleanup request never dispatched an archive agent:\n%s", h.audit.String())
	}
	if err := waitForCondition(2*time.Second, func() bool {
		return h.launcher.countKind(agent.ArchiveAgent) == 1
	}); err != nil {
		t.Fatalf("archive launches = %d, want 1:\n%s",
			h.launcher.countKind(agent.ArchiveAgent), h.audit.String())
	}
	spec, ok := h.launcher.firstSpecOfKind(agent.ArchiveAgent)
	if !ok {
		t.Fatal("no archive spec recorded")
	}
	if spec.Workspace != h.wspDir {
		t.Errorf("archive agent workspace = %q, want the wsp workspace %q", spec.Workspace, h.wspDir)
	}
	if _, err := os.Stat(h.wspDir); err != nil {
		t.Fatalf("wsp workspace was not provisioned: %v", err)
	}
	// The request outranks status routing and doesn't spill into other kinds.
	if got := len(h.launcher.launched()); got != 1 {
		t.Errorf("launched %d agents for one `:cleanup`, want 1: %+v", got, h.launcher.launched())
	}

	// 3. The agent's outcome: it reports success by writing `archive: true`,
	// leaving the request flag for the daemon to clear when its session ends.
	// Re-read first, the way an agent editing the file in place does — the
	// daemon has stamped `created_at` by now, and writing back the copy loaded
	// before the dispatch would drop it (and get it re-stamped mid-clear).
	succeeded := *h.loadProject(t)
	succeeded.Archive = true
	if err := project.SaveAs(h.projPath, &succeeded, project.WriterAgent); err != nil {
		t.Fatalf("agent writeback: %v", err)
	}
	h.launcher.closeKind(t, agent.ArchiveAgent)

	if err := waitForString(h.audit, "cleanup_flag_cleared", 4*time.Second); err != nil {
		t.Fatalf("daemon never cleared the request flag:\n%s", h.audit.String())
	}
	if err := waitForCondition(2*time.Second, func() bool {
		p, err := project.Load(h.projPath)
		return err == nil && !p.Cleanup && p.Archive
	}); err != nil {
		t.Fatalf("project file never settled on archive: true / cleanup cleared: %+v\n%s",
			h.loadProject(t), h.audit.String())
	}
	if got := h.loadProject(t).Status; got != project.StatusDone {
		t.Errorf("status = %q, want it left at done — the cleanup owns `archive`, not the status", got)
	}
	// A finished cleanup must not leave anything behind that re-dispatches.
	if got := h.launcher.countKind(agent.ArchiveAgent); got != 1 {
		t.Errorf("archive launches for a single `:cleanup` = %d, want 1", got)
	}

	// 4. The gallery picks the flag up on its next scan and gives the card the
	// blue border. Colours are stripped without a TTY, so assert on the style
	// the renderer chooses (see projectCardBorder).
	h.refresh(t)
	view := h.selectedView(t)
	if !view.Archive {
		t.Fatalf("gallery view missing the archive flag: %+v", view)
	}
	got := projectCardBorder(view, false).GetBorderTopForeground()
	if want := cardArchivedBorder.GetBorderTopForeground(); got != want {
		t.Errorf("card border = %v, want the archived blue %v", got, want)
	}
	if plain := cardBorder.GetBorderTopForeground(); got == plain {
		t.Errorf("archived card border is indistinguishable from a normal card (%v)", plain)
	}
	if !strings.Contains(h.model.View(), h.name) {
		t.Errorf("work stream %q missing from the rendered gallery:\n%s", h.name, h.model.View())
	}

	// 5. `:archive` now proceeds: the guard passes, the confirm modal opens, and
	// `y` removes the wsp workspace and moves the project to the backup root.
	h.run(t, "archive")
	if h.model.mode != modeConfirmArchive {
		t.Fatalf("mode = %v, want modeConfirmArchive for a cleaned-up work stream (statusMsg=%q)",
			h.model.mode, h.model.statusMsg)
	}
	step, _ := h.model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	h.model = step.(model)

	if h.model.mode != modeNormal {
		t.Errorf("after y, mode = %v, want modeNormal", h.model.mode)
	}
	if !strings.Contains(h.model.statusMsg, "archived "+h.name) {
		t.Errorf("statusMsg = %q, want an archived confirmation", h.model.statusMsg)
	}
	if _, err := os.Stat(filepath.Join(h.root, h.name)); !os.IsNotExist(err) {
		t.Errorf("project dir still under the orch root (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(h.root+".backup", h.name, ".project.yaml")); err != nil {
		t.Errorf("archived project missing from the backup root: %v", err)
	}
	if _, err := os.Stat(h.wspDir); !os.IsNotExist(err) {
		t.Errorf("wsp workspace %s survived the archive (err=%v)", h.wspDir, err)
	}
	// And the gallery drops the work stream on its next scan.
	h.refresh(t)
	for _, v := range h.model.projects {
		if v.Path == h.projPath {
			t.Errorf("archived work stream still in the gallery: %+v", v)
		}
	}
}

// TestArchiveWithoutCleanupRefusesAndChangesNothing is the negative path of the
// same loop: `:archive` on a work stream the cleanup agent has never finished
// warns and stops, leaving the project directory and its wsp workspace exactly
// where they were.
func TestArchiveWithoutCleanupRefusesAndChangesNothing(t *testing.T) {
	h := newCleanupHarness(t, "messy-tui", "feat/messy")

	// Give the work stream a real wsp workspace so "untouched" means something.
	if _, err := h.wsp.Create(context.Background(), h.branch, nil); err != nil {
		t.Fatalf("provision workspace: %v", err)
	}

	h.refresh(t)
	if view := h.selectedView(t); view.Archive {
		t.Fatalf("work stream starts out cleaned up; the test premise is wrong: %+v", view)
	}

	h.run(t, "archive")

	if h.model.mode != modeNormal {
		t.Errorf("mode = %v, want modeNormal — no confirm modal for an uncleaned work stream", h.model.mode)
	}
	if h.model.archiveTarget != "" {
		t.Errorf("archiveTarget = %q, want empty", h.model.archiveTarget)
	}
	if !strings.Contains(h.model.statusMsg, "cleaned up") || !strings.Contains(h.model.statusMsg, ":cleanup") {
		t.Errorf("statusMsg = %q, want a warning that points at :cleanup", h.model.statusMsg)
	}

	// Nothing moved: project tree, backup root, workspace.
	if _, err := os.Stat(h.projPath); err != nil {
		t.Errorf("project should be intact after the refusal: %v", err)
	}
	if _, err := os.Stat(h.root + ".backup"); !os.IsNotExist(err) {
		t.Errorf("backup root should not exist after a refused archive (err=%v)", err)
	}
	if _, err := os.Stat(h.wspDir); err != nil {
		t.Errorf("wsp workspace should be untouched after the refusal: %v", err)
	}
	// The refusal is local to the TUI — it must not have poked the daemon.
	if got := len(h.launcher.launched()); got != 0 {
		t.Errorf("a refused archive launched %d agents: %+v", got, h.launcher.launched())
	}
	p := h.loadProject(t)
	if p.Archive || p.Cleanup {
		t.Errorf("refused archive wrote flags to the project file: %+v", p)
	}
}
