//go:build !windows

package config

import (
	"os"
	"syscall"
)

// dirID returns the filesystem's own identity for a directory (device + inode).
// A rename preserves it; a copy never reproduces it. That is the one fact that
// separates a mount whose folder MOVED from a second folder holding a
// byte-identical .bdrive/config.json — the two are otherwise indistinguishable
// on disk, and getting them confused costs either a stolen registry row or a
// stranded project. A zero pair means "unknown", and every caller must then
// fall back to the conservative answer.
func dirID(path string) (uint64, uint64) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Ino == 0 {
		return 0, 0
	}
	return uint64(st.Dev), uint64(st.Ino)
}
