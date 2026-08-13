package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/slimslenderslacks/work/internal/policy"
	"github.com/slimslenderslacks/work/internal/project"
	"github.com/slimslenderslacks/work/internal/task"
)

func TestRenderProjectCardIncludesNameStatusAndBreakdown(t *testing.T) {
	v := ProjectView{
		Name:   "alpha",
		Branch: "feat/alpha",
		Status: project.StatusWorking,
		TaskCounts: map[task.Status]int{
			task.StatusReady:     2,
			task.StatusRunning:   1,
			task.StatusBlocked:   0,
			task.StatusSuccess:   1,
			task.StatusCommitted: 3,
			task.StatusFailed:    1,
		},
	}
	out := renderProjectCard(v, 32, false)
	if !strings.Contains(out, "alpha") {
		t.Errorf("card missing project name; got:\n%s", out)
	}
	if !strings.Contains(out, "working") {
		t.Errorf("card missing status; got:\n%s", out)
	}
	// Done == success + committed = 4.
	if !strings.Contains(out, "R:2") ||
		!strings.Contains(out, "W:1") ||
		!strings.Contains(out, "B:0") ||
		!strings.Contains(out, "D:4") ||
		!strings.Contains(out, "F:1") {
		t.Errorf("breakdown line missing or wrong; got:\n%s", out)
	}
}

func TestRenderProjectCardHandlesNoTasks(t *testing.T) {
	v := ProjectView{Name: "bare", Status: project.StatusReady}
	out := renderProjectCard(v, 32, false)
	if !strings.Contains(out, "no tasks") {
		t.Errorf("empty-task card should say 'no tasks'; got:\n%s", out)
	}
}

func TestRenderProjectGridReflowsByWidth(t *testing.T) {
	views := []ProjectView{
		{Name: "alpha", Status: project.StatusReady},
		{Name: "bravo", Status: project.StatusWorking},
		{Name: "charlie", Status: project.StatusBlocked},
		{Name: "delta", Status: project.StatusDone},
	}

	// Pass a large row budget so the grid renders every project; this test
	// only exercises the width-based reflow.
	wide := renderProjectGrid(views, "", 200, 100)
	narrow := renderProjectGrid(views, "", 30, 100)

	wideRows := strings.Count(wide, "\n")
	narrowRows := strings.Count(narrow, "\n")
	if narrowRows <= wideRows {
		t.Errorf("narrow grid should produce more rows than wide grid; wide=%d narrow=%d", wideRows, narrowRows)
	}

	// Every project must appear in both layouts.
	for _, v := range views {
		if !strings.Contains(wide, v.Name) {
			t.Errorf("wide grid missing %q", v.Name)
		}
		if !strings.Contains(narrow, v.Name) {
			t.Errorf("narrow grid missing %q", v.Name)
		}
	}

	// Sanity: the rendered grid should fit within the requested width.
	for _, line := range strings.Split(wide, "\n") {
		if lipgloss.Width(line) > 200 {
			t.Errorf("wide line exceeds requested width: %d", lipgloss.Width(line))
		}
	}
}

func TestRenderTasksShowsNamesAndStatus(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = sized.(model)
	step, _ := m.Update(projectsMsg{views: []ProjectView{
		{
			Name:   "alpha",
			Path:   "/x/alpha/.project.yaml",
			Status: project.StatusWorking,
			Tasks: []TaskView{
				{Name: "register-repo", Status: task.StatusCommitted},
				{Name: "survey", Status: task.StatusRunning},
				{Name: "implement", Status: task.StatusReady},
			},
		},
	}})
	m = step.(model)

	view := m.View()
	for _, want := range []string{"register-repo", "survey", "implement", "committed", "running", "ready"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
}

func TestRenderTasksTableShowsModelMCPsAndRules(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = sized.(model)
	step, _ := m.Update(projectsMsg{views: []ProjectView{
		{
			Name:   "alpha",
			Path:   "/x/alpha/.project.yaml",
			Status: project.StatusWorking,
			Tasks: []TaskView{
				{
					Name:       "fetch-readme",
					Model:      "default",
					StaticMCPs: []string{"github", "web-search"},
					Policies: []policy.Rule{
						{Action: policy.ActionDeny, Kind: policy.KindNetwork, Resource: "**"},
						{Action: policy.ActionAllow, Kind: policy.KindNetwork, Resource: "api.github.com"},
					},
					Status: task.StatusReady,
				},
				{
					Name:   "bare",
					Status: task.StatusReady,
				},
			},
		},
	}})
	m = step.(model)

	view := m.View()
	// Column header is rendered above the data rows.
	for _, want := range []string{"name", "model", "mcps", "rules", "status"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing column header %q:\n%s", want, view)
		}
	}
	// First row's cell values are all present.
	for _, want := range []string{"fetch-readme", "default", "github,web-search", "2"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing cell %q:\n%s", want, view)
		}
	}
	// Second row's mcps + rules show the empty-value placeholder.
	if !strings.Contains(view, "bare") {
		t.Errorf("view missing second task name 'bare':\n%s", view)
	}
	// "-" placeholders for empty mcps/rules — count occurrences loosely.
	if strings.Count(view, "-") < 2 {
		t.Errorf("view should carry placeholder '-' for empty mcps and rules:\n%s", view)
	}
}

