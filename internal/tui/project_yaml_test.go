package tui

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/slimslenderslacks/work/internal/project"
)

// writeManyLineProject writes a .project.yaml whose body is far taller than any
// test viewport, so cursor/scroll behaviour has room to move. Returns its path.
func writeManyLineProject(t *testing.T, lines int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".project.yaml")
	var b strings.Builder
	b.WriteString("description: |\n")
	for i := 0; i < lines; i++ {
		b.WriteString("  line ")
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('\n')
	}
	b.WriteString("status: ready\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProjectYAMLBodyReturnsRawFileContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".project.yaml")
	if err := project.SaveAs(path, &project.Project{
		Description: "alpha description",
		Branch:      "feat/alpha",
		Status:      project.StatusWorking,
		Repos:       []project.Repo{{Org: "acme", Name: "alpha"}},
	}, project.WriterAgent); err != nil {
		t.Fatal(err)
	}

	body, isErr := projectYAMLBody(path)
	if isErr {
		t.Fatalf("projectYAMLBody returned error body: %s", body)
	}
	for _, want := range []string{
		"description: alpha description",
		"branch: feat/alpha",
		"status: working",
		"org: acme",
		"name: alpha",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("YAML body missing %q; got:\n%s", want, body)
		}
	}
}

func TestProjectYAMLBodyEmptySelection(t *testing.T) {
	body, isErr := projectYAMLBody("")
	if isErr {
		t.Errorf("empty selection should not be an error; got %q", body)
	}
	if !strings.Contains(body, "(none)") {
		t.Errorf("empty selection body = %q, want it to contain '(none)'", body)
	}
}

func TestProjectYAMLBodyMissingFile(t *testing.T) {
	body, isErr := projectYAMLBody("/nonexistent/.project.yaml")
	if !isErr {
		t.Errorf("missing file should be an error; got %q", body)
	}
}

func TestProjectYAMLPaneRendersSelectedProject(t *testing.T) {
	dir := t.TempDir()
	pDir := filepath.Join(dir, "alpha")
	if err := os.MkdirAll(pDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(pDir, ".project.yaml")
	if err := project.SaveAs(path, &project.Project{
		Description: "alpha",
		Branch:      "main",
		Status:      project.StatusReady,
	}, project.WriterAgent); err != nil {
		t.Fatal(err)
	}

	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 60})
	m = sized.(model)
	step, _ := m.Update(projectsMsg{views: []ProjectView{
		{Name: "alpha", Path: path, Status: project.StatusReady},
	}})
	m = step.(model)

	view := m.View()
	if !strings.Contains(view, "branch:") {
		t.Errorf("view missing YAML branch field:\n%s", view)
	}
}

func TestReconcileYAMLView(t *testing.T) {
	// contentRows=5, n=20.
	if c, s := reconcileYAMLView(3, 0, 20, 5); c != 3 || s != 0 {
		t.Errorf("in-view: got (%d,%d), want (3,0)", c, s)
	}
	if c, s := reconcileYAMLView(10, 0, 20, 5); c != 10 || s != 6 {
		t.Errorf("scroll down to keep cursor last-visible: got (%d,%d), want (10,6)", c, s)
	}
	if c, s := reconcileYAMLView(2, 8, 20, 5); c != 2 || s != 2 {
		t.Errorf("scroll up to cursor: got (%d,%d), want (2,2)", c, s)
	}
	if c, s := reconcileYAMLView(999, 0, 20, 5); c != 19 || s != 15 {
		t.Errorf("clamp bottom: got (%d,%d), want (19,15)", c, s)
	}
	if c, s := reconcileYAMLView(-5, 3, 20, 5); c != 0 || s != 0 {
		t.Errorf("clamp top: got (%d,%d), want (0,0)", c, s)
	}
	if c, s := reconcileYAMLView(0, 0, 0, 5); c != 0 || s != 0 {
		t.Errorf("empty body: got (%d,%d), want (0,0)", c, s)
	}
}

