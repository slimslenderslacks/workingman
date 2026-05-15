// Package tui hosts the orch terminal UI. The layout is a two-pane skeleton:
// a left column listing agent sessions and a main panel showing a gallery of
// project cards. Both panes reflect live state — projects from the
// .project.yaml files scanned by WatchProjects, sessions from a channel the
// daemon feeds in via its WatchSessions adapter.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/slimslenderslacks/work/internal/task"
)

type pane int

const (
	paneSessions pane = iota
	paneProjects
)

type projectsMsg struct {
	views []ProjectView
}

type sessionsMsg struct {
	views []SessionView
}

type auditMsg struct {
	lines []string
}

type model struct {
	width      int
	height     int
	focus      pane
	sessions   []SessionView
	sessCh     <-chan []SessionView
	sessLoaded bool
	sessSel    string
	projects   []ProjectView
	projCh        <-chan []ProjectView
	projSel       string // path of the selected project card; empty when unset
	loaded        bool
	attacher      tmuxAttacher
	statusMsg     string

	auditLines []string
	auditCh    <-chan []string
}

func newModel(projCh <-chan []ProjectView, sessCh <-chan []SessionView, auditCh <-chan []string, attacher tmuxAttacher) model {
	// When no sessions source is wired in (standalone tui mode), short-circuit
	// to the empty state so the pane shows "(none)" instead of an endless
	// "(loading…)".
	return model{
		focus:      paneSessions,
		projCh:     projCh,
		sessCh:     sessCh,
		auditCh:    auditCh,
		sessLoaded: sessCh == nil,
		attacher:   attacher,
	}
}

func (m model) Init() tea.Cmd {
	var cmds []tea.Cmd
	if m.projCh != nil {
		cmds = append(cmds, waitForProjects(m.projCh))
	}
	if m.sessCh != nil {
		cmds = append(cmds, waitForSessions(m.sessCh))
	}
	if m.auditCh != nil {
		cmds = append(cmds, waitForAudit(m.auditCh))
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

func waitForProjects(ch <-chan []ProjectView) tea.Cmd {
	return func() tea.Msg {
		views, ok := <-ch
		if !ok {
			return nil
		}
		return projectsMsg{views: views}
	}
}

func waitForSessions(ch <-chan []SessionView) tea.Cmd {
	return func() tea.Msg {
		views, ok := <-ch
		if !ok {
			return nil
		}
		return sessionsMsg{views: views}
	}
}

func waitForAudit(ch <-chan []string) tea.Cmd {
	return func() tea.Msg {
		lines, ok := <-ch
		if !ok {
			return nil
		}
		return auditMsg{lines: lines}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case projectsMsg:
		m.projects = msg.views
		m.loaded = true
		m.projSel = reconcileProjectSelection(m.projects, m.projSel)
		m.sessSel = reconcileSelection(m.visibleSessions(), m.sessSel)
		return m, waitForProjects(m.projCh)
	case sessionsMsg:
		m.sessions = msg.views
		m.sessLoaded = true
		m.sessSel = reconcileSelection(m.visibleSessions(), m.sessSel)
		return m, waitForSessions(m.sessCh)
	case auditMsg:
		m.auditLines = msg.lines
		return m, waitForAudit(m.auditCh)
	case attachResultMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("attach %s: %v", msg.target, msg.err)
		} else {
			m.statusMsg = ""
		}
		return m, nil
	case tea.MouseMsg:
		return m.handleMouse(msg)
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "tab", "shift+tab":
			m.focus = togglePane(m.focus)
			m.statusMsg = ""
		case "right", "l":
			m.focus = paneProjects
			m.statusMsg = ""
		case "left", "h":
			m.focus = paneSessions
			m.statusMsg = ""
		case "up":
			switch m.focus {
			case paneSessions:
				m.sessSel = moveSelection(m.visibleSessions(), m.sessSel, -1)
			case paneProjects:
				m.projSel = moveProjectSelection(m.projects, m.projSel, -1)
				m.sessSel = reconcileSelection(m.visibleSessions(), m.sessSel)
			}
			m.statusMsg = ""
		case "down":
			switch m.focus {
			case paneSessions:
				m.sessSel = moveSelection(m.visibleSessions(), m.sessSel, 1)
			case paneProjects:
				m.projSel = moveProjectSelection(m.projects, m.projSel, 1)
				m.sessSel = reconcileSelection(m.visibleSessions(), m.sessSel)
			}
			m.statusMsg = ""
		case "enter":
			if m.focus == paneSessions {
				return m.attachSelected()
			}
		}
	}
	return m, nil
}

