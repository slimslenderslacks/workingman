package tui

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/slimslenderslacks/work/internal/acpclient"
)

// acpScrollModel returns a model with the ACP view open on one tab ("s") whose
// agent message holds n numbered lines L000..L<n-1>, at 80×20 — a body of
// 13 rows, so a page (ctrl+f/ctrl+b) is 11.
func acpScrollModel(t *testing.T, n int) model {
	t.Helper()
	m := newModel(nil, nil, nil)
	m.acpCh = make(chan acpTabEvent)
	m.width, m.height = 80, 20
	m = acpUpdate(t, m, acpTabEvent{kind: acpTabAdded, id: "s", title: "s"})
	acpStreamLines(t, &m, 0, n)
	m = acpUpdate(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if !m.showACP {
		t.Fatal("`a` did not open the ACP view")
	}
	return m
}

func acpUpdate(t *testing.T, m model, msg tea.Msg) model {
	t.Helper()
	step, _ := m.Update(msg)
	return step.(model)
}

// acpStreamLines streams lines L<from>..L<to-1> into tab "s" as the watcher
// would, one chunk per line.
func acpStreamLines(t *testing.T, m *model, from, to int) {
	t.Helper()
	for i := from; i < to; i++ {
		*m = acpUpdate(t, *m, acpTabEvent{kind: acpTabStream, id: "s", ev: acpclient.Event{
			State: acpclient.StateStreaming, Text: fmt.Sprintf("L%03d\n", i),
		}})
	}
}

func acpKey(t *testing.T, m model, key string) model {
	t.Helper()
	var msg tea.KeyMsg
	switch key {
	case "ctrl+f":
		msg = tea.KeyMsg{Type: tea.KeyCtrlF}
	case "ctrl+b":
		msg = tea.KeyMsg{Type: tea.KeyCtrlB}
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	return acpUpdate(t, m, msg)
}

var acpLineRe = regexp.MustCompile(`L(\d{3})`)

// acpVisible returns the numbers of the L-lines the rendered view shows, in
// order.
func acpVisible(t *testing.T, m model) []int {
	t.Helper()
	var out []int
	for _, sm := range acpLineRe.FindAllStringSubmatch(m.View(), -1) {
		n, _ := strconv.Atoi(sm[1])
		out = append(out, n)
	}
	if len(out) == 0 {
		t.Fatalf("no transcript lines visible:\n%s", m.View())
	}
	return out
}

func acpFirstVisible(t *testing.T, m model) int { return acpVisible(t, m)[0] }

func acpLastVisible(t *testing.T, m model) int {
	v := acpVisible(t, m)
	return v[len(v)-1]
}

func acpSel(t *testing.T, m model) *acpTab {
	t.Helper()
	tab, ok := m.acp.selected()
	if !ok {
		t.Fatal("no selected tab")
	}
	return tab
}

// TestACPScrollbackKeepsFullHistory: output that has scrolled off the screen is
// still buffered — following shows only the newest rows, but every line is
// kept and reachable by scrolling.
func TestACPScrollbackKeepsFullHistory(t *testing.T) {
	m := acpScrollModel(t, 100)
	if got := acpLastVisible(t, m); got != 99 {
		t.Errorf("following: last visible = L%03d, want L099", got)
	}
	if got := acpFirstVisible(t, m); got == 0 {
		t.Error("following: L000 should be off screen")
	}
	if text := acpSel(t, m).entries[0].text; !strings.Contains(text, "L000") || !strings.Contains(text, "L099") {
		t.Error("scrollback lost lines")
	}
	for i := 0; i < 20; i++ {
		m = acpKey(t, m, "ctrl+b")
	}
	if got := acpFirstVisible(t, m); got != 0 {
		t.Errorf("after paging to the top, first visible = L%03d, want L000", got)
	}
}

// TestACPScrollbackIsBounded: past acpScrollbackLimit the oldest entries are
// dropped (with a marker saying so), and the indices the tab keeps into its
// entries — the streaming message, tool calls — follow the shift.
func TestACPScrollbackIsBounded(t *testing.T) {
	defer func(old int) { acpScrollbackLimit = old }(acpScrollbackLimit)
	acpScrollbackLimit = 50

	var a acpTabs
	a.upsert("s", "s")
	for i := 0; i < 100; i++ {
		a.addPrompt("s", fmt.Sprintf("prompt %d", i))
	}
	a.apply("s", acpclient.Event{Kind: acpclient.EventToolCall, State: acpclient.StateStreaming, ToolCallID: "t1", ToolTitle: "Run"})
	a.apply("s", acpclient.Event{State: acpclient.StateStreaming, Text: "hello"})
	for i := 0; i < 30; i++ {
		a.addPrompt("s", fmt.Sprintf("more %d", i))
	}
	tab := &a.tabs[0]
	if tab.lineCount > 50 {
		t.Errorf("lineCount = %d, want <= 50", tab.lineCount)
	}
	sum := 0
	for _, e := range tab.entries {
		sum += e.weight()
	}
	if sum != tab.lineCount {
		t.Errorf("lineCount = %d, but entries weigh %d", tab.lineCount, sum)
	}
	if tab.dropped == 0 {
		t.Error("dropped = 0, want > 0")
	}
	if tab.entries[0].text == "prompt 0" {
		t.Error("oldest entry was not dropped")
	}
	if last := tab.entries[len(tab.entries)-1].text; last != "more 29" {
		t.Errorf("newest entry = %q, want %q", last, "more 29")
	}
	if idx, ok := tab.toolIdx["t1"]; ok && tab.entries[idx].toolID != "t1" {
		t.Errorf("toolIdx[t1] = %d points at %+v after trimming", idx, tab.entries[idx])
	}
	if body := renderACPTabBody(tab, 60, 200, false); !strings.Contains(body, "earlier lines dropped") {
		t.Errorf("no dropped marker in body:\n%s", body)
	}

	// A single entry larger than the whole limit keeps its tail.
	var b acpTabs
	b.upsert("s", "s")
	for i := 0; i < 200; i++ {
		b.apply("s", acpclient.Event{State: acpclient.StateStreaming, Text: fmt.Sprintf("L%03d\n", i)})
	}
	big := &b.tabs[0]
	if big.lineCount > 50 {
		t.Errorf("single entry: lineCount = %d, want <= 50", big.lineCount)
	}
	if text := big.entries[0].text; strings.Contains(text, "L000") || !strings.Contains(text, "L199") {
		t.Errorf("single entry should keep its tail; got %q", text)
	}
}

// TestACPScrollLineKeys: k moves the view up one line (entering inspection),
// j moves it back down, and reaching the bottom with j resumes following.
func TestACPScrollLineKeys(t *testing.T) {
	m := acpScrollModel(t, 100)
	last := acpFirstVisible(t, m) // the first row tracks a one-row move exactly

	m = acpKey(t, m, "k")
	if !acpSel(t, m).inspecting {
		t.Fatal("k did not enter inspection mode")
	}
	if got := acpFirstVisible(t, m); got != last-1 {
		t.Errorf("after k, first visible = L%03d, want L%03d", got, last-1)
	}
	m = acpKey(t, m, "k")
	if got := acpFirstVisible(t, m); got != last-2 {
		t.Errorf("after k k, first visible = L%03d, want L%03d", got, last-2)
	}

	m = acpKey(t, m, "j")
	if got := acpFirstVisible(t, m); got != last-1 {
		t.Errorf("after j, first visible = L%03d, want L%03d", got, last-1)
	}
	if !acpSel(t, m).inspecting {
		t.Error("j short of the bottom left inspection mode")
	}
	m = acpKey(t, m, "j")
	if acpSel(t, m).inspecting {
		t.Error("j back to the bottom did not resume follow mode")
	}
	m = acpKey(t, m, "j") // j at the bottom stays put
	if got := acpFirstVisible(t, m); got != last || acpSel(t, m).inspecting {
		t.Errorf("j at the bottom moved the view (first L%03d) or left follow mode", got)
	}
}

// TestACPScrollPageKeys: ctrl+b moves up a page (less two rows of overlap) and
// enters inspection; ctrl+f moves back down and, at the bottom, follows.
func TestACPScrollPageKeys(t *testing.T) {
	m := acpScrollModel(t, 100)
	_, h := m.acpBodySize(acpSel(t, m))
	page := acpPageRows(h)
	first := acpFirstVisible(t, m)

	m = acpKey(t, m, "ctrl+b")
	if !acpSel(t, m).inspecting {
		t.Fatal("ctrl+b did not enter inspection mode")
	}
	if got := acpFirstVisible(t, m); got != first-page {
		t.Errorf("after ctrl+b, first visible = L%03d, want L%03d", got, first-page)
	}
	m = acpKey(t, m, "ctrl+b")
	m = acpKey(t, m, "ctrl+f")
	if got := acpFirstVisible(t, m); got != first-page {
		t.Errorf("after ctrl+b ctrl+b ctrl+f, first visible = L%03d, want L%03d", got, first-page)
	}
	m = acpKey(t, m, "ctrl+f")
	if acpSel(t, m).inspecting {
		t.Error("ctrl+f back to the bottom did not resume follow mode")
	}
	if got := acpLastVisible(t, m); got != 99 {
		t.Errorf("after ctrl+f to the bottom, last visible = L%03d, want L099", got)
	}
}

// TestACPGResumesFollow: G from anywhere in the history jumps to the newest
// output and resumes following it.
func TestACPGResumesFollow(t *testing.T) {
	m := acpScrollModel(t, 100)
	for i := 0; i < 20; i++ {
		m = acpKey(t, m, "ctrl+b")
	}
	if got := acpFirstVisible(t, m); got != 0 {
		t.Fatalf("setup: first visible = L%03d, want L000", got)
	}
	m = acpKey(t, m, "G")
	if acpSel(t, m).inspecting {
		t.Error("G did not resume follow mode")
	}
	if got := acpLastVisible(t, m); got != 99 {
		t.Errorf("after G, last visible = L%03d, want L099", got)
	}
	acpStreamLines(t, &m, 100, 105)
	if got := acpLastVisible(t, m); got != 104 {
		t.Errorf("following after G: last visible = L%03d, want L104", got)
	}
}

// TestACPFollowTracksNewOutput: in follow mode the view moves with each new
// line.
func TestACPFollowTracksNewOutput(t *testing.T) {
	m := acpScrollModel(t, 50)
	acpStreamLines(t, &m, 50, 60)
	if got := acpLastVisible(t, m); got != 59 {
		t.Errorf("following: last visible = L%03d, want L059", got)
	}
}

// TestACPInspectionHoldsViewAsOutputArrives: while inspecting, new output —
// growing the streaming message, new entries below it, even trimming of the
// oldest entries above it — buffers without moving what is on screen.
func TestACPInspectionHoldsViewAsOutputArrives(t *testing.T) {
	m := acpScrollModel(t, 100)
	m = acpKey(t, m, "ctrl+b")
	m = acpKey(t, m, "k")
	before := acpVisible(t, m)

	acpStreamLines(t, &m, 100, 150)
	m = acpUpdate(t, m, acpTabEvent{kind: acpTabStream, id: "s", ev: acpclient.Event{State: acpclient.StateCompleted}})
	m = acpUpdate(t, m, acpTabEvent{kind: acpTabPrompt, id: "s", text: "next prompt"})

	if !acpSel(t, m).inspecting {
		t.Fatal("new output dropped out of inspection mode")
	}
	if after := acpVisible(t, m); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("view moved while inspecting:\nbefore %v\nafter  %v", before, after)
	}
	if !strings.Contains(m.View(), "inspection") {
		t.Error("status line no longer says inspection")
	}
	m = acpKey(t, m, "G")
	if !strings.Contains(m.View(), "next prompt") {
		t.Error("output buffered during inspection is missing after G")
	}

	// Trimming entries off the front keeps the anchored entry on screen.
	defer func(old int) { acpScrollbackLimit = old }(acpScrollbackLimit)
	acpScrollbackLimit = 40
	var a acpTabs
	a.upsert("s", "s")
	for i := 0; i < 15; i++ {
		a.addPrompt("s", fmt.Sprintf("P%03d", i))
	}
	tab := &a.tabs[0]
	lines, starts := acpTranscriptLines(tab, 40, false)
	tab.scroll(-12, starts, len(lines), 10)
	held := renderACPTabBody(tab, 40, 10, false)
	for i := 15; i < 20; i++ {
		a.addPrompt("s", fmt.Sprintf("P%03d", i))
	}
	if tab.dropped == 0 {
		t.Fatal("setup: nothing was trimmed")
	}
	if got := renderACPTabBody(tab, 40, 10, false); got != held {
		t.Errorf("trimming moved the view:\nbefore\n%s\nafter\n%s", held, got)
	}
}

// TestACPStatusLineShowsScrollMode: "live" while following; "inspection" plus
// the G hint while inspecting.
func TestACPStatusLineShowsScrollMode(t *testing.T) {
	m := acpScrollModel(t, 100)
	tab := acpSel(t, m)
	if got := acpScrollStatus(tab); got != "live" {
		t.Errorf("following: status = %q, want live", got)
	}
	if out := m.View(); !strings.Contains(out, "live") || strings.Contains(out, "inspection") {
		t.Errorf("following: view should say live, not inspection:\n%s", out)
	}
	m = acpKey(t, m, "k")
	want := "inspection — press G to follow"
	if got := acpScrollStatus(acpSel(t, m)); got != want {
		t.Errorf("inspecting: status = %q, want %q", got, want)
	}
	if out := m.View(); !strings.Contains(out, want) {
		t.Errorf("inspecting: view missing %q:\n%s", want, out)
	}
	m = acpKey(t, m, "G")
	if out := m.View(); !strings.Contains(out, "live") || strings.Contains(out, "inspection") {
		t.Errorf("after G: view should say live again:\n%s", out)
	}
}

// TestACPScrollShortTranscriptStaysLive: with nothing off screen there is no
// history to inspect, so k leaves the view in follow mode.
func TestACPScrollShortTranscriptStaysLive(t *testing.T) {
	m := acpScrollModel(t, 3)
	m = acpKey(t, m, "k")
	m = acpKey(t, m, "ctrl+b")
	if acpSel(t, m).inspecting {
		t.Error("scrolling a transcript that fits entered inspection mode")
	}
}

// TestACPScrollClampedOnResize: a resize rewraps the transcript; the pinned
// spot is kept, the offset stays inside the buffer, and the rendered view
// never overflows.
func TestACPScrollClampedOnResize(t *testing.T) {
	m := acpScrollModel(t, 100)
	for i := 0; i < 3; i++ {
		m = acpKey(t, m, "ctrl+b")
	}
	first := acpFirstVisible(t, m)

	m = acpUpdate(t, m, tea.WindowSizeMsg{Width: 60, Height: 12})
	if got := acpFirstVisible(t, m); got != first {
		t.Errorf("after shrink, first visible = L%03d, want L%03d (anchor kept)", got, first)
	}
	if lines := strings.Split(m.View(), "\n"); len(lines) >= m.height {
		t.Errorf("after shrink: %d lines, want < %d", len(lines), m.height)
	}

	// Grow taller than the whole transcript: the offset clamps to 0 and the
	// whole buffer shows, with the last line at the bottom.
	m = acpUpdate(t, m, tea.WindowSizeMsg{Width: 80, Height: 200})
	if got := acpFirstVisible(t, m); got != 0 {
		t.Errorf("after growing past the transcript, first visible = L%03d, want L000", got)
	}
	if got := acpLastVisible(t, m); got != 99 {
		t.Errorf("after growing past the transcript, last visible = L%03d, want L099", got)
	}

	// An anchor past the end (entries since dropped) clamps rather than panics.
	tab := acpSel(t, m)
	tab.inspecting, tab.anchorEntry, tab.anchorLine = true, 99, 999
	m = acpUpdate(t, m, tea.WindowSizeMsg{Width: 80, Height: 20})
	if got := acpLastVisible(t, m); got != 99 {
		t.Errorf("out-of-range anchor: last visible = L%03d, want L099 (clamped)", got)
	}
}

// TestACPScrollIsPerTab: each tab keeps its own scroll position and mode.
func TestACPScrollIsPerTab(t *testing.T) {
	m := acpScrollModel(t, 100)
	m = acpUpdate(t, m, acpTabEvent{kind: acpTabAdded, id: "other", title: "other"})
	m = acpKey(t, m, "k")
	m = acpKey(t, m, "l") // next tab
	if acpSel(t, m).inspecting {
		t.Error("the other tab inherited inspection mode")
	}
	m = acpKey(t, m, "h")
	if !acpSel(t, m).inspecting {
		t.Error("switching tabs lost the first tab's inspection mode")
	}
}

// TestACPComposeKeepsLettersAsText: while typing into a conversational tab,
// j/k/G are text, not scroll keys; ctrl+f/ctrl+b still scroll.
func TestACPComposeKeepsLettersAsText(t *testing.T) {
	m := acpScrollModel(t, 100)
	m.acp.setInteractive("s", true)
	m = acpKey(t, m, "enter")
	if !m.acpComposing {
		t.Fatal("enter did not start composing")
	}
	for _, k := range []string{"j", "k", "G"} {
		m = acpKey(t, m, k)
	}
	tab := acpSel(t, m)
	if tab.draft != "jkG" {
		t.Errorf("draft = %q, want jkG", tab.draft)
	}
	if tab.inspecting {
		t.Error("letters typed while composing scrolled the view")
	}
	m = acpKey(t, m, "ctrl+b")
	if !acpSel(t, m).inspecting {
		t.Error("ctrl+b while composing did not scroll")
	}
	m = acpKey(t, m, "ctrl+f")
	if acpSel(t, m).inspecting {
		t.Error("ctrl+f while composing did not scroll back to follow")
	}
}
