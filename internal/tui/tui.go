// Package tui hosts the orch terminal UI. The layout is a two-pane skeleton:
// a left column listing agent sessions and a main panel showing a gallery of
// project cards. Both panes reflect live state — projects from the
// .project.yaml files scanned by WatchProjects, sessions from a channel the
// daemon feeds in via its WatchSessions adapter.
package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/slimslenderslacks/work/internal/task"
)

type pane int

const (
	paneSessions pane = iota
	paneProjectYAML
	paneProjects
	paneTasks
)

// uiMode is the input mode the TUI is currently capturing keystrokes for.
// Modes don't change pane focus — they layer a text-entry overlay on top of
// it. modeCommandLine is vim-style: the user has pressed `:` and is typing a
// command at the bottom of the screen. modeNewProject is the modal dialog
// that prompts for a project name after `:new` is executed.
type uiMode int

const (
	modeNormal uiMode = iota
	modeCommandLine
	modeNewProject
)

// yamlSource picks what the YAML viewer pane renders: the selected
// project's .project.yaml or the selected task's YAML file. The user
// flips this with the p / t keys; focus changes do NOT — moving focus
// elsewhere lets the viewer keep showing what the user asked for.
type yamlSource int

const (
	yamlSourceProject yamlSource = iota
	yamlSourceTask
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
	projCh     <-chan []ProjectView
	projSel    string // path of the selected project card; empty when unset
	loaded     bool
	attacher   tmuxAttacher
	statusMsg  string

	// yamlScroll is the index of the first visible wrapped line of the
	// project-YAML viewer. Reset to 0 whenever projSel or taskSel changes
	// so a fresh selection opens from the top of the file.
	yamlScroll int

	// taskSel is the file path of the currently-selected task. Drives the
	// Tasks pane's row highlight and feeds the YAML viewer when yamlSrc is
	// yamlSourceTask. Empty when no task is selected (e.g. the project has
	// no tasks yet); reconciled against the current task list the same way
	// projSel is reconciled against the project list.
	taskSel string

	// yamlSrc picks which file the YAML viewer renders. Toggled via the p
	// / t keys; defaults to yamlSourceProject so a fresh model opens on
	// the project view that existed before the task viewer was added.
	yamlSrc yamlSource

	// projectRoot is the directory where the `:new` command creates a new
	// project's empty .project.yaml. Set by Run() from the first --root the
	// caller passed in; empty in standalone test models.
	projectRoot string

	// Input-mode state. mode gates which key handler the Update loop hands
	// the next keystroke to. cmdInput holds the characters typed after `:`
	// in command-line mode; newProjName / newProjErr drive the new-project
	// modal's input field and its inline error line.
	mode        uiMode
	cmdInput    string
	newProjName string
	newProjErr  string

	auditLines []string
	auditCh    <-chan []string

	// acp holds the ACP-session tab view: one tab per live non-interactive ACP
	// session, fed by acpCh. showACP toggles the full-window tab view on top of
	// the normal two-pane layout (entered with `a`, left with esc). acpCh is nil
	// in standalone/test models with no ACP source wired in; Run sets it.
	acp     acpTabs
	acpCh   <-chan acpTabEvent
	showACP bool
	// acpToolsExpanded controls whether tool-call blocks show their output in
	// the ACP view. Tool calls render collapsed to a single summary line by
	// default (keeping the transcript readable); the `z` key toggles this to
	// reveal every tool call's output.
	acpToolsExpanded bool
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
	if m.acpCh != nil {
		cmds = append(cmds, waitForACP(m.acpCh))
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

func waitForACP(ch <-chan acpTabEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return ev
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
		prevProjSel := m.projSel
		m.projSel = reconcileProjectSelection(m.projects, m.projSel)
		if m.projSel != prevProjSel && m.yamlSrc == yamlSourceProject {
			m.yamlScroll = 0
		}
		prevTaskSel := m.taskSel
		m.taskSel = reconcileTaskSelection(m.selectedProjectTasks(), m.taskSel)
		if m.taskSel != prevTaskSel && m.yamlSrc == yamlSourceTask {
			m.yamlScroll = 0
		}
		m.sessSel = reconcileSelection(m.sessions, m.sessSel)
		return m, waitForProjects(m.projCh)
	case sessionsMsg:
		m.sessions = msg.views
		m.sessLoaded = true
		m.sessSel = reconcileSelection(m.sessions, m.sessSel)
		return m, waitForSessions(m.sessCh)
	case auditMsg:
		m.auditLines = msg.lines
		return m, waitForAudit(m.auditCh)
	case acpTabEvent:
		switch msg.kind {
		case acpTabAdded:
			m.acp.upsert(msg.id, msg.title)
		case acpTabPrompt:
			m.acp.addPrompt(msg.id, msg.text)
		case acpTabStream:
			m.acp.apply(msg.id, msg.ev)
		case acpTabRemoved:
			m.acp.remove(msg.id)
		}
		return m, waitForACP(m.acpCh)
	case attachResultMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("attach %s: %v", msg.target, msg.err)
		} else {
			m.statusMsg = ""
		}
		return m, nil
	case tea.MouseMsg:
		// Mouse events get suspended while a modal or the ACP tab view is open —
		// the user is committed to that flow until they leave it.
		if m.mode != modeNormal || m.showACP {
			return m, nil
		}
		return m.handleMouse(msg)
	case tea.KeyMsg:
		// ctrl+c always quits, regardless of mode, so the user can't get
		// trapped in a modal they don't know how to exit.
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		// The ACP tab view captures keys whole while it's open, layered on top
		// of any pane focus the normal UI had.
		if m.showACP {
			return m.handleACPKey(msg)
		}
		switch m.mode {
		case modeCommandLine:
			return m.handleCommandLineKey(msg)
		case modeNewProject:
			return m.handleNewProjectKey(msg)
		}
		return m.handleNormalKey(msg)
	}
	return m, nil
}

