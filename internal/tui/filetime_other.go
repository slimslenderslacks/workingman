//go:build !darwin

package tui

import (
	"os"
	"time"
)

// birthTime falls back to ModTime on platforms where Go can't portably read
// the filesystem's true creation time (Linux's stat(2) has no birth-time
// field; only statx(2) does, and not every filesystem populates it).
func birthTime(info os.FileInfo) time.Time {
	return info.ModTime()
}