// handleMouse routes a mouse event to its pane.
//
//   - Sessions pane (left column): left-button press selects the row under
//     the cursor and immediately attaches to it (click-to-attach UX).
//   - Projects pane (right column, upper area): left-button press focuses
//     the pane and selects the card under the cursor. No attach.
//   - Tasks pane (right column, lower area): focus only; tasks are display
//     today.
func (m model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}
	if m.width <= 0 || msg.X < 0 || msg.Y < 0 {
		return m, nil
	}
	l := m.computeLayout()

	// Sessions pane: clicks in the left column.
	if msg.X < l.sessionsW {
		idx := sessionRowAtY(msg.Y, len(m.visibleSessions()))
		if idx < 0 {
			m.focus = paneSessions
			return m, nil
		}
		m.focus = paneSessions
		m.sessSel = m.visibleSessions()[idx].ID
		m.statusMsg = ""
		return m.attachSelected()
	}

	// Right column: split between projects (top) and tasks (bottom). Header
	// is row 0; projects pane occupies rows [1, 1+projectsH); tasks below.
	const headerRows = 1
	if msg.Y >= headerRows && msg.Y < headerRows+l.projectsH {
		innerW := l.projectsW - unfocusedBorder.GetHorizontalFrameSize()
		if innerW < 0 {
			innerW = 0
		}
		idx := projectCardAtPoint(msg.X-l.sessionsW, msg.Y, innerW, len(m.projects))
		m.focus = paneProjects
		m.statusMsg = ""
		if idx < 0 || idx >= len(m.projects) {
			return m, nil
		}
		m.projSel = m.projects[idx].Path
		m.sessSel = reconcileSelection(m.visibleSessions(), m.sessSel)
		return m, nil
	}
	// Click below the projects pane → tasks area (or audit). Treat as
	// focus-only on projects so up/down keys keep navigating the cards.
	m.focus = paneProjects
	return m, nil
}

// attachSelected dispatches the tmux-attach command for the currently
// selected session row. The actual suspend/resume is owned by bubbletea via
// tea.ExecProcess; this model just returns the Cmd.
func (m model) attachSelected() (tea.Model, tea.Cmd) {
	if m.attacher == nil {
		m.statusMsg = "tmux attach disabled (no attacher wired)"
		return m, nil
	}
	target, ok := selectedTmuxTarget(m.visibleSessions(), m.sessSel)
	if !ok {
		m.statusMsg = "no session selected"
		return m, nil
	}
	m.statusMsg = ""
	return m, m.attacher.Attach(target)
}

func selectedTmuxTarget(views []SessionView, id string) (string, bool) {
	for _, v := range views {
		if v.ID == id {
			return v.TmuxTarget, true
		}
	}
	return "", false
}

// visibleSessions returns the slice of sessions the pane should display,
// filtered by the currently-selected project. When no project is selected
// (e.g. there are no projects yet), all sessions are returned so the user
// still sees activity. When a project is selected but no session matches,
// an empty slice is returned and the pane renders "(none)".
//
// Filtering by Project (a short label like "my-feature") rather than by
// path keeps the daemon's SessionInfo decoupled from the project file's
// absolute path on disk.
func (m model) visibleSessions() []SessionView {
	if m.projSel == "" || len(m.projects) == 0 {
		return m.sessions
	}
	var selName string
	for _, p := range m.projects {
		if p.Path == m.projSel {
			selName = p.Name
			break
		}
	}
	if selName == "" {
		return m.sessions
	}
	out := make([]SessionView, 0, len(m.sessions))
	for _, s := range m.sessions {
		if s.Project == selName {
			out = append(out, s)
		}
	}
	return out
}