// handleNormalKey processes a keystroke when no modal is open. It is the
// original key dispatcher, refactored into its own function so the Update
// loop can route keys to mode-specific handlers when a modal takes over.
func (m model) handleNormalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m, tea.Quit
	case ":":
		// Vim-style command-line entry, but only when the projects pane is
		// focused — `:new` is currently the only command and it's a
		// project-pane action. Future commands could relax this.
		if m.focus == paneProjects {
			m.mode = modeCommandLine
			m.cmdInput = ""
			m.statusMsg = ""
		}
	case "a":
		// Open the full-window ACP session tab view. Only meaningful when an
		// ACP source is wired in (daemon mode); a no-op in standalone tui mode.
		if m.acpCh != nil {
			m.showACP = true
			m.statusMsg = ""
		}
	case "left", "h":
		// Left/right move the selection (or scroll) WITHIN the focused pane:
		// previous/next project in Work Streams, previous/next task in Tasks,
		// previous/next session in Agent Sessions, line-scroll in the YAML pane.
		// Pane focus itself is moved with up/down (see below).
		m = m.moveSelectionInPane(-1)
		m.statusMsg = ""
	case "right", "l":
		m = m.moveSelectionInPane(1)
		m.statusMsg = ""
	case "p":
		// Switch the YAML viewer to project content. Independent of pane
		// focus — the user can keep navigating tasks while the viewer
		// stays on the project file.
		if m.yamlSrc != yamlSourceProject {
			m.yamlSrc = yamlSourceProject
			m.yamlScroll = 0
		}
		m.statusMsg = ""
	case "t":
		// Switch the YAML viewer to task content.
		if m.yamlSrc != yamlSourceTask {
			m.yamlSrc = yamlSourceTask
			m.yamlScroll = 0
		}
		m.statusMsg = ""
	case "ctrl+f":
		// Vim-style page-down on whichever YAML view is currently visible.
		// Independent of pane focus — the user can be navigating projects or
		// tasks and still page through the YAML pane below. The page size is
		// one screen minus one row of context overlap, matching less/vim.
		// renderProjectYAML clamps yamlScroll on overflow, so we don't need
		// the line count here.
		if page := yamlPageSize(m); page > 0 {
			m.yamlScroll += page
		}
		m.statusMsg = ""
	case "ctrl+b":
		// Vim-style page-up on whichever YAML view is currently visible.
		if page := yamlPageSize(m); page > 0 {
			m.yamlScroll -= page
			if m.yamlScroll < 0 {
				m.yamlScroll = 0
			}
		}
		m.statusMsg = ""
	case "up":
		// Up/down move focus between panes (previous/next in the cycle),
		// replacing the former Tab/Shift+Tab. Selection within a pane is moved
		// with left/right (see above).
		m.focus = shiftTogglePane(m.focus)
		m.statusMsg = ""
	case "down":
		m.focus = togglePane(m.focus)
		m.statusMsg = ""
	case "enter":
		if m.focus == paneSessions {
			return m.attachSelected()
		}
	}
	return m, nil
}

// moveSelectionInPane moves the selection (or scroll position) by delta within
// the currently focused pane: project in Work Streams, task in Tasks, session in
// Agent Sessions, or a one-line scroll in the YAML pane. delta is -1 for the
// previous item (left) and +1 for the next (right). Changing the project or task
// resets the YAML scroll when the viewer is showing that file, and changing the
// project reconciles the dependent task/session selections — mirroring the
// behaviour the up/down keys used to carry before they became pane switches.
func (m model) moveSelectionInPane(delta int) model {
	switch m.focus {
	case paneSessions:
		m.sessSel = moveSelection(m.sessions, m.sessSel, delta)
	case paneProjects:
		prev := m.projSel
		m.projSel = moveProjectSelection(m.projects, m.projSel, delta)
		if m.projSel != prev && m.yamlSrc == yamlSourceProject {
			m.yamlScroll = 0
		}
		m.taskSel = reconcileTaskSelection(m.selectedProjectTasks(), m.taskSel)
		m.sessSel = reconcileSelection(m.sessions, m.sessSel)
	case paneProjectYAML:
		m.yamlScroll += delta
		if m.yamlScroll < 0 {
			m.yamlScroll = 0
		}
	case paneTasks:
		prev := m.taskSel
		m.taskSel = moveTaskSelection(m.selectedProjectTasks(), m.taskSel, delta)
		if m.taskSel != prev && m.yamlSrc == yamlSourceTask {
			m.yamlScroll = 0
		}
	}
	return m
}

// handleACPKey processes a keystroke while the full-window ACP tab view is open.
// Left/right switch tabs; esc (or `a`) returns to the normal two-pane UI; q and
// ctrl+c still quit the whole TUI. The Tab key no longer switches tabs.
func (m model) handleACPKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m, tea.Quit
	case "esc", "a":
		m.showACP = false
	case "right", "l":
		m.acp.next()
	case "left", "h":
		m.acp.prev()
	case "z":
		m.acpToolsExpanded = !m.acpToolsExpanded
	}
	return m, nil
}