func TestRenderTasksSwapsOnProjectSelection(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = sized.(model)
	step, _ := m.Update(projectsMsg{views: []ProjectView{
		{
			Name: "alpha", Path: "/x/alpha/.project.yaml",
			Status: project.StatusWorking,
			Tasks:  []TaskView{{Name: "alpha-only", Status: task.StatusReady}},
		},
		{
			Name: "bravo", Path: "/x/bravo/.project.yaml",
			Status: project.StatusWorking,
			Tasks:  []TaskView{{Name: "bravo-only", Status: task.StatusReady}},
		},
	}})
	m = step.(model)

	v1 := m.View()
	if !strings.Contains(v1, "alpha-only") || strings.Contains(v1, "bravo-only") {
		t.Errorf("initial view should show alpha-only and not bravo-only:\n%s", v1)
	}

	// Focus the projects pane, then advance the selection to bravo with j.
	m = focusProjectsPane(t, m)
	right, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = right.(model)

	v2 := m.View()
	if !strings.Contains(v2, "bravo-only") || strings.Contains(v2, "alpha-only") {
		t.Errorf("after selecting bravo, view should show bravo-only and not alpha-only:\n%s", v2)
	}
}

func TestRenderTasksEmptyState(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = sized.(model)
	step, _ := m.Update(projectsMsg{views: []ProjectView{
		{Name: "empty", Path: "/x/empty/.project.yaml", Status: project.StatusReady},
	}})
	m = step.(model)
	if !strings.Contains(m.View(), "(none)") {
		t.Errorf("empty tasks should show (none); got:\n%s", m.View())
	}
}

func TestProjectsPaneSpillsOverMultipleRows(t *testing.T) {
	// Eight projects at width=160 fits two cards per row (innerWidth ≈ 100,
	// cardTargetWidth=30 → perRow=3), so the gallery needs three rows to
	// show every card. With a generous height the layout must grow the
	// projects pane to fit them all instead of clipping to the historical
	// one-row default.
	views := make([]ProjectView, 0, 8)
	for _, name := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"} {
		views = append(views, ProjectView{
			Name:   name,
			Path:   "/x/" + name + "/.project.yaml",
			Status: project.StatusReady,
		})
	}

	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 80})
	m = sized.(model)
	step, _ := m.Update(projectsMsg{views: views})
	m = step.(model)

	view := m.View()
	for _, v := range views {
		if !strings.Contains(view, v.Name) {
			t.Errorf("expected every project to render; missing %q in view", v.Name)
		}
	}

	// Sanity: the projects pane must have grown beyond the one-card-row
	// minimum so the spill-over is actually layout-driven and not just the
	// renderer overrunning its allocation.
	l := m.computeLayout()
	if l.projectsH <= projectsMinHeight {
		t.Errorf("projectsH = %d, want > projectsMinHeight(%d) to confirm spill-over",
			l.projectsH, projectsMinHeight)
	}
}

func TestDesiredProjectsHeightGrowsWithCount(t *testing.T) {
	one := desiredProjectsHeight(80, 1)
	many := desiredProjectsHeight(80, 8)
	if one != projectsMinHeight {
		t.Errorf("one project: desiredProjectsHeight = %d, want %d", one, projectsMinHeight)
	}
	if many <= one {
		t.Errorf("eight projects (%d rows) should need more height than one (%d)", many, one)
	}
	// Empty + zero-width edge cases must return the floor without dividing
	// by zero or producing nonsense values.
	if got := desiredProjectsHeight(80, 0); got != projectsMinHeight {
		t.Errorf("zero projects: desiredProjectsHeight = %d, want %d", got, projectsMinHeight)
	}
	if got := desiredProjectsHeight(0, 5); got != projectsMinHeight {
		t.Errorf("zero width: desiredProjectsHeight = %d, want %d", got, projectsMinHeight)
	}
}

