//go:build darwin

package tui

import (
	"os"
	"syscall"
	"time"
)

// birthTime returns the filesystem birth ("creation") time recorded for
// info, when the underlying OS exposes it. On Darwin/APFS and HFS+ this
// comes straight off Stat_t.Birthtimespec — see filetime_other.go for the
// portable fallback used on platforms (Linux included) where stat(2) has no
// birth-time field.
func birthTime(info os.FileInfo) time.Time {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info.ModTime()
	}
	return time.Unix(st.Birthtimespec.Sec, st.Birthtimespec.Nsec)
}