// handleMouse routes a mouse event to its pane. With the single-column
// layout, X is irrelevant — every pane spans the full body width, so the
// pane under the cursor is determined purely by Y.
//
//   - Projects pane (top): focus + select the card under the cursor.
//   - Tasks pane (middle): focus + select the task row under the cursor.
//   - Project-YAML pane (optional, between tasks and sessions): focus only;
//     the body is read-only and scrolled via keyboard.
//   - Sessions pane (bottom): focus + select the row under the cursor and
//     immediately attach to it (click-to-attach UX).
func (m model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}
	if m.width <= 0 || msg.X < 0 || msg.Y < 0 {
		return m, nil
	}
	l := m.computeLayout()

	// Vertical layout (top → bottom): header (1 row), projects, tasks,
	// optional YAML, optional sessions. The bands here mirror View()'s
	// stacking exactly so click routing and rendering can't drift.
	const headerRows = 1
	projectsEnd := headerRows + l.projectsH
	tasksStart := projectsEnd
	tasksEnd := tasksStart + l.tasksH
	yamlStart := tasksEnd
	yamlEnd := yamlStart + l.yamlH
	sessionsStart := yamlEnd
	sessionsEnd := sessionsStart + l.sessionsH

	if msg.Y >= headerRows && msg.Y < projectsEnd {
		innerW := l.bodyW - unfocusedBorder.GetHorizontalFrameSize()
		if innerW < 0 {
			innerW = 0
		}
		idx := projectCardAtPoint(msg.X, msg.Y, innerW, len(m.projects))
		m.focus = paneProjects
		m.statusMsg = ""
		if idx < 0 || idx >= len(m.projects) {
			return m, nil
		}
		prev := m.projSel
		m.projSel = m.projects[idx].Path
		if m.projSel != prev && m.yamlSrc == yamlSourceProject {
			m.yamlScroll = 0
		}
		m.taskSel = reconcileTaskSelection(m.selectedProjectTasks(), m.taskSel)
		m.sessSel = reconcileSelection(m.sessions, m.sessSel)
		return m, nil
	}
	if msg.Y >= tasksStart && msg.Y < tasksEnd {
		m.focus = paneTasks
		m.statusMsg = ""
		tasks := m.selectedProjectTasks()
		idx := taskRowAtY(msg.Y, tasksStart, len(tasks))
		if idx >= 0 {
			prev := m.taskSel
			m.taskSel = tasks[idx].Path
			if m.taskSel != prev && m.yamlSrc == yamlSourceTask {
				m.yamlScroll = 0
			}
		}
		return m, nil
	}
	if l.yamlH > 0 && msg.Y >= yamlStart && msg.Y < yamlEnd {
		m.focus = paneProjectYAML
		m.statusMsg = ""
		return m, nil
	}
	if l.sessionsH > 0 && msg.Y >= sessionsStart && msg.Y < sessionsEnd {
		idx := sessionRowAtY(msg.Y, sessionsStart, len(m.sessions))
		m.focus = paneSessions
		m.statusMsg = ""
		if idx < 0 {
			return m, nil
		}
		m.sessSel = m.sessions[idx].ID
		return m.attachSelected()
	}
	// Click below every pane (audit area or empty). Focus projects as a
	// safe default so up/down still does something sensible.
	m.focus = paneProjects
	return m, nil
}

// yamlPageSize returns the number of lines ctrl+f / ctrl+b should scroll
// the YAML pane by — one screen minus one row of context overlap so the
// last visible line before a page-down becomes the first line after it
// (matching less/vim convention). Returns 0 when the YAML pane is not in
// the layout, so the caller can treat that as "no-op".
func yamlPageSize(m model) int {
	l := m.computeLayout()
	if l.yamlH <= 0 {
		return 0
	}
	// Vertical chrome inside the pane: top border + bottom border + title +
	// blank = 4 rows. The remainder is visible content.
	const chromeRows = 4
	contentRows := l.yamlH - chromeRows
	if contentRows < 1 {
		return 0
	}
	if contentRows == 1 {
		return 1
	}
	return contentRows - 1
}

// taskRowAtY maps an absolute y row to a task index using the tasks pane's
// table layout: top border (1) + title (1) + blank (1) + column header (1) +
// one row per task. Returns -1 for clicks on the chrome (incl. the header
// row) or beyond the last task.
func taskRowAtY(y, paneTop, count int) int {
	const chromeRows = 4 // top border + "Tasks" + blank + column header
	rel := y - paneTop - chromeRows
	if rel < 0 || rel >= count {
		return -1
	}
	return rel
}