// projectCardAtPoint maps a click inside the projects pane to a card index.
// The arithmetic mirrors renderProjectGrid: cards are cardWidth wide with a
// cardGap-column gap between them and three rows tall (top border, content,
// bottom border) — same shape regardless of how full each card's content is.
//
// xRel and yRel are coordinates relative to the inner edge of the projects
// pane (i.e. after subtracting m.sessionsWidth from msg.X). innerWidth is
// the pane's inner content width (after borders).
//
// Returns -1 when the click lands in a gap, in the title/blank rows above
// the cards, below the last row, or beyond the last card in its row.
func projectCardAtPoint(xRel, yRel, innerWidth, count int) int {
	if count <= 0 || innerWidth <= 0 || xRel < 0 || yRel < 0 {
		return -1
	}
	const (
		headerLines    = 1 // outer "orch" title
		paneTopBorder  = 1
		paneTitleLines = 2 // "Projects" + blank line
		paneLeftBorder = 1
		cardRows       = 3 // top border + body + bottom border (project card body always renders 1 row tall in our layout)
	)
	cardWidth := cardTargetWidth
	if cardWidth > innerWidth {
		cardWidth = innerWidth
	}
	if cardWidth < cardMinWidth {
		cardWidth = innerWidth
	}
	perRow := (innerWidth + cardGap) / (cardWidth + cardGap)
	if perRow < 1 {
		perRow = 1
	}

	// Vertical: skip the outer header, the projects pane's top border, and
	// the title + blank rows; each card occupies cardRows.
	yIn := yRel - (headerLines + paneTopBorder + paneTitleLines)
	if yIn < 0 {
		return -1
	}
	row := yIn / cardRows
	totalRows := (count + perRow - 1) / perRow
	if row >= totalRows {
		return -1
	}

	// Horizontal: skip the pane's left border, then each card occupies
	// cardWidth columns with cardGap columns of gap after.
	xIn := xRel - paneLeftBorder
	if xIn < 0 {
		return -1
	}
	col := -1
	for c := 0; c < perRow; c++ {
		start := c * (cardWidth + cardGap)
		end := start + cardWidth
		if xIn >= start && xIn < end {
			col = c
			break
		}
	}
	if col < 0 {
		return -1 // landed in a gap between cards
	}

	idx := row*perRow + col
	if idx >= count {
		return -1 // last row may have fewer cards than perRow
	}
	return idx
}

// sessionRowAtY maps an absolute y row to a session index using the same
// layout constants View() renders with: header(1) + pane top border(1) +
// "Agent Sessions" title + blank(2) + per-session triplet of (head, status,
// separator). The separator row is intentionally a dead zone so clicking
// between rows doesn't pick the wrong session.
func sessionRowAtY(y, count int) int {
	const (
		headerLines    = 1
		paneTopBorder  = 1
		paneTitleLines = 2
		rowsPerSession = 3
	)
	rel := y - (headerLines + paneTopBorder + paneTitleLines)
	if rel < 0 {
		return -1
	}
	idx := rel / rowsPerSession
	if rel%rowsPerSession == 2 {
		return -1
	}
	if idx < 0 || idx >= count {
		return -1
	}
	return idx
}