func TestSelectedCardBorderUsesAccentColor(t *testing.T) {
	// lipgloss strips colours in test environments (no TTY), so we can't
	// compare rendered output. Verify the style values directly via the
	// per-side getter (BorderForeground sets all four sides identically).
	plain := cardBorder.GetBorderTopForeground()
	sel := cardSelectedRing.GetBorderTopForeground()
	focused := focusedBorder.GetBorderTopForeground()
	if plain == sel {
		t.Errorf("cardSelectedRing must use a different colour than cardBorder; both = %v", plain)
	}
	if sel != focused {
		t.Errorf("cardSelectedRing should share the focus accent colour; got %v, want %v", sel, focused)
	}
}

// TestSelectionRingIsSizedLikeItsSpacer pins the invariant that makes the ring
// safe to add: a selected card and an unselected one occupy the same box, so
// moving the cursor can't reflow the gallery. Both are one border thick on
// every side, and cardDisplayRows accounts for the ring's two rows on top of
// the card's own five.
func TestSelectionRingIsSizedLikeItsSpacer(t *testing.T) {
	ring, spacer := projectCardRing(true), projectCardRing(false)
	if got, want := ring.GetHorizontalBorderSize(), spacer.GetHorizontalBorderSize(); got != want {
		t.Errorf("ring horizontal size = %d, spacer = %d; they must match or the grid reflows on selection", got, want)
	}
	if got, want := ring.GetVerticalBorderSize(), spacer.GetVerticalBorderSize(); got != want {
		t.Errorf("ring vertical size = %d, spacer = %d; they must match or the grid reflows on selection", got, want)
	}
	cardRows := cardBorder.GetVerticalBorderSize() + 3 // name + status + breakdown
	if want := cardRows + ring.GetVerticalBorderSize(); cardDisplayRows != want {
		t.Errorf("cardDisplayRows = %d, want %d (card + ring)", cardDisplayRows, want)
	}
}

// TestSelectedCardRendersBothBorders is the point of the ring: the state colour
// has to survive selection. Colours are stripped without a TTY, so assert on
// geometry instead — the selected card gains a border ring while staying the
// same width, which is what leaves the inner border visible.
func TestSelectedCardRendersBothBorders(t *testing.T) {
	v := ProjectView{Name: "alpha", Status: project.StatusDone, Archive: true}
	const width = cardTargetWidth

	sel := strings.Split(renderProjectCard(v, width, true), "\n")
	unsel := strings.Split(renderProjectCard(v, width, false), "\n")

	if len(sel) != cardDisplayRows || len(unsel) != cardDisplayRows {
		t.Fatalf("card heights = %d selected / %d unselected, want %d for both",
			len(sel), len(unsel), cardDisplayRows)
	}
	for i, line := range sel {
		if got := lipgloss.Width(line); got != width {
			t.Errorf("selected line %d width = %d, want %d", i, got, width)
		}
	}

	// Two nested rounded borders: the ring's top-left corner opens row 0, and
	// the card's own corner sits on row 1 just inside the ring's left edge.
	rounded := lipgloss.RoundedBorder()
	if !strings.HasPrefix(sel[0], rounded.TopLeft) {
		t.Errorf("selected row 0 = %q, want the ring's %q corner", sel[0], rounded.TopLeft)
	}
	if want := rounded.Left + rounded.TopLeft; !strings.HasPrefix(sel[1], want) {
		t.Errorf("selected row 1 = %q, want %q — the card's corner inside the ring", sel[1], want)
	}
	// Unselected: the ring's rows and columns are reserved but blank, so the
	// card's corner lands in the same place with nothing drawn around it.
	if strings.TrimSpace(unsel[0]) != "" {
		t.Errorf("unselected row 0 = %q, want blank reserved space", unsel[0])
	}
	if want := " " + rounded.TopLeft; !strings.HasPrefix(unsel[1], want) {
		t.Errorf("unselected row 1 = %q, want %q", unsel[1], want)
	}
}

func TestArchivedCardBorderIsBlueAndDistinct(t *testing.T) {
	// Same constraint as TestSelectedCardBorderUsesAccentColor: colours are
	// stripped in a non-TTY test run, so inspect the style values directly.
	archived := cardArchivedBorder.GetBorderTopForeground()
	plain := cardBorder.GetBorderTopForeground()
	sel := cardSelectedRing.GetBorderTopForeground()
	if archived == plain {
		t.Errorf("cardArchivedBorder must differ from cardBorder; both = %v", plain)
	}
	// The ring sits directly against this border on a selected card, so the two
	// colours have to be told apart side by side.
	if archived == sel {
		t.Errorf("cardArchivedBorder must differ from cardSelectedRing; both = %v", sel)
	}
	// Blue comes from the same 256-colour family as statusReady.
	if want := statusReady.GetForeground(); archived != want {
		t.Errorf("cardArchivedBorder colour = %v, want the blue %v", archived, want)
	}
	// The archived card must keep the same border geometry as the others, or
	// the width arithmetic in renderProjectCard fragments the card.
	if got, want := cardArchivedBorder.GetHorizontalBorderSize(), cardBorder.GetHorizontalBorderSize(); got != want {
		t.Errorf("cardArchivedBorder horizontal border size = %d, want %d", got, want)
	}
}

