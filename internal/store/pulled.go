package store

import (
	"os"
	"path/filepath"
	"time"
)

// The pulled marker is when the daemon last finished a pass against the hub
// for this volume. An agent turn hook beside a running daemon reads the files
// on disk instead of pulling itself, and only while this is recent: a daemon
// that is offline or reconnecting holds its lock but stops refreshing it.
// Free functions on the volume dir, like the paused marker.

func pulledPath(dir string) string { return filepath.Join(dir, "pulled") }

// MarkPulled records that a pass against the hub just completed.
func MarkPulled(dir string) error {
	now := time.Now()
	if err := os.Chtimes(pulledPath(dir), now, now); err == nil {
		return nil
	}
	if err := os.WriteFile(pulledPath(dir), nil, 0o600); err != nil {
		return err
	}
	return os.Chtimes(pulledPath(dir), now, now)
}

// PulledAt reports when the last pass against the hub completed, zero if none
// is recorded.
func PulledAt(dir string) time.Time {
	fi, err := os.Stat(pulledPath(dir))
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}