// paneWidths reproduces View()'s sessions/projects split so handleMouse can
// decide whether a click landed in the sessions pane without re-rendering.
// Keep this in lockstep with the width clamps used at the top of View().
func paneWidths(width int) (sessionsWidth, projectsWidth int) {
	if width <= 0 {
		return 0, 0
	}
	sessionsWidth = width / 3
	if sessionsWidth < 20 {
		sessionsWidth = 20
	}
	if sessionsWidth > 40 {
		sessionsWidth = 40
	}
	if sessionsWidth > width-10 {
		sessionsWidth = width - 10
		if sessionsWidth < 10 {
			sessionsWidth = width
		}
	}
	projectsWidth = width - sessionsWidth
	if projectsWidth < 1 {
		projectsWidth = 1
	}
	return sessionsWidth, projectsWidth
}

func togglePane(p pane) pane {
	if p == paneSessions {
		return paneProjects
	}
	return paneSessions
}

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("212"))
	hintStyle = lipgloss.NewStyle().
			Faint(true)
	paneTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("110"))
	focusedBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("212")).
			Padding(0, 1)
	unfocusedBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)
	statusReady   = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	statusWorking = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	statusBlocked = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	statusDone    = lipgloss.NewStyle().Foreground(lipgloss.Color("46"))
	statusRunning = lipgloss.NewStyle().Foreground(lipgloss.Color("82"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	cardNameStyle = lipgloss.NewStyle().Bold(true)
	cardBorder    = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)
	// cardSelectedBorder highlights the active project card. Uses the same
	// accent colour as focusedBorder so the eye learns one signal for
	// "active thing".
	cardSelectedBorder = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("212")).
				Padding(0, 1)
	sessionRowSelectedStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("212")).
				Background(lipgloss.Color("236"))
	sessionRowSelectedStatusStyle = lipgloss.NewStyle().
					Background(lipgloss.Color("236"))
	// sessionRowInteractiveStyle marks a row whose agent kind waits for a
	// human (project / wolf). Yellow is chosen so it doesn't collide with
	// the selection-pink or any of the status colours, and so it reads as
	// "your attention is needed" at a glance.
	sessionRowInteractiveStyle = lipgloss.NewStyle().
					Bold(true).
					Foreground(lipgloss.Color("220"))
	statusErrStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196"))
)

// Marker glyphs for the sessions pane. The selected row gets a filled marker;
// other rows get whitespace of the same width so the agent-name column stays
// aligned regardless of which row is selected.
const (
	sessionMarkerSelected = "▶ "
	sessionMarkerIdle     = "  "
	// interactiveBadge is appended to the agent name on rows whose kind
	// waits for a human. Single-column glyph so the alignment of
	// surrounding columns stays consistent.
	interactiveBadge = " ◆"
)

// Card sizing. Width is a target; the layout falls back to a single-column
// stack when the projects pane is too narrow to fit a card at this size.
const (
	cardTargetWidth = 30
	cardMinWidth    = 20
	cardGap         = 1
)

func (m model) borderStyle(p pane) lipgloss.Style {
	if m.focus == p {
		return focusedBorder
	}
	return unfocusedBorder
}

func renderStatus(s string) string {
	switch s {
	case "ready":
		return statusReady.Render(s)
	case "working":
		return statusWorking.Render(s)
	case "blocked":
		return statusBlocked.Render(s)
	case "done":
		return statusDone.Render(s)
	default:
		return s
	}
}

func (m model) renderSessions(width, height int) string {
	// lipgloss.Height(N) sets *content* height; borders add another 2 rows.
	// Caller passes the total rows we want the pane to occupy, so we have
	// to subtract the frame size before handing it to Height().
	base := m.borderStyle(paneSessions).Width(width)
	innerHeight := height - base.GetVerticalFrameSize()
	if innerHeight < 0 {
		innerHeight = 0
	}
	style := base.Height(innerHeight)
	innerWidth := width - style.GetHorizontalFrameSize()
	if innerWidth < 0 {
		innerWidth = 0
	}

	var b strings.Builder
	b.WriteString(paneTitleStyle.Render("Agent Sessions"))
	b.WriteString("\n\n")
	if !m.sessLoaded {
		b.WriteString(dimStyle.Render("(loading…)"))
		return style.Render(b.String())
	}
	visible := m.visibleSessions()
	if len(visible) == 0 {
		b.WriteString(dimStyle.Render("(none)"))
		return style.Render(b.String())
	}
	for i, s := range visible {
		b.WriteString(renderSessionRow(s, s.ID == m.sessSel, innerWidth))
		if i < len(visible)-1 {
			b.WriteString("\n")
		}
	}
	return style.Render(b.String())
}