func TestCronActiveCardBorderIsGreenAndDistinct(t *testing.T) {
	// Mirrors TestArchivedCardBorderIsBlueAndDistinct: colours are stripped in
	// a non-TTY test run, so inspect the style values directly.
	cronActive := cardCronActiveBorder.GetBorderTopForeground()
	plain := cardBorder.GetBorderTopForeground()
	sel := cardSelectedRing.GetBorderTopForeground()
	archived := cardArchivedBorder.GetBorderTopForeground()
	if cronActive == plain {
		t.Errorf("cardCronActiveBorder must differ from cardBorder; both = %v", plain)
	}
	// As with the archive blue: the ring is drawn immediately outside this
	// border when the card is selected.
	if cronActive == sel {
		t.Errorf("cardCronActiveBorder must differ from cardSelectedRing; both = %v", sel)
	}
	if cronActive == archived {
		t.Errorf("cardCronActiveBorder must differ from cardArchivedBorder; both = %v", archived)
	}
	// Green comes from the same 256-colour family as statusRunning.
	if want := statusRunning.GetForeground(); cronActive != want {
		t.Errorf("cardCronActiveBorder colour = %v, want the green %v", cronActive, want)
	}
	// Same border geometry as the others, or the width arithmetic in
	// renderProjectCard fragments the card.
	if got, want := cardCronActiveBorder.GetHorizontalBorderSize(), cardBorder.GetHorizontalBorderSize(); got != want {
		t.Errorf("cardCronActiveBorder horizontal border size = %d, want %d", got, want)
	}
}

// TestProjectCardBorderPrefersArchiveOverCron covers the card's own border,
// which now encodes durable state only: archive beats cron-active, and
// selection doesn't enter into it — the ring carries that instead, so a
// selected card keeps showing what it is.
func TestProjectCardBorderPrefersArchiveOverCron(t *testing.T) {
	archived := ProjectView{Name: "alpha", Status: project.StatusDone, Archive: true}
	normal := ProjectView{Name: "bravo", Status: project.StatusWorking}
	cronActive := ProjectView{Name: "charlie", Status: project.StatusWorking, CronActive: true}
	both := ProjectView{Name: "delta", Status: project.StatusDone, Archive: true, CronActive: true}

	cases := []struct {
		name string
		view ProjectView
		want lipgloss.Style
	}{
		{"archived", archived, cardArchivedBorder},
		{"normal", normal, cardBorder},
		{"cron-active", cronActive, cardCronActiveBorder},
		{"archived and cron-active", both, cardArchivedBorder},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := projectCardBorder(tc.view).GetBorderTopForeground()
			if want := tc.want.GetBorderTopForeground(); got != want {
				t.Errorf("border colour = %v, want %v", got, want)
			}
		})
	}
}

func TestRenderArchivedProjectCardStaysIntact(t *testing.T) {
	// The archived style must not change the card's shape: same line count and
	// same width as a normal card, so a blue card doesn't fragment the grid.
	archived := renderProjectCard(ProjectView{Name: "alpha", Status: project.StatusDone, Archive: true}, 32, false)
	plain := renderProjectCard(ProjectView{Name: "alpha", Status: project.StatusDone}, 32, false)
	for _, line := range strings.Split(archived, "\n") {
		if got := lipgloss.Width(line); got != 32 {
			t.Errorf("archived card line width = %d, want 32; line=%q", got, line)
		}
	}
	if got, want := strings.Count(archived, "\n"), strings.Count(plain, "\n"); got != want {
		t.Errorf("archived card has %d line breaks, want %d", got, want)
	}
}

// TestOptionGlyphSwitchesPane covers the macOS Option-key fallback: a default
// macOS terminal emits "∆"/"˚" for ⌥j/⌥k (no Meta), so those glyphs must
// switch panes just like "alt+j"/"alt+k".
func TestOptionGlyphSwitchesPane(t *testing.T) {
	m := newModel(nil, make(<-chan []SessionView), nil, &fakeAttacher{})
	start := m.focus
	// "˚" (⌥k) advances focus like alt+k.
	step, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'˚'}})
	m = step.(model)
	if m.focus == start {
		t.Fatalf("⌥k glyph did not switch pane: focus still %v", m.focus)
	}
	// "∆" (⌥j) moves back to where we started.
	step, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'∆'}})
	m = step.(model)
	if m.focus != start {
		t.Errorf("⌥j glyph should invert ⌥k: focus = %v, want %v", m.focus, start)
	}
}