func TestYAMLCursorAdvancesOnJK(t *testing.T) {
	path := writeManyLineProject(t, 80)
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 60})
	m = sized.(model)
	step, _ := m.Update(projectsMsg{views: []ProjectView{
		{Name: "alpha", Path: path, Status: project.StatusReady},
	}})
	m = step.(model)
	m.projSel = path
	m.focus = paneProjectYAML

	// j advances the cursor; the body is far taller than the viewport so the
	// first step stays scrolled at the top.
	j, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = j.(model)
	if m.yamlCursor != 1 {
		t.Errorf("after j: yamlCursor = %d, want 1", m.yamlCursor)
	}
	if m.yamlScroll != 0 {
		t.Errorf("after j near top: yamlScroll = %d, want 0", m.yamlScroll)
	}
	// k moves it back; k at the top clamps to 0 (no underflow).
	k, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m = k.(model)
	if m.yamlCursor != 0 {
		t.Errorf("after k: yamlCursor = %d, want 0", m.yamlCursor)
	}
	k2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m = k2.(model)
	if m.yamlCursor != 0 || m.yamlScroll != 0 {
		t.Errorf("k at top: cursor=%d scroll=%d, want 0/0", m.yamlCursor, m.yamlScroll)
	}
}

func TestCtrlFCtrlBPageYAMLRegardlessOfFocus(t *testing.T) {
	path := writeManyLineProject(t, 200)
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	// Use a window tall enough that the YAML pane gets a meaningful slice.
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 60})
	m = sized.(model)
	step, _ := m.Update(projectsMsg{views: []ProjectView{
		{Name: "alpha", Path: path, Status: project.StatusReady},
	}})
	m = step.(model)
	m.projSel = path
	// This fixture is one long description block; expand it so there's a full
	// body to page through (the fold is covered by its own test).
	m.yamlExpanded = true

	page := yamlPageSize(m)
	if page <= 0 {
		t.Fatalf("yamlPageSize = %d; need a positive page size for the test (height too small?)", page)
	}

	// Try every pane focus, including Audit — the newest addition to the
	// center-column stack — to prove the binding really is independent of
	// pane focus rather than happening to work only for whichever pane the
	// original test picked.
	for _, focus := range []pane{paneProjects, paneTasks, paneAudit, paneSessions} {
		m.focus = focus
		m.yamlScroll = 0

		fwd, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlF})
		m = fwd.(model)
		if m.yamlScroll != page {
			t.Errorf("ctrl+f from focus=%v: scroll = %d, want %d", focus, m.yamlScroll, page)
		}
		back, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlB})
		m = back.(model)
		if m.yamlScroll != 0 {
			t.Errorf("ctrl+b back to top from focus=%v: scroll = %d, want 0", focus, m.yamlScroll)
		}
		// Ctrl+b at zero must not underflow.
		back2, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlB})
		m = back2.(model)
		if m.yamlScroll != 0 {
			t.Errorf("ctrl+b at zero from focus=%v: scroll = %d, want 0", focus, m.yamlScroll)
		}
	}
}

// TestGJumpsToEndOfYAML pins the vim "G" binding: it scrolls the YAML viewer to
// the bottom of the buffer, lands on the last page, and works regardless of
// which pane holds focus (same independence as ctrl+f/ctrl+b).
func TestGJumpsToEndOfYAML(t *testing.T) {
	path := writeManyLineProject(t, 200)
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 60})
	m = sized.(model)
	step, _ := m.Update(projectsMsg{views: []ProjectView{
		{Name: "alpha", Path: path, Status: project.StatusReady},
	}})
	m = step.(model)
	m.projSel = path
	// One long description block; expand it so there's a real buffer to jump
	// through (the fold has its own test).
	m.yamlExpanded = true

	m.focus = paneProjects
	m = pressKey(m, "G")
	end := m.yamlScroll
	if end <= 0 {
		t.Fatalf("G did not scroll toward the end; yamlScroll=%d", end)
	}
	// Already at the bottom: a further page-down can't move past it.
	if further := m.pageYAML(yamlPageSize(m)); further.yamlScroll != end {
		t.Errorf("G should land on the last page; ctrl+f advanced it from %d to %d", end, further.yamlScroll)
	}

	// Independent of pane focus: from the top with a side pane focused, G still
	// reaches the same bottom.
	m.focus = paneSessions
	m.yamlScroll, m.yamlCursor = 0, 0
	m = pressKey(m, "G")
	if m.yamlScroll != end {
		t.Errorf("G from a non-YAML focus scroll=%d, want %d", m.yamlScroll, end)
	}
}

func TestProjectYAMLScrollResetsWhenSelectionChanges(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 60})
	m = sized.(model)
	step, _ := m.Update(projectsMsg{views: []ProjectView{
		{Name: "alpha", Path: "/x/alpha/.project.yaml", Status: project.StatusReady},
		{Name: "bravo", Path: "/x/bravo/.project.yaml", Status: project.StatusReady},
	}})
	m = step.(model)
	m.yamlScroll = 5

	// Focus the projects pane, then advance the selection with j.
	m = focusProjectsPane(t, m)
	right, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = right.(model)

	if m.projSel != "/x/bravo/.project.yaml" {
		t.Fatalf("projSel = %q, want bravo", m.projSel)
	}
	if m.yamlScroll != 0 {
		t.Errorf("yamlScroll = %d, want 0 after project selection change", m.yamlScroll)
	}
}