// renderSessionRow draws one session as a two-line block: the headline carries
// the agent kind, an interactive badge (when the kind waits for a human),
// and the project; the second line carries a colored status indicator and
// (for task/commit agents) the task name. Truncation happens on the raw
// text before any style is applied so the byte-slice in truncate never
// tears an ANSI escape.
//
// Three row styles, in precedence order:
//   - selected: bold pink on dark background, both lines highlighted.
//   - interactive (not selected): bold yellow on the headline so the row
//     stands out even when another pane is focused.
//   - autonomous (not selected): plain headline, status-coloured second
//     line.
func renderSessionRow(s SessionView, selected bool, width int) string {
	marker := sessionMarkerIdle
	if selected {
		marker = sessionMarkerSelected
	}
	agentName := s.AgentName
	if s.Interactive {
		agentName += interactiveBadge
	}
	head := marker + agentName
	if s.Project != "" {
		head += "  " + s.Project
	}
	head = padToWidth(truncate(head, width), width)

	statusLine := "  " + sessionStatusGlyph(s.Status) + " " + s.Status
	if s.TaskName != "" {
		statusLine += "  " + s.TaskName
	}
	statusText := padToWidth(truncate(statusLine, width), width)

	switch {
	case selected:
		head = sessionRowSelectedStyle.Render(head)
		statusText = sessionRowSelectedStatusStyle.Inherit(sessionStatusStyle(s.Status)).Render(statusText)
	case s.Interactive:
		head = sessionRowInteractiveStyle.Render(head)
		statusText = sessionStatusStyle(s.Status).Render(statusText)
	default:
		statusText = sessionStatusStyle(s.Status).Render(statusText)
	}
	return head + "\n" + statusText
}