// attachSelected dispatches the tmux-attach command for the currently
// selected session row. The actual suspend/resume is owned by bubbletea via
// tea.ExecProcess; this model just returns the Cmd.
func (m model) attachSelected() (tea.Model, tea.Cmd) {
	if m.attacher == nil {
		m.statusMsg = "tmux attach disabled (no attacher wired)"
		return m, nil
	}
	target, ok := selectedTmuxTarget(m.sessions, m.sessSel)
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

// sessionRowAtY maps an absolute y row to a session index. paneStartY is the
// absolute y of the sessions pane's top border (so this function works no
// matter where the pane sits in the stack). Inside the pane:
// top border(1) + "Agent Sessions" title(1) + blank(1) + column header(1) +
// one row per session.
func sessionRowAtY(y, paneStartY, count int) int {
	const (
		paneTopBorder   = 1
		paneTitleLines  = 2 // title + blank
		paneHeaderLines = 1 // column header
	)
	if count <= 0 {
		return -1
	}
	rel := y - paneStartY - (paneTopBorder + paneTitleLines + paneHeaderLines)
	if rel < 0 || rel >= count {
		return -1
	}
	return rel
}

// togglePane cycles forward through the focusable panes in the order they
// appear on screen, top-to-bottom: projects → tasks → project-YAML →
// sessions → projects. shiftTogglePane is the inverse cycle.
func togglePane(p pane) pane {
	switch p {
	case paneProjects:
		return paneTasks
	case paneTasks:
		return paneProjectYAML
	case paneProjectYAML:
		return paneSessions
	default:
		return paneProjects
	}
}

func shiftTogglePane(p pane) pane {
	switch p {
	case paneProjects:
		return paneSessions
	case paneSessions:
		return paneProjectYAML
	case paneProjectYAML:
		return paneTasks
	default:
		return paneProjects
	}
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
// cardDisplayRows is the rendered height of a card: top border + name +
// status + breakdown + bottom border = 5 rows. The grid uses it to decide
// how many full card rows fit in the projects pane.
const (
	cardTargetWidth = 30
	cardMinWidth    = 20
	cardGap         = 1
	cardDisplayRows = 5
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
	// lipgloss.Width/Height(N) set the *content+padding* size; borders are
	// added outside. Caller passes the total cols/rows we want the pane to
	// occupy, so we subtract the border size before handing dimensions to
	// Width()/Height(). Without the Width adjustment the pane overflows by
	// 2 cols on the right, which clips the rightmost border off the screen
	// once both body panes are joined horizontally.
	bs := m.borderStyle(paneSessions)
	base := bs.Width(width - bs.GetHorizontalBorderSize())
	innerHeight := height - base.GetVerticalFrameSize()
	if innerHeight < 0 {
		innerHeight = 0
	}
	// MaxWidth is a hard cap lipgloss applies after the border so a long
	// line can't push the pane past the terminal edge. Vertical overflow is
	// handled by clamping the content lines below (clipping after the
	// border would erase the bottom border).
	style := base.Height(innerHeight).MaxWidth(width)
	innerWidth := width - style.GetHorizontalFrameSize()
	if innerWidth < 0 {
		innerWidth = 0
	}

	var b strings.Builder
	b.WriteString(paneTitleStyle.Render("Agent Sessions"))
	b.WriteString("\n\n")
	if !m.sessLoaded {
		b.WriteString(dimStyle.Render("(loading…)"))
		return style.Render(clampLines(b.String(), innerHeight))
	}
	if len(m.sessions) == 0 {
		b.WriteString(dimStyle.Render("(none)"))
		return style.Render(clampLines(b.String(), innerHeight))
	}

	cols := sessionColumnWidths(innerWidth)
	b.WriteString(renderSessionHeader(cols))

	// 3 chrome rows above (title, blank, header) — data rows fit in the
	// remaining inner height. Anything past that is dropped so the bottom
	// border survives.
	maxRows := innerHeight - 3
	if maxRows < 0 {
		maxRows = 0
	}
	sessions := m.sessions
	if len(sessions) > maxRows {
		sessions = sessions[:maxRows]
	}
	for _, s := range sessions {
		b.WriteString("\n")
		b.WriteString(renderSessionRow(s, cols, s.ID == m.sessSel))
	}
	return style.Render(clampLines(b.String(), innerHeight))
}

// sessionColumns holds the per-column character widths for the sessions
// table. Computed once per render from the pane's inner width so the header
// and every data row share the same alignment.
type sessionColumns struct {
	agent, project, task, status, sandbox int
}

// Column gap is one space; the selection marker occupies a fixed two-column
// prefix before the table proper (so the marker can flip without touching
// column alignment).
const (
	sessionMarkerWidth   = 2
	sessionColGap        = 1
	sessionColGaps       = 4
	sessionColAgentDef   = 10
	sessionColTaskDef    = 14
	sessionColStatusDef  = 10
	sessionColSandboxDef = 28
	sessionColMinProject = 4
)

// sessionColumnWidths sizes the five columns to fill innerWidth (after the
// selection-marker prefix). project takes whatever's left; the other four get
// fixed defaults that shrink in priority order (task → sandbox → agent) when
// the pane is too narrow to honour them.
func sessionColumnWidths(innerWidth int) sessionColumns {
	c := sessionColumns{
		agent:   sessionColAgentDef,
		task:    sessionColTaskDef,
		status:  sessionColStatusDef,
		sandbox: sessionColSandboxDef,
	}
	avail := innerWidth - sessionMarkerWidth
	if avail < 0 {
		avail = 0
	}
	required := func() int {
		return c.agent + c.task + c.status + c.sandbox + sessionColGaps*sessionColGap
	}
	// task shrinks first (least load-bearing for at-a-glance read), then
	// sandbox (often long but informational), then agent (kind labels are
	// short, so this rarely fires). status stays fixed — it's the
	// at-a-glance signal.
	for avail-required() < sessionColMinProject && c.task > 0 {
		c.task--
	}
	for avail-required() < sessionColMinProject && c.sandbox > 0 {
		c.sandbox--
	}
	for avail-required() < sessionColMinProject && c.agent > 0 {
		c.agent--
	}
	c.project = avail - required()
	if c.project < 0 {
		c.project = 0
	}
	return c
}

// renderSessionHeader emits the column-header row in the dim style so the
// table reads as a unit. The marker prefix is left blank — selection state
// belongs on data rows only.
func renderSessionHeader(c sessionColumns) string {
	cells := []string{
		padCell("agent", c.agent),
		padCell("project", c.project),
		padCell("task", c.task),
		padCell("status", c.status),
		padCell("sandbox", c.sandbox),
	}
	gap := strings.Repeat(" ", sessionColGap)
	return dimStyle.Render(strings.Repeat(" ", sessionMarkerWidth) + strings.Join(nonEmpty(cells), gap))
}

// renderSessionRow lays one session out as a single table row:
// marker | agent | project | task | status | sandbox.
//
// The marker is a two-char selection indicator that flips between ▶ and a
// blank. Truncation happens on the raw text before any style is applied so
// the byte-slice in truncate never tears an ANSI escape.
//
// Three row styles, in precedence order:
//   - selected: accent background across the full row width.
//   - interactive (not selected): yellow agent cell flags rows that wait
//     for a human, even when another pane is focused.
//   - autonomous (not selected): plain cells, status-coloured status cell.
//
// Sandbox is only populated for ACP-routed sessions; for the rest the cell
// renders an em-dash so the column reads as "n/a" rather than empty.
func renderSessionRow(s SessionView, c sessionColumns, selected bool) string {
	marker := sessionMarkerIdle
	if selected {
		marker = sessionMarkerSelected
	}

	agent := s.AgentName
	if s.Interactive {
		agent += interactiveBadge
	}
	statusText := sessionStatusGlyph(s.Status) + " " + s.Status
	sandbox := s.SandboxName
	if sandbox == "" {
		sandbox = "—"
	}

	gap := strings.Repeat(" ", sessionColGap)

	if selected {
		cells := []string{
			padCell(truncate(agent, c.agent), c.agent),
			padCell(truncate(s.Project, c.project), c.project),
			padCell(truncate(s.TaskName, c.task), c.task),
			padCell(truncate(statusText, c.status), c.status),
			padCell(truncate(sandbox, c.sandbox), c.sandbox),
		}
		plain := marker + strings.Join(nonEmpty(cells), gap)
		return sessionRowSelectedStyle.Render(padToWidth(plain, c.totalWidth()))
	}

	// Non-selected rows: status cell is coloured by status; the agent cell
	// is coloured yellow on interactive kinds. All other cells stay plain.
	agentCell := padCell(truncate(agent, c.agent), c.agent)
	if s.Interactive {
		agentCell = sessionRowInteractiveStyle.Render(agentCell)
	}
	statusCell := sessionStatusStyle(s.Status).Render(truncate(statusText, c.status))
	statusCell = padCell(statusCell, c.status)

	cells := []string{
		agentCell,
		padCell(truncate(s.Project, c.project), c.project),
		padCell(truncate(s.TaskName, c.task), c.task),
		statusCell,
		padCell(truncate(sandbox, c.sandbox), c.sandbox),
	}
	return marker + strings.Join(nonEmpty(cells), gap)
}

// totalWidth is the rendered width of a session row including the marker
// prefix and all column gaps. Used by the selected-row highlight so the
// accent background fills the full pane width.
func (c sessionColumns) totalWidth() int {
	w := sessionMarkerWidth
	gaps := 0
	for _, n := range []int{c.agent, c.project, c.task, c.status, c.sandbox} {
		if n > 0 {
			w += n
			gaps++
		}
	}
	if gaps > 1 {
		w += (gaps - 1) * sessionColGap
	}
	return w
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

// clampLines returns s truncated to at most max lines. Used by the pane
// renderers to keep content from overflowing the box vertically — clipping
// before the border is applied keeps the bottom border intact, unlike
// lipgloss's MaxHeight which clips after the border and would erase it.
func clampLines(s string, max int) string {
	if max <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= max {
		return s
	}
	return strings.Join(lines[:max], "\n")
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
	bs := m.borderStyle(paneProjects)
	base := bs.Width(width - bs.GetHorizontalBorderSize())
	innerHeight := height - base.GetVerticalFrameSize()
	if innerHeight < 0 {
		innerHeight = 0
	}
	style := base.Height(innerHeight).MaxWidth(width)
	innerWidth := width - style.GetHorizontalFrameSize()
	if innerWidth < 0 {
		innerWidth = 0
	}

	var b strings.Builder
	b.WriteString(paneTitleStyle.Render("Work Streams"))
	b.WriteString("\n\n")
	if !m.loaded {
		b.WriteString(dimStyle.Render("(loading…)"))
		return style.Render(clampLines(b.String(), innerHeight))
	}
	if len(m.projects) == 0 {
		b.WriteString(dimStyle.Render("(none)"))
		return style.Render(clampLines(b.String(), innerHeight))
	}

	// Two rows are already consumed by the title and the blank line that
	// follows it, so cards have innerHeight-2 rows to play with.
	cardsBudget := innerHeight - 2
	if cardsBudget < 0 {
		cardsBudget = 0
	}
	b.WriteString(renderProjectGrid(m.projects, m.projSel, innerWidth, cardsBudget))
	return style.Render(clampLines(b.String(), innerHeight))
}

// renderProjectGrid lays project cards out left-to-right, wrapping to a new
// row whenever the next card wouldn't fit in innerWidth. Cards shrink to the
// available width when the pane can't hold one at the target width. The card
// matching selPath gets the highlighted-border treatment.
//
// rowBudget is the number of terminal rows available for cards (i.e. the
// projects pane's inner height minus title and blank). The grid renders
// only the card rows that fit entirely — a partial card with a missing
// bottom border looks broken, so we drop the row instead.
func renderProjectGrid(views []ProjectView, selPath string, innerWidth, rowBudget int) string {
	if innerWidth <= 0 || len(views) == 0 || rowBudget < cardDisplayRows {
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

	// Cap the number of cards to the row budget: only whole card rows fit.
	maxCardRowsTotal := rowBudget / cardDisplayRows
	if maxViews := maxCardRowsTotal * perRow; len(views) > maxViews {
		views = views[:maxViews]
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
	// width is the desired display width on screen; lipgloss .Width(N) sets
	// the content+padding size and adds borders outside, so subtract the
	// border size before handing it over. Skipping this fragments the
	// rounded border into two lines when cardWidth ≈ inner pane width.
	style := border.Width(width - border.GetHorizontalBorderSize())
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
// is large enough to split. Sized so exactly one row of cards fits cleanly:
// 2 border + 1 title + 1 blank + 5 card = 9 rows.
const projectsMinHeight = 9

// tasksMinHeight is the floor for the tasks pane below the projects pane.
// 6 rows = top border + "Tasks" title + blank + column header + 1 task row +
// bottom border. Below this the table can't show even one task underneath
// the column headers.
const tasksMinHeight = 6

// yamlMinHeight is the floor for the project-YAML pane stacked between
// projects and tasks. 5 rows = top border + title + blank + 1 content line +
// bottom border. Below this we drop the pane entirely and fall back to
// projects + tasks the way it was before the YAML viewer existed.
const yamlMinHeight = 5

// sessionsBottomHeight is the target height of the sessions pane when it
// sits at the bottom of the body stack. 10 rows = 5 chrome (top border,
// title, blank, column header, bottom border) + 5 content, enough for five
// rows of the table. The pane is deliberately short — sessions are
// status-at-a-glance, not the primary workspace.
const sessionsBottomHeight = 10

// sessionsMinHeight is the absolute floor for the sessions pane: 5 chrome +
// 1 content. Below this we drop it from the layout entirely so the projects/
// tasks/yaml stack can still fit on a tiny terminal.
const sessionsMinHeight = 6

// uiLayout caches the computed dimensions of every pane for a given window
// size. View() and handleMouse() both use it so the rendering and the
// click-routing math stay in lockstep.
//
// The body is a single column stacked vertically: projects (top), tasks,
// project/task YAML viewer (optional), and the sessions pane at the bottom.
// yamlH is 0 when the terminal is too short to fit all four; sessionsH is 0
// when even after dropping yaml there's no room. bodyW is the full inner
// width (we no longer split a left column out for sessions).
type uiLayout struct {
	bodyW                                       int
	bodyH                                       int
	projectsH, yamlH, tasksH, sessionsH, auditH int
}

func (m model) computeLayout() uiLayout {
	bodyW := m.width

	// Reserve audit space only when there's room for it without squashing
	// the body below a usable minimum. Otherwise the audit pane forces the
	// body to overflow the terminal and the footer disappears off-screen.
	const (
		headerH   = 1
		footerH   = 1
		minBodyH  = 4
		minAuditH = 5
	)
	audit := 0
	if m.auditCh != nil {
		candidate := auditPaneHeight
		if candidate > m.height/3 {
			candidate = m.height / 3
		}
		if candidate >= minAuditH && m.height-headerH-footerH-candidate >= minBodyH {
			audit = candidate
		}
	}
	bodyH := m.height - headerH - footerH - audit
	if bodyH < 1 {
		bodyH = 1
	}

	// Carve a fixed-ish slice for the sessions pane at the bottom of the
	// body. Drop it entirely when there isn't enough room left for the rest
	// of the stack to hit their minimums — we'd rather lose the at-a-glance
	// status pane than overflow the terminal.
	sessionsH := sessionsBottomHeight
	if sessionsH > bodyH/3 {
		sessionsH = bodyH / 3
	}
	if sessionsH < sessionsMinHeight ||
		bodyH-sessionsH < projectsMinHeight+tasksMinHeight {
		sessionsH = 0
	}

	projH, yamlH, tasksH := splitRightColumn(bodyH-sessionsH, bodyW, len(m.projects))
	return uiLayout{
		bodyW:     bodyW,
		bodyH:     bodyH,
		projectsH: projH,
		yamlH:     yamlH,
		tasksH:    tasksH,
		sessionsH: sessionsH,
		auditH:    audit,
	}
}

// splitRightColumn divides the right column's bodyH rows between projects
// (top), tasks (middle), and the YAML viewer (bottom).
//
// Sizing strategy:
//   - When all three fit, projects grows to whatever height it would need
//     to render every card without truncation (see desiredProjectsHeight),
//     capped so yaml and tasks still get at least their minimums. The
//     leftover rows are split roughly 55/45 between yaml and tasks. YAML
//     gets a hair more because a long .project.yaml is the reason the
//     pane was added.
//   - When only two fit, drop the YAML middle and revert to the older
//     projects+tasks split. Projects still grows up to whatever rows are
//     available beyond tasksMinHeight so a long gallery isn't artificially
//     clipped to one row in the narrow regime either.
//
// All clamps prefer fitting within bodyH over hitting the per-pane minimums
// so the right column never overflows the audit footer below it.
func splitRightColumn(bodyH, projW, projCount int) (proj, yaml, tasks int) {
	if bodyH <= 0 {
		return 0, 0, 0
	}
	desired := desiredProjectsHeight(projW, projCount)
	if desired < projectsMinHeight {
		desired = projectsMinHeight
	}
	// Can we fit all three at their minimums?
	if bodyH >= projectsMinHeight+yamlMinHeight+tasksMinHeight {
		maxProj := bodyH - yamlMinHeight - tasksMinHeight
		proj = desired
		if proj > maxProj {
			proj = maxProj
		}
		if proj < projectsMinHeight {
			proj = projectsMinHeight
		}
		remaining := bodyH - proj
		yaml = remaining * 11 / 20
		if yaml < yamlMinHeight {
			yaml = yamlMinHeight
		}
		tasks = remaining - yaml
		if tasks < tasksMinHeight {
			tasks = tasksMinHeight
			yaml = remaining - tasks
		}
		return proj, yaml, tasks
	}
	// Not enough room for the YAML middle pane; fall back to the original
	// projects+tasks split so existing callers see the same behaviour they
	// did before the viewer was added.
	proj = desired
	if proj > bodyH-tasksMinHeight {
		proj = bodyH - tasksMinHeight
	}
	if proj < projectsMinHeight {
		proj = projectsMinHeight
	}
	if proj > bodyH {
		proj = bodyH
	}
	if proj < 1 {
		proj = 1
	}
	tasks = bodyH - proj
	if tasks < 0 {
		tasks = 0
	}
	return proj, 0, tasks
}

// desiredProjectsHeight returns the row count the projects pane would need
// to render every project card without truncation, given the pane's outer
// width and the number of projects. The math mirrors renderProjectGrid's
// own layout so a pane sized this tall holds the same grid the renderer
// would draw at bodyH ≫ enough.
//
// Returns projectsMinHeight when projW or projCount is too small to compute
// a meaningful answer — the caller still floors at projectsMinHeight, but
// returning it directly here avoids a divide-by-zero in the per-row math.
func desiredProjectsHeight(projW, projCount int) int {
	if projCount <= 0 || projW <= 0 {
		return projectsMinHeight
	}
	innerWidth := projW - unfocusedBorder.GetHorizontalFrameSize()
	if innerWidth < 1 {
		return projectsMinHeight
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
	totalRows := (projCount + perRow - 1) / perRow
	// Pane chrome: top border + title + blank + bottom border = 4 rows. Each
	// card row is cardDisplayRows tall.
	return paneChromeRows + totalRows*cardDisplayRows
}

// paneChromeRows is the non-content height every pane carries: two border
// rows plus the title + blank header. Used by sizing math that needs to ask
// "how many rows does N rows of content cost?".
const paneChromeRows = 4

// Minimum terminal dimensions below which we don't try to lay out the full
// UI — the panes' titles and a single row of content can't fit, so we show
// a one-line "terminal too small" message instead of a garbled grid.
const (
	minTerminalWidth  = 24
	minTerminalHeight = 6
)

func (m model) View() string {
	if m.width == 0 || m.height == 0 {
		return "starting orch tui…"
	}
	if m.width < minTerminalWidth || m.height < minTerminalHeight {
		return fmt.Sprintf("terminal too small (need ≥ %d×%d)", minTerminalWidth, minTerminalHeight)
	}

	// Modal mode replaces the entire screen with the centered dialog.
	// Bubbletea has no native overlay primitive, so blanking the body is
	// the simplest reliable approach — the user dismisses the modal with
	// esc/enter and the regular UI returns intact.
	if m.mode == modeNewProject {
		return m.renderNewProjectModal()
	}

	// The ACP tab view takes over the whole window when open.
	if m.showACP {
		return m.renderACPView()
	}

	l := m.computeLayout()
	header := titleStyle.Render("orch")
	footer := m.renderFooter()

	projects := m.renderProjects(l.bodyW, l.projectsH)
	tasks := m.renderTasks(l.bodyW, l.tasksH)
	panes := []string{projects, tasks}
	if l.yamlH > 0 {
		panes = append(panes, m.renderProjectYAML(l.bodyW, l.yamlH))
	}
	if l.sessionsH > 0 {
		panes = append(panes, m.renderSessions(l.bodyW, l.sessionsH))
	}
	body := lipgloss.JoinVertical(lipgloss.Left, panes...)

	if l.auditH > 0 {
		audit := m.renderAudit(m.width, l.auditH)
		return lipgloss.JoinVertical(lipgloss.Left, header, body, audit, footer)
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

// renderTasks draws the per-project task list pane as a table: one header
// row plus one row per task with columns name | model | mcps | rules |
// status. The list is bound to whichever project projSel currently points
// at; switching project swaps the content.
//
// The row matching taskSel gets the highlighted treatment (same pink accent
// used by the session row selection) so the user can see which task the
// YAML viewer is currently pointing at when this pane is focused. The Tasks
// pane uses the focused border when m.focus == paneTasks so the user knows
// the pane accepts up/down input.
//
// `height` is the total rows the pane should occupy. Borders eat 2 of them.
func (m model) renderTasks(width, height int) string {
	bs := m.borderStyle(paneTasks)
	base := bs.Width(width - bs.GetHorizontalBorderSize())
	innerHeight := height - base.GetVerticalFrameSize()
	if innerHeight < 0 {
		innerHeight = 0
	}
	style := base.Height(innerHeight).MaxWidth(width)
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
		return style.Render(clampLines(b.String(), innerHeight))
	}

	cols := taskColumnWidths(innerWidth)
	b.WriteString(renderTaskHeader(cols))

	// 3 chrome rows above (title, blank, header) and we just wrote the
	// header — so task data rows fit in innerHeight-3.
	maxRows := innerHeight - 3
	if maxRows < 0 {
		maxRows = 0
	}
	if len(tasks) > maxRows {
		tasks = tasks[:maxRows]
	}
	for _, t := range tasks {
		b.WriteString("\n")
		b.WriteString(renderTaskRow(t, cols, t.Path == m.taskSel))
	}
	return style.Render(clampLines(b.String(), innerHeight))
}

// taskColumns holds the per-column character widths the tasks table renders
// with. Computed once per render from the pane's inner width so the header
// and every data row share the same alignment.
type taskColumns struct {
	name, model, mcps, rules, status int
}

// Column gap is one space; five columns means four gaps.
const (
	taskColGap           = 1
	taskColGaps          = 4
	taskColModelDefault  = 10
	taskColMCPsDefault   = 18
	taskColRulesDefault  = 6
	taskColStatusDefault = 10
	taskColMinNameWidth  = 4
	taskColMinTotalWidth = taskColMinNameWidth + taskColModelDefault + taskColMCPsDefault + taskColRulesDefault + taskColStatusDefault + taskColGaps*taskColGap
)

// taskColumnWidths sizes the five columns to fill innerWidth. The model,
// mcps, rules and status columns get fixed defaults; the name column takes
// whatever's left. When the pane is too narrow to honour the defaults, the
// model/mcps columns shrink (in that order) before name drops below its
// minimum. mcps and rules can both drop to 0 — they're informational, so
// truncating them to nothing is better than crowding name and status.
func taskColumnWidths(innerWidth int) taskColumns {
	c := taskColumns{
		name:   0,
		model:  taskColModelDefault,
		mcps:   taskColMCPsDefault,
		rules:  taskColRulesDefault,
		status: taskColStatusDefault,
	}
	required := func() int {
		gaps := 0
		if c.model > 0 {
			gaps++
		}
		if c.mcps > 0 {
			gaps++
		}
		if c.rules > 0 {
			gaps++
		}
		if c.status > 0 {
			gaps++
		}
		return c.model + c.mcps + c.rules + c.status + gaps*taskColGap
	}
	// Shrink the optional informational columns when there isn't enough
	// room. Order: mcps (verbose) → rules (small) → model (small).
	for innerWidth-required() < taskColMinNameWidth && c.mcps > 0 {
		c.mcps--
	}
	for innerWidth-required() < taskColMinNameWidth && c.rules > 0 {
		c.rules--
	}
	for innerWidth-required() < taskColMinNameWidth && c.model > 0 {
		c.model--
	}
	c.name = innerWidth - required()
	if c.name < taskColMinNameWidth {
		c.name = taskColMinNameWidth
	}
	// Status always survives — it's the at-a-glance signal. If even that
	// doesn't fit (super narrow pane), let it truncate when rendered.
	return c
}

// renderTaskHeader emits the column-header row in the title style so the
// table reads as one above its data.
func renderTaskHeader(c taskColumns) string {
	cells := []string{
		padCell("name", c.name),
		padCell("model", c.model),
		padCell("mcps", c.mcps),
		padCell("rules", c.rules),
		padCell("status", c.status),
	}
	return dimStyle.Render(strings.Join(nonEmpty(cells), strings.Repeat(" ", taskColGap)))
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

// renderTaskRow lays one task out as a row in the table: name | model |
// mcps | rules | status. Each cell is padded to its column width so the
// columns line up; columns sized to 0 by taskColumnWidths drop out entirely.
// The status column is the only one that carries colour — same palette as
// the project status indicator so the eye learns one mapping.
//
// When selected, every cell is rendered with the accent background/foreground
// used elsewhere for "active selection" so the user can see at a glance
// which task the YAML viewer is mirroring.
func renderTaskRow(t TaskView, c taskColumns, selected bool) string {
	model := t.Model
	if model == "" {
		// Defensive: TaskView already mirrors task.Load()'s backfill, but
		// nothing in this file guarantees that path was taken — display
		// "default" so a stray empty value doesn't render as a blank cell.
		model = "default"
	}
	mcps := strings.Join(t.StaticMCPs, ",")
	if mcps == "" {
		mcps = "-"
	}
	rules := "-"
	if n := len(t.Policies); n > 0 {
		rules = strconv.Itoa(n)
	}
	status := string(t.Status)

	cells := []string{
		padCell(truncate(t.Name, c.name), c.name),
		padCell(truncate(model, c.model), c.model),
		padCell(truncate(mcps, c.mcps), c.mcps),
		padCell(truncate(rules, c.rules), c.rules),
		padCell(truncate(status, c.status), c.status),
	}
	gap := strings.Repeat(" ", taskColGap)
	plain := strings.Join(nonEmpty(cells), gap)
	if selected {
		return sessionRowSelectedStyle.Render(padToWidth(plain, c.totalWidth()))
	}
	// Re-emit non-status cells as plain and recolour just the status cell so
	// the table tints stay subtle.
	if c.status > 0 {
		nonStatus := nonEmpty(cells[:4])
		joined := strings.Join(nonStatus, gap)
		statusCell := taskStatusStyle(t.Status).Render(truncate(status, c.status))
		statusCell = padCell(statusCell, c.status)
		if joined == "" {
			return statusCell
		}
		return joined + gap + statusCell
	}
	return plain
}

// totalWidth is the rendered width of a row including gaps. Used by the
// selected-row highlight so the accent background fills the full column.
func (c taskColumns) totalWidth() int {
	w := 0
	gaps := 0
	for _, n := range []int{c.name, c.model, c.mcps, c.rules, c.status} {
		if n > 0 {
			w += n
			gaps++
		}
	}
	if gaps > 1 {
		w += (gaps - 1) * taskColGap
	}
	return w
}

// padCell pads s to exactly width visible columns on the right with spaces,
// or returns "" when width is 0 (column dropped). It's used both for header
// and data cells so columns align by character count regardless of styling.
func padCell(s string, width int) string {
	if width <= 0 {
		return ""
	}
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

// nonEmpty returns a new slice with empty strings dropped, preserving order.
// Used to assemble row text without leading/trailing/duplicate gaps when a
// column has been dropped. A fresh slice is allocated so callers can pass
// sub-slices of the same backing array (e.g. cells, cells[:4]) without one
// call clobbering the other.
func nonEmpty(parts []string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
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
	base := unfocusedBorder.Width(width - unfocusedBorder.GetHorizontalBorderSize())
	innerHeight := height - base.GetVerticalFrameSize()
	if innerHeight < 0 {
		innerHeight = 0
	}
	style := base.Height(innerHeight).MaxWidth(width)
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
		return style.Render(clampLines(b.String(), innerHeight))
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
	return style.Render(clampLines(b.String(), innerHeight))
}

// renderFooter draws the bottom hint line. Four variants, in precedence
// order:
//
//   - Command-line mode: shows the vim-style `:cmd` prompt with a cursor so
//     the user sees what they're typing.
//   - statusMsg set (e.g. tmux-attach failure, "created project foo"): the
//     message replaces the hint so the user can't miss it.
//   - Projects pane focused: hint includes the `:new` discovery affordance.
//   - Default: the regular keybinding hint.
//
// All variants are truncated to the terminal width so a long string can't
// push the line past the screen edge (which on bubbletea altscreen wraps it
// onto a phantom row that scrolls the rest of the UI up).
func (m model) renderFooter() string {
	if m.mode == modeCommandLine {
		text := ":" + m.cmdInput + "▌"
		if m.width > 0 {
			text = truncate(text, m.width)
		}
		return hintStyle.Render(text)
	}
	text := m.statusMsg
	style := statusErrStyle
	if text == "" {
		base := "↑/↓: switch pane  •  ←/→: select in pane  •  p/t: project/task yaml  •  enter/click: attach  •  q: quit"
		if m.acpCh != nil {
			base += "  •  a: acp tabs"
		}
		if m.focus == paneProjects {
			base += "  •  :new: create project"
		}
		text = base + "  •  focus: " + paneName(m.focus)
		style = hintStyle
	}
	if m.width > 0 {
		text = truncate(text, m.width)
	}
	return style.Render(text)
}

func paneName(p pane) string {
	switch p {
	case paneSessions:
		return "sessions"
	case paneProjectYAML:
		return "yaml"
	case paneTasks:
		return "tasks"
	default:
		return "work streams"
	}
}

// Run launches the TUI. It scans the given roots for projects and live-updates
// the gallery as files change; the sessions pane is fed by sessCh, which the
// caller wires up to a real source (the daemon's WatchSessions) or leaves nil
// for standalone/demo mode. The audit log pane at the bottom tails auditPath
// when non-empty. When sessionsRoot is non-empty, Run also watches that ACP
// sessions root and feeds the `a`-key tab view one tab per live ACP session.
// Run blocks until the user quits or ctx is cancelled.
//
// Mouse cell-motion is enabled so the sessions pane responds to clicks. The
// tmux-attach plumbing opens a new Terminal.app window via osascript, so the
// TUI keeps running while the user works in the attached session.
func Run(ctx context.Context, roots []string, sessCh <-chan []SessionView, auditPath, sessionsRoot string) error {
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

	m := newModel(ch, sessCh, auditCh, newTmuxAttacher())
	// Watch ACP sessions for the tab view when a sessions root is configured
	// (daemon mode with --acp-kit). Standalone `orch tui` passes "" and gets no
	// ACP tabs, matching its nil sessions source.
	if sessionsRoot != "" {
		m.acpCh = WatchACPSessions(tuiCtx, sessionsRoot, 0)
	}
	// `:new` writes the empty .project.yaml into the first --root the caller
	// passed in. Standalone tui mode without any --root has no place to
	// create projects; the command-line handler reports that gracefully.
	if len(roots) > 0 {
		m.projectRoot = roots[0]
	}

	p := tea.NewProgram(
		m,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithContext(tuiCtx),
	)
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}