func TestWrapDisplayWidthSplitsLongLinesAndPreservesBlanks(t *testing.T) {
	in := "short\n\nverylongline-of-text-that-must-wrap-cleanly"
	got := wrapDisplayWidth(in, 10)

	// First entry is "short" (≤ 10 cols, no wrap).
	if got[0] != "short" {
		t.Errorf("wrap[0] = %q, want %q", got[0], "short")
	}
	// Blank source line is preserved as a single empty entry.
	if got[1] != "" {
		t.Errorf("wrap[1] = %q, want empty (blank line preserved)", got[1])
	}
	// Remaining entries are wrapped fragments, each ≤ 10 cols.
	if len(got) < 4 {
		t.Fatalf("expected long line to wrap into >= 2 fragments, got %d total entries: %v", len(got), got)
	}
	for i, line := range got[2:] {
		if len(line) > 10 {
			t.Errorf("wrap[%d] = %q exceeds 10 cols", i+2, line)
		}
	}
	// Reassembled (skipping the blank), wrapped fragments must match the
	// original long line so no characters were dropped.
	if joined := strings.Join(got[2:], ""); joined != "verylongline-of-text-that-must-wrap-cleanly" {
		t.Errorf("wrapped fragments do not reassemble: %q", joined)
	}
}

const collapseSampleYAML = `description: |
    line one
    line two
    line three
    line four
    line five
branch: feat/x
status: reviewing
summary: |
    only one
repos:
    - org: docker
`

func TestCollapseYAMLBlocksFoldsLongBlocks(t *testing.T) {
	got := collapseYAMLBlocks(collapseSampleYAML, false)

	// The five-line description keeps yamlCollapseLines lines + a marker.
	if !strings.Contains(got, "line three") {
		t.Errorf("collapsed body dropped a kept line:\n%s", got)
	}
	if strings.Contains(got, "line four") || strings.Contains(got, "line five") {
		t.Errorf("collapsed body still shows hidden lines:\n%s", got)
	}
	if !strings.Contains(got, "… +2 more (tt to expand)") {
		t.Errorf("collapsed body missing the fold marker:\n%s", got)
	}
	// Fields after the collapsed block survive intact.
	for _, want := range []string{"branch: feat/x", "status: reviewing", "repos:", "- org: docker"} {
		if !strings.Contains(got, want) {
			t.Errorf("collapsed body dropped %q:\n%s", want, got)
		}
	}
	// A short block (summary: one line) is under the limit, so no marker and its
	// content stays.
	if !strings.Contains(got, "only one") {
		t.Errorf("short summary block was altered:\n%s", got)
	}
	if strings.Count(got, "to expand") != 1 {
		t.Errorf("only the long block should be folded; got %d markers:\n%s", strings.Count(got, "to expand"), got)
	}
}

func TestCollapseYAMLBlocksExpandedIsIdentity(t *testing.T) {
	if got := collapseYAMLBlocks(collapseSampleYAML, true); got != collapseSampleYAML {
		t.Errorf("expanded=true must return the body unchanged; got:\n%s", got)
	}
}

// TestTTChordTogglesYAMLExpansion pins the "tt" chord: it flips yamlExpanded
// (fold/unfold the description & summary blocks) regardless of pane focus, and
// the second "t" must not re-arm the chord or leave a stray "t" standalone
// action pending.
func TestTTChordTogglesYAMLExpansion(t *testing.T) {
	m := zoomTestModel()
	m.focus = paneProjects // works independent of focus

	m = pressKey(m, "t")
	if m.pendingKey != "t" {
		t.Fatalf("first t should arm the chord; pendingKey=%q", m.pendingKey)
	}
	m = pressKey(m, "t")
	if m.yamlExpanded != true {
		t.Errorf("tt should expand; yamlExpanded=false")
	}
	if m.pendingKey != "" {
		t.Errorf("tt must not leave a chord pending; pendingKey=%q", m.pendingKey)
	}
	if m.zoomed {
		t.Errorf("tt must not touch the maximize state")
	}

	// Toggle back.
	m = pressKey(m, "t")
	m = pressKey(m, "t")
	if m.yamlExpanded {
		t.Errorf("second tt should collapse again; yamlExpanded=true")
	}
}