// padToWidth right-pads s with spaces so it reaches width display columns.
// Used so background-coloured selection rows extend across the whole inner
// pane width — without this the highlight would stop at the last printable
// character.
func padToWidth(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

// sessionStatusGlyph returns a compact symbol that pairs with the status
// label. It mirrors the colored dots used by terminal status lines so the
// pane reads at a glance.
func sessionStatusGlyph(status string) string {
	switch status {
	case "running":
		return "●"
	default:
		return "○"
	}
}

func sessionStatusStyle(status string) lipgloss.Style {
	switch status {
	case "running":
		return statusRunning
	default:
		return dimStyle
	}
}

func (m model) renderProjects(width, height int) string {
	base := m.borderStyle(paneProjects).Width(width)
	innerHeight := height - base.GetVerticalFrameSize()
	if innerHeight < 0 {
		innerHeight = 0
	}
	style := base.Height(innerHeight)
	innerWidth := width - style.GetHorizontalFrameSize()
	if innerWidth < 0 {
		innerWidth = 0
	}

	var b strings.Builder
	b.WriteString(paneTitleStyle.Render("Projects"))
	b.WriteString("\n\n")
	if !m.loaded {
		b.WriteString(dimStyle.Render("(loading…)"))
		return style.Render(b.String())
	}
	if len(m.projects) == 0 {
		b.WriteString(dimStyle.Render("(none)"))
		return style.Render(b.String())
	}

	b.WriteString(renderProjectGrid(m.projects, m.projSel, innerWidth))
	return style.Render(b.String())
}

// renderProjectGrid lays project cards out left-to-right, wrapping to a new
// row whenever the next card wouldn't fit in innerWidth. Cards shrink to the
// available width when the pane can't hold one at the target width. The card
// matching selPath gets the highlighted-border treatment.
func renderProjectGrid(views []ProjectView, selPath string, innerWidth int) string {
	if innerWidth <= 0 || len(views) == 0 {
		return ""
	}

	cardWidth := cardTargetWidth
	if cardWidth > innerWidth {
		cardWidth = innerWidth
	}
	if cardWidth < cardMinWidth {
		cardWidth = innerWidth
	}

	perRow := (innerWidth + cardGap) / (cardWidth + cardGap)
	if perRow < 1 {
		perRow = 1
	}

	var rows []string
	for i := 0; i < len(views); i += perRow {
		end := i + perRow
		if end > len(views) {
			end = len(views)
		}
		cards := make([]string, 0, end-i)
		for j := i; j < end; j++ {
			cards = append(cards, renderProjectCard(views[j], cardWidth, views[j].Path == selPath))
		}
		if cardGap > 0 && len(cards) > 1 {
			joined := make([]string, 0, len(cards)*2-1)
			gap := strings.Repeat(" ", cardGap)
			for k, c := range cards {
				if k > 0 {
					joined = append(joined, gap)
				}
				joined = append(joined, c)
			}
			rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, joined...))
		} else {
			rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, cards...))
		}
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func renderProjectCard(v ProjectView, width int, selected bool) string {
	border := cardBorder
	if selected {
		border = cardSelectedBorder
	}
	style := border.Width(width)
	inner := width - style.GetHorizontalFrameSize()
	if inner < 1 {
		inner = 1
	}

	name := cardNameStyle.Render(truncate(v.Name, inner))
	statusLine := renderStatus(string(v.Status))
	if v.Branch != "" {
		branchTxt := dimStyle.Render(v.Branch)
		statusLine = truncate(statusLine+"  "+branchTxt, inner)
	}
	breakdown := renderTaskBreakdown(v.TaskCounts, inner)

	body := name + "\n" + statusLine + "\n" + breakdown
	return style.Render(body)
}

// renderTaskBreakdown renders the per-state task counts in a compact form.
// Done counts both committed and success tasks — from the user's vantage the
// task is "done" once it has produced a successful run, even if the commit
// agent hasn't flipped it to committed yet.
func renderTaskBreakdown(counts map[task.Status]int, width int) string {
	if len(counts) == 0 {
		return dimStyle.Render(truncate("no tasks", width))
	}
	ready := counts[task.StatusReady]
	running := counts[task.StatusRunning]
	blocked := counts[task.StatusBlocked]
	done := counts[task.StatusSuccess] + counts[task.StatusCommitted]
	failed := counts[task.StatusFailed]
	line := fmt.Sprintf("R:%d W:%d B:%d D:%d F:%d",
		ready, running, blocked, done, failed)
	return dimStyle.Render(truncate(line, width))
}

// truncate clips s to width display columns. Width 0 returns empty.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	if width <= 1 {
		return "…"
	}
	for i := len(s); i > 0; i-- {
		candidate := s[:i] + "…"
		if lipgloss.Width(candidate) <= width {
			return candidate
		}
	}
	return "…"
}

// auditPaneHeight is how many rows the audit log pane occupies (including
// borders + title). Sized so ~6 lines of log fit comfortably without
// crowding the body panes above.
const auditPaneHeight = 10

// projectsMinHeight is the floor for the projects pane when the body height
// is large enough to split. Two rows of cards fit comfortably at this size.
const projectsMinHeight = 9

// uiLayout caches the computed dimensions of every pane for a given window
// size. View() and handleMouse() both use it so the rendering and the
// click-routing math stay in lockstep.
type uiLayout struct {
	sessionsW, projectsW       int
	bodyH                      int
	projectsH, tasksH, auditH  int
}

