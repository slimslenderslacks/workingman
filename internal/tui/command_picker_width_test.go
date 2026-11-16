package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/slimslenderslacks/work/internal/project"
)

// TestCommandPickerRowsFitOnOneLine checks that at a typical terminal width
// the modal is wide enough that no command's description wraps: each label
// appears on exactly one rendered line, and that line also holds its desc.
func TestCommandPickerRowsFitOnOneLine(t *testing.T) {
	for _, status := range []project.Status{project.StatusIdle, project.StatusMerged} {
		m := openCommandPicker(t, mergedModel(t, status))
		m.width, m.height = 120, 40
		lines := strings.Split(ansi.Strip(m.renderCommandPickerModal()), "\n")
		for _, c := range projectCommands {
			desc := c.desc
			if status == project.StatusMerged && c.key == "cleanup" {
				desc = "merged — ready to :cleanup/archive"
			}
			var rows []string
			for _, l := range lines {
				// Rows are "│  " + marker + padded label + " — " + desc.
				f := strings.Fields(strings.Trim(l, "│ ▸"))
				if len(f) > 1 && f[0] == c.label && f[1] == "—" {
					rows = append(rows, l)
				}
			}
			if len(rows) != 1 {
				t.Errorf("%s: %q appears on %d rows, want 1", status, c.label, len(rows))
				continue
			}
			if !strings.Contains(rows[0], desc) {
				t.Errorf("%s: %q row %q does not hold full desc %q", status, c.label, rows[0], desc)
			}
		}
	}
}