func (m model) computeLayout() uiLayout {
	sessW, projW := paneWidths(m.width)
	audit := 0
	if m.auditCh != nil {
		audit = auditPaneHeight
		if audit > m.height/3 {
			audit = m.height / 3
		}
		if audit < 5 {
			audit = 5
		}
	}
	bodyH := m.height - 1 /*header*/ - 1 /*footer*/ - audit - 1 /*buffer*/
	if bodyH < 6 {
		bodyH = 6
	}
	// Split the right column: projects gets the smaller half so the tasks
	// list, which can be long, takes the remainder. Clamp to a sensible
	// minimum for the projects pane.
	projH := bodyH / 3
	if projH < projectsMinHeight {
		projH = projectsMinHeight
	}
	if projH > bodyH-projectsMinHeight {
		projH = bodyH - projectsMinHeight
	}
	if projH < 3 {
		projH = 3
	}
	tasksH := bodyH - projH
	if tasksH < 3 {
		tasksH = 3
	}
	return uiLayout{sessW, projW, bodyH, projH, tasksH, audit}
}

func (m model) View() string {
	if m.width == 0 || m.height == 0 {
		return "starting orch tui…"
	}

	l := m.computeLayout()
	header := titleStyle.Render("orch")
	footer := m.renderFooter()

	left := m.renderSessions(l.sessionsW, l.bodyH)

	projects := m.renderProjects(l.projectsW, l.projectsH)
	tasks := m.renderTasks(l.projectsW, l.tasksH)
	right := lipgloss.JoinVertical(lipgloss.Left, projects, tasks)

	body := lipgloss.JoinHorizontal(lipgloss.Top, left, right)

	if l.auditH > 0 {
		audit := m.renderAudit(m.width, l.auditH)
		return lipgloss.JoinVertical(lipgloss.Left, header, body, audit, footer)
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

// renderTasks draws the per-project task list pane that sits below the
// projects gallery. Each task takes one row: name on the left, colored
// status on the right. The list is bound to whichever project projSel
// currently points at; switching project swaps the content.
//
// `height` is the total rows the pane should occupy. Borders eat 2 of them.
func (m model) renderTasks(width, height int) string {
	base := unfocusedBorder.Width(width)
	innerHeight := height - base.GetVerticalFrameSize()
	if innerHeight < 0 {
		innerHeight = 0
	}
	style := base.Height(innerHeight)
	innerWidth := width - style.GetHorizontalFrameSize()
	if innerWidth < 0 {
		innerWidth = 0
	}

	var b strings.Builder
	b.WriteString(paneTitleStyle.Render("Tasks"))
	b.WriteString("\n\n")

	tasks := m.selectedProjectTasks()
	if len(tasks) == 0 {
		b.WriteString(dimStyle.Render("(none)"))
		return style.Render(b.String())
	}

	maxRows := innerHeight - 2 // title + blank
	if maxRows < 0 {
		maxRows = 0
	}
	if len(tasks) > maxRows {
		tasks = tasks[:maxRows]
	}
	for i, t := range tasks {
		b.WriteString(renderTaskRow(t, innerWidth))
		if i < len(tasks)-1 {
			b.WriteString("\n")
		}
	}
	return style.Render(b.String())
}

// selectedProjectTasks returns the task slice for the currently-selected
// project, or nil if no project is selected or the selection no longer
// exists in the projects snapshot.
func (m model) selectedProjectTasks() []TaskView {
	if m.projSel == "" || len(m.projects) == 0 {
		return nil
	}
	for _, p := range m.projects {
		if p.Path == m.projSel {
			return p.Tasks
		}
	}
	return nil
}

// renderTaskRow lays one task out as "name … status", right-aligning the
// status to the inner width so the colored statuses form a tidy column. The
// task status colour matches the project-status palette so the eye learns
// one mapping across the UI.
func renderTaskRow(t TaskView, width int) string {
	status := string(t.Status)
	statusW := lipgloss.Width(status)
	nameW := width - statusW - 1 // 1 col gap
	if nameW < 1 {
		nameW = 1
	}
	name := truncate(t.Name, nameW)
	pad := width - lipgloss.Width(name) - statusW
	if pad < 1 {
		pad = 1
	}
	statusStyled := taskStatusStyle(t.Status).Render(status)
	return name + strings.Repeat(" ", pad) + statusStyled
}

// taskStatusStyle picks the same colour palette renderStatus uses for
// project status, plus a couple of task-specific cases. Falls back to dim
// for unknown values.
func taskStatusStyle(s task.Status) lipgloss.Style {
	switch s {
	case task.StatusReady:
		return statusReady
	case task.StatusRunning:
		return statusRunning
	case task.StatusSuccess, task.StatusCommitted:
		return statusDone
	case task.StatusBlocked:
		return statusBlocked
	case task.StatusFailed:
		return statusBlocked
	}
	return dimStyle
}

// renderAudit draws the audit log tail pane that lives below the body panes.
// Lines are shown newest-last (matching `tail -f` semantics) so the eye
// naturally lands on the most recent event at the bottom of the pane.
//
// `height` is the total rows the pane should occupy. lipgloss.Height sets
// content height (borders add 2), so we subtract the frame size before
// handing it to .Height().
func (m model) renderAudit(width, height int) string {
	base := unfocusedBorder.Width(width)
	innerHeight := height - base.GetVerticalFrameSize()
	if innerHeight < 0 {
		innerHeight = 0
	}
	style := base.Height(innerHeight)
	innerWidth := width - style.GetHorizontalFrameSize()
	if innerWidth < 0 {
		innerWidth = 0
	}
	// Leave one row for the title + one blank line within the content area.
	maxLines := innerHeight - 2
	if maxLines < 0 {
		maxLines = 0
	}

	var b strings.Builder
	b.WriteString(paneTitleStyle.Render("Audit log"))
	b.WriteString("\n\n")
	if len(m.auditLines) == 0 {
		b.WriteString(dimStyle.Render("(empty)"))
		return style.Render(b.String())
	}
	lines := m.auditLines
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	for i, line := range lines {
		b.WriteString(dimStyle.Render(truncate(line, innerWidth)))
		if i < len(lines)-1 {
			b.WriteString("\n")
		}
	}
	return style.Render(b.String())
}

// renderFooter draws the bottom hint line. When statusMsg is set (typically a
// tmux-attach failure), it replaces the keybinding hint so the error is
// front-and-centre instead of buried.
func (m model) renderFooter() string {
	if m.statusMsg != "" {
		return statusErrStyle.Render(m.statusMsg)
	}
	return hintStyle.Render("tab: switch pane  •  ↑/↓: select session  •  enter/click: attach  •  q: quit  •  focus: " + paneName(m.focus))
}

func paneName(p pane) string {
	if p == paneSessions {
		return "sessions"
	}
	return "projects"
}

// Run launches the TUI. It scans the given roots for projects and live-updates
// the gallery as files change; the sessions pane is fed by sessCh, which the
// caller wires up to a real source (the daemon's WatchSessions) or leaves nil
// for standalone/demo mode. The audit log pane at the bottom tails auditPath
// when non-empty. Run blocks until the user quits or ctx is cancelled.
//
// Mouse cell-motion is enabled so the sessions pane responds to clicks. The
// tmux-attach plumbing opens a new Terminal.app window via osascript, so the
// TUI keeps running while the user works in the attached session.
func Run(ctx context.Context, roots []string, sessCh <-chan []SessionView, auditPath string) error {
	tuiCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var ch <-chan []ProjectView
	if len(roots) > 0 {
		ch = WatchProjects(tuiCtx, roots, time.Second)
	}
	var auditCh <-chan []string
	if auditPath != "" {
		auditCh = TailAudit(tuiCtx, auditPath, 250*time.Millisecond, 0)
	}

	p := tea.NewProgram(
		newModel(ch, sessCh, auditCh, newTmuxAttacher()),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithContext(tuiCtx),
	)
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}
